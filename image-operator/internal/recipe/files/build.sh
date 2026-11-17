#!/bin/bash
# Builds the generic CAPI-ready Elemental 3 node image for one release manifest and target.
# Interface, outputs and exit codes: README.md (top section). No secret is read or written.
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=defaults.env
. "$HERE/defaults.env"

usage() { sed -n '/^## Interface/,/^The rest of this file/p' "$HERE/README.md" >&2; exit 2; }
die() { echo "build.sh: $*" >&2; exit 1; }

MANIFEST="" TARGET="" ARCH="$(uname -m)" OUT="$PWD/out" NAME="" ELEMENTAL_IMAGE="" DISK_SIZE=20G
CACHE="$HOME/.cache/e3p-generic-image" KEEP=0 DEVIATIONS="$HERE/$DEVIATIONS_FILE" CHART_VALUES=()
while [ $# -gt 0 ]; do
  case "$1" in
    --manifest) MANIFEST="${2#oci://}"; shift 2 ;;
    --target) TARGET="$2"; shift 2 ;;
    --arch) ARCH="$2"; shift 2 ;;
    --output-dir) OUT="$2"; shift 2 ;;
    --name) NAME="$2"; shift 2 ;;
    --elemental-image) ELEMENTAL_IMAGE="$2"; shift 2 ;;
    --elemental-repo) ELEMENTAL_REPO="$2"; shift 2 ;;
    --elemental-ref) ELEMENTAL_REF="$2"; shift 2 ;;
    --deviations) DEVIATIONS="$2"; shift 2 ;;
    --chart-values) CHART_VALUES+=("$2"); shift 2 ;;
    --disk-size) DISK_SIZE="$2"; shift 2 ;;
    --cache-dir) CACHE="$2"; shift 2 ;;
    --keep-work) KEEP=1; shift ;;
    *) usage ;;
  esac
done
[ -n "$MANIFEST" ] && [ -n "$TARGET" ] || usage
PODMAN="${PODMAN:-podman}"

# Per target: output format and the files under targets/<target>/. Per architecture: OCI name and console.
case "$TARGET" in
  harvester) FORMAT=qcow2 ;;
  *) echo "build.sh: unknown target '$TARGET' (known: harvester)" >&2; exit 2 ;;
esac
case "$ARCH" in
  x86_64) OCIARCH=amd64; CONSOLE="console=ttyS0 console=tty0" ;;
  aarch64) OCIARCH=arm64; CONSOLE="console=ttyAMA0 console=tty0" ;;   # untested
  *) echo "build.sh: unknown architecture '$ARCH'" >&2; exit 2 ;;
esac
TAG="${MANIFEST##*[:@]}"
NAME="${NAME:-e3-generic-$TARGET-$ARCH-${TAG//[^A-Za-z0-9._-]/_}}"
mkdir -p "$OUT" "$CACHE"
OUT="$(cd "$OUT" && pwd)"; CACHE="$(cd "$CACHE" && pwd)"
WORK="$(mktemp -d "$OUT/.work-$NAME.XXXXXX")"
cleanup() { [ "$KEEP" = 1 ] || rm -rf -- "$WORK"; }
trap cleanup EXIT
LOG="$OUT/$NAME.build.log"; : > "$LOG"
log() { echo "$(date -u +%FT%TZ) $*" | tee -a "$LOG" >&2; }

# 1. Tooling: an elemental3 container image, given or built from source.
EL_REPO="" EL_REF="" EL_COMMIT=""
if [ -z "$ELEMENTAL_IMAGE" ]; then
  EL_REPO="$ELEMENTAL_REPO"; EL_REF="$ELEMENTAL_REF"
  log "cloning $EL_REPO at $EL_REF"
  git clone -q --branch "$EL_REF" "$EL_REPO" "$WORK/elemental" 2>>"$LOG" ||
    { git clone -q "$EL_REPO" "$WORK/elemental" && git -C "$WORK/elemental" checkout -q "$EL_REF"; } >>"$LOG" 2>&1 ||
    die "cannot fetch $EL_REPO at $EL_REF"
  # The version elemental3 reports comes from "git describe", and customize refuses a release whose
  # Elemental minor version is newer than its own. A fork without tags would build as "v3.0".
  [ -z "${ELEMENTAL_TAGS_REPO:-}" ] || git -C "$WORK/elemental" fetch -q --tags "$ELEMENTAL_TAGS_REPO" >>"$LOG" 2>&1 ||
    die "cannot fetch tags from $ELEMENTAL_TAGS_REPO"
  EL_COMMIT="$(git -C "$WORK/elemental" rev-parse HEAD)"
  ELEMENTAL_IMAGE="localhost/e3p-generic-image/elemental3:${EL_COMMIT:0:12}"
  if ! $PODMAN image exists "$ELEMENTAL_IMAGE"; then
    log "building $ELEMENTAL_IMAGE (about 8 minutes cold)"
    $PODMAN build --platform "linux/$OCIARCH" --target runner-elemental3 -t "$ELEMENTAL_IMAGE" "$WORK/elemental" >>"$LOG" 2>&1 ||
      die "tool build failed, see $LOG"
  fi
