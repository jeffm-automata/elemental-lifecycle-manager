#!/bin/bash
# Runs once in the build container of a builder pod. Calls the embedded recipe with the tools of the
# builder image (no container is started) and reports through /work/out/status.json, which the pod's
# second container serves on port 8080 together with the image. Reads no secret and writes none.
set -uo pipefail
# shellcheck disable=SC1091
. /etc/e3-build/env
OUT=/work/out; WORK=/work/build
export OUT NAME KEY EXPECT_DIGEST
STARTED="$(date -u +%FT%TZ)"; export STARTED
umask 022
mkdir -p "$OUT" "$WORK/recipe" "$WORK/out" "$WORK/cache" /work/home /work/mnt

status() { STATE="$1" STEP="$2" EXIT_CODE="${3:-0}" ERROR="${4:-}" python3 -I /etc/e3-build/status.py; }
fail() { status failed "$1" "$2" "$3"; exit 1; }

status building unpack
tar --no-same-owner -xzf /etc/e3-build/recipe.tgz -C "$WORK/recipe" || fail unpack 1 "cannot unpack the recipe"
for tool in elemental3 elemental3ctl python3 qemu-img yq unsquashfs e3-podman; do
  command -v "$tool" >/dev/null 2>&1 || fail tools 1 "the builder image lacks $tool"
done
# The tool commit is part of the cache key: an image with another elemental3 must not answer this request.
VERSION="$(elemental3 version 2>/dev/null | tr '\n' ' ')"
# "elemental3 version" ends in +g<first 7 characters of the commit>.
case "$VERSION" in
  *"+g${ELEMENTAL_REF:0:7}"*) ;;
  *) fail tools 1 "the builder image holds another elemental3 than commit $ELEMENTAL_REF: $VERSION" ;;
esac

CV=()
for f in /etc/e3-build/chart-values-*.yaml; do
  [ -e "$f" ] || continue
  chart="$(basename "$f" .yaml)"; CV+=(--chart-values "${chart#chart-values-}=$f")
done

status building build
export HOME=/work/home PODMAN=/usr/local/bin/e3-podman E3_MNT=/work/mnt
export E3_TOOL_IMAGE="builder-image/elemental3:${ELEMENTAL_REF:0:12}"
cd "$WORK" || fail build 1 "no work directory"
"$WORK/recipe/build.sh" --manifest "$MANIFEST" --target "$TARGET" --arch "$ARCH" --output-dir "$WORK/out" \
  --name "$NAME" --disk-size "$DISK_SIZE" --elemental-image "$E3_TOOL_IMAGE" \
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
# For sizing the pod: what the work volume holds at the end and the container's memory peak (page cache included).
du -sx --block-size=1M /work 2>/dev/null | cut -f1 > "$OUT/work-volume-mib.txt" || true
cat /sys/fs/cgroup/memory.peak > "$OUT/memory-peak-bytes.txt" 2>/dev/null || true
status "done" "done"
