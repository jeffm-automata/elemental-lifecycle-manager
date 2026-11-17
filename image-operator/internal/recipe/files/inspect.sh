#!/bin/bash
# Takes a built node image apart without mounting it (unprivileged container) and checks that it
# holds nothing that belongs to one node or one cluster. Called by build.sh; can be run on its own.
# Usage: inspect.sh <image.qcow2> <elemental3-container-image> <report.txt> <result.json> [<split-config-dir>]
#        KEEP_EXTRACT=<dir> keeps the extracted files there (default: removed with the temporary directory).
# Exit: 0 all checks passed, 1 a check failed, 2 the image could not be taken apart.
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
IMG="$(realpath "${1:?image}")"; TOOL="${2:?elemental3 container image}"; REPORT="${3:?report}"; RESULT="${4:?result}"; SPLIT="${5:-}"
PODMAN="${PODMAN:-podman}"
INSPECT_IMAGE="localhost/e3p-generic-image/inspect:$($PODMAN image inspect --format '{{.Id}}' "$TOOL" | cut -c1-12)"
$PODMAN image exists "$INSPECT_IMAGE" ||
  $PODMAN build -q --build-arg "ELEMENTAL_IMAGE=$TOOL" -t "$INSPECT_IMAGE" -f "$HERE/lib/Containerfile.inspect" "$HERE/lib" >/dev/null ||
  { echo "inspect.sh: cannot build the inspection image" >&2; exit 2; }
if [ -n "${KEEP_EXTRACT:-}" ]; then X="$KEEP_EXTRACT"; mkdir -p "$X"; else X="$(mktemp -d "$(dirname "$REPORT")/.extract.XXXXXX")"; trap 'rm -rf -- "$X"' EXIT; fi
$PODMAN run --rm --security-opt label=disable -v "$IMG:/in/image:ro" -v "$HERE/lib:/e3p-lib:ro" -v "$X:/out" \
  "$INSPECT_IMAGE" /e3p-lib/extract-in-container.sh || { echo "inspect.sh: extraction failed" >&2; exit 2; }
python3 -I "$HERE/lib/inspect.py" "$X" "$REPORT" "$RESULT" "$SPLIT"
