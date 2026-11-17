#!/bin/bash
# Runs once in the builder VM (installed as /usr/local/bin/e3-run-build), started by cloud-init through e3-build.service.
# Calls the embedded recipe and reports through /var/lib/e3-build/out/status.json, which the VM
# serves on port 8080 together with the image. Reads no secret and writes none.
set -uo pipefail
# shellcheck disable=SC1091
. /etc/e3-build/env
ROOT=/var/lib/e3-build; OUT="$ROOT/out"; WORK="$ROOT/work"
export OUT NAME KEY EXPECT_DIGEST
STARTED="$(date -u +%FT%TZ)"; export STARTED
mkdir -p "$OUT" "$WORK/recipe" "$WORK/out" "$WORK/cache"

status() { STATE="$1" STEP="$2" EXIT_CODE="${3:-0}" ERROR="${4:-}" python3 -I /etc/e3-build/status.py; }
fail() { status failed "$1" "$2" "$3"; exit 1; }

status building unpack
tar -xzf /etc/e3-build/recipe.tgz -C "$WORK/recipe" || fail unpack 1 "cannot unpack the recipe"

# Waits (bounded) for the tools cloud-init installs; the unit can start before the last package.
for _ in $(seq 1 60); do
  command -v podman git python3 qemu-img yq >/dev/null 2>&1 && break
  sleep 5
done
for tool in podman git python3 qemu-img yq; do
  command -v "$tool" >/dev/null 2>&1 || fail tools 1 "the builder image lacks $tool"
done

CV=()
for f in /etc/e3-build/chart-values/*.yaml; do
  [ -e "$f" ] && CV+=(--chart-values "$(basename "$f" .yaml)=$f")
done

status building build
export HOME=/root
cd "$WORK" || fail build 1 "no work directory"
"$WORK/recipe/build.sh" --manifest "$MANIFEST" --target "$TARGET" --arch "$ARCH" --output-dir "$WORK/out" \
  --name "$NAME" --disk-size "$DISK_SIZE" --elemental-repo "$ELEMENTAL_REPO" --elemental-ref "$ELEMENTAL_REF" \
  --cache-dir "$WORK/cache" "${CV[@]}" > "$OUT/runner.log" 2>&1
RC=$?
for ext in build.log inspect.txt metadata.json release_manifest.yaml; do
  [ -f "$WORK/out/$NAME.$ext" ] && cp -f "$WORK/out/$NAME.$ext" "$OUT/"
done
[ "$RC" = 0 ] || fail build "$RC" "the recipe ended with exit status $RC"

status building verify
GOT="$(python3 -I -c 'import json,sys; print(json.load(open(sys.argv[1]))["release"]["manifestDigest"])' "$OUT/$NAME.metadata.json")"
# Exit status 4: the tag pointed at another manifest than the one the request was resolved to.
[ "$GOT" = "$EXPECT_DIGEST" ] || fail verify 4 "the release moved: built $GOT, requested $EXPECT_DIGEST"

status building checksum
IMG="$WORK/out/$NAME.qcow2"
( cd "$WORK/out" && sha512sum "$NAME.qcow2" > "$OUT/$NAME.qcow2.sha512" ) || fail checksum 1 "no image file"
cp -f "$IMG.sha256" "$OUT/"; chmod 0644 "$IMG"; mv "$IMG" "$OUT/"
status "done" "done"