fi
EL_VERSION="$($PODMAN run --rm "$ELEMENTAL_IMAGE" version 2>/dev/null | tr '\n' ' ' | sed 's/ *$//')"
EL_DIGEST="$($PODMAN image inspect --format '{{.Digest}}' "$ELEMENTAL_IMAGE")"
[ -n "$EL_COMMIT" ] || EL_COMMIT="$(sed -n 's/.*\([0-9a-f]\{40\}\).*/\1/p' <<<"$EL_VERSION" | head -1)"
log "tooling: $ELEMENTAL_IMAGE ($EL_VERSION)"

# 2. Release: digest and content of the manifest, read from the registry before the build.
ociref() { python3 -I "$HERE/lib/ociref.py" "$@"; }
MAN_DIGEST="$(ociref digest "$MANIFEST")" || die "cannot resolve $MANIFEST"
mkdir "$WORK/manifest"
$PODMAN run --rm --security-opt label=disable -v "$WORK/manifest:/out" --entrypoint elemental3ctl "$ELEMENTAL_IMAGE" \
  unpack-image --image "$MANIFEST" --platform "linux/$OCIARCH" --target /out >>"$LOG" 2>&1 || die "cannot read the manifest"
cp "$WORK/manifest/release_manifest.yaml" "$WORK/release_manifest.yaml"
cp "$WORK/release_manifest.yaml" "$OUT/$NAME.release_manifest.yaml"
m() { yq -r "$1 // \"\"" "$WORK/release_manifest.yaml"; }
REL_NAME="$(m .metadata.name)"; REL_VERSION="$(m .metadata.version)"; REL_DATE="$(m .metadata.creationDate)"
OS_IMAGE="$(m .components.operatingSystem.image.base)"; K8S_VERSION="$(m .components.kubernetes.version)"
[ -n "$OS_IMAGE" ] || die "manifest names no OS image (.components.operatingSystem.image.base)"
OS_DIGEST="$(ociref digest "$OS_IMAGE")" || die "cannot resolve $OS_IMAGE"
log "release: $MANIFEST@$MAN_DIGEST ($REL_NAME $REL_VERSION), OS $OS_IMAGE@$OS_DIGEST, Kubernetes $K8S_VERSION"

