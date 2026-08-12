#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
OUTPUT_PATH=${1:-"$SCRIPT_DIR/AppIcon.icns"}
WORK_DIR=$(mktemp -d "${TMPDIR:-/tmp}/experiment-control-plane-icon.XXXXXX")
trap 'rm -rf "$WORK_DIR"' EXIT INT TERM

ICONSET="$WORK_DIR/AppIcon.iconset"
mkdir -p "$ICONSET"

for SIZE in 16 32 128 256 512; do
  sips -s format png -z "$SIZE" "$SIZE" "$SCRIPT_DIR/AppIcon.svg" --out "$ICONSET/icon_${SIZE}x${SIZE}.png" >/dev/null
  DOUBLE=$((SIZE * 2))
  sips -s format png -z "$DOUBLE" "$DOUBLE" "$SCRIPT_DIR/AppIcon.svg" --out "$ICONSET/icon_${SIZE}x${SIZE}@2x.png" >/dev/null
done

mkdir -p "$(dirname -- "$OUTPUT_PATH")"
iconutil -c icns "$ICONSET" -o "$OUTPUT_PATH"
echo "Generated $OUTPUT_PATH"