# 3. Configuration directory: the target's files with the placeholders filled in. Nothing per node.
CONF="$WORK/config"; cp -a "$HERE/targets/$TARGET" "$CONF"
sed -i -e "s|@MANIFEST@|$MANIFEST|" -e "s|@DISK_SIZE@|$DISK_SIZE|" -e "s|@CONSOLE@|$CONSOLE|" "$CONF"/*.yaml
# Optional Helm values for charts of the release (--chart-values <chart>=<file>), each recorded as a deviation.
EXTRA_DEV="$WORK/extra-deviations.txt"; : > "$EXTRA_DEV"
for cv in "${CHART_VALUES[@]}"; do
  chart="${cv%%=*}"; file="${cv#*=}"; [ -f "$file" ] || die "--chart-values: no such file $file"
  mkdir -p "$CONF/kubernetes/helm/values"; cp "$file" "$CONF/kubernetes/helm/values/$chart.yaml"
  CHART="$chart" yq -i '(.components.helm[] | select(.chart == strenv(CHART))).valuesFile = strenv(CHART) + ".yaml"' "$CONF/release.yaml"
  grep -q "valuesFile: $chart.yaml" "$CONF/release.yaml" || die "--chart-values: chart $chart is not listed in targets/$TARGET/release.yaml"
  printf '%s\t%s\n' "$chart" "$(grep -v '^ *#' "$file" | tr -s '\n ' ' ')" >> "$EXTRA_DEV"
done

# 4. Build. --privileged: customize partitions and formats the image through loop devices.
log "customize --type $FORMAT --mode split"
START=$(date +%s)
$PODMAN run --rm --privileged --network host --security-opt label=disable \
  -v "$CONF:/config" -v "$CACHE:/cache" "$ELEMENTAL_IMAGE" --debug customize \
  --type "$FORMAT" --mode split --platform "linux/$OCIARCH" --cache-dir /cache \
  --config-dir /config --output "/config/$NAME.$FORMAT" >>"$LOG" 2>&1 || die "customize failed, see $LOG"
BUILD_SECONDS=$(( $(date +%s) - START ))
[ "$(ociref digest "$MANIFEST")" = "$MAN_DIGEST" ] || die "$MANIFEST moved during the build"
IMG="$OUT/$NAME.$FORMAT"; mv "$CONF/$NAME.$FORMAT" "$IMG"
# What split mode kept out of the image; listed by the inspection, never published.
SPLIT="$CONF/$NAME-config"; [ -d "$SPLIT" ] || SPLIT=""

# 5. Inspect the image itself, then describe it.
set +e
"$HERE/inspect.sh" "$IMG" "$ELEMENTAL_IMAGE" "$OUT/$NAME.inspect.txt" "$WORK/inspection.json" "$SPLIT"
INSPECT_RC=$?
set -e
[ -s "$WORK/inspection.json" ] || die "inspection did not run, see $OUT/$NAME.inspect.txt"
( cd "$OUT" && sha256sum "$NAME.$FORMAT" > "$NAME.$FORMAT.sha256" )
python3 -I - "$OUT/$NAME.metadata.json" <<EOF
import json, os, subprocess, sys
img = "$IMG"
info = json.loads(subprocess.run(["qemu-img", "info", "--output=json", img], check=True, capture_output=True).stdout)
dev = json.load(open("$DEVIATIONS")) if os.path.exists("$DEVIATIONS") else []
for line in open("$EXTRA_DEV"):
    chart, values = line.rstrip("\n").split("\t", 1)
    dev.append({"id": "chart-values-" + chart, "summary": "Helm values given at build time for the release chart "
                + chart + ": " + values.strip(), "removedBy": "a release whose chart needs no override"})
json.dump({
    "schemaVersion": 1, "name": "$NAME", "target": "$TARGET", "arch": "$ARCH", "format": "$FORMAT",
    "file": os.path.basename(img), "sha256": open(img + ".sha256").read().split()[0],
    "sizeBytes": os.path.getsize(img), "virtualSizeBytes": info["virtual-size"],
    "buildDate": "$(date -u +%FT%TZ)", "buildSeconds": $BUILD_SECONDS, "mode": "split",
    "release": {"manifestRef": "$MANIFEST", "manifestDigest": "$MAN_DIGEST", "name": "$REL_NAME", "version": "$REL_VERSION", "creationDate": "$REL_DATE"},
    "os": {"image": "$OS_IMAGE", "digest": "$OS_DIGEST"},
    "kubernetes": {"distribution": "rke2", "version": "$K8S_VERSION"},
    "elemental": {"repo": "$EL_REPO", "ref": "$EL_REF", "commit": "$EL_COMMIT", "image": "$ELEMENTAL_IMAGE",
                  "imageDigest": "$EL_DIGEST", "version": "$EL_VERSION"},
    "userData": {"format": "cloud-config", "datasources": ["NoCloud", "ConfigDrive"], "ignitionPlatform": "metal"},
    "inspection": json.load(open("$WORK/inspection.json")),
    "deviations": dev,
}, open(sys.argv[1], "w"), indent=2)
EOF
if [ "$INSPECT_RC" != 0 ]; then
  for f in "$IMG" "$IMG.sha256"; do mv "$f" "$f.REJECTED"; done
  log "REJECTED: the inspection found content that must not be in a generic image, see $OUT/$NAME.inspect.txt"
  exit 3
fi
log "built $IMG in ${BUILD_SECONDS}s, sha256 $(cut -d' ' -f1 "$IMG.sha256")"
