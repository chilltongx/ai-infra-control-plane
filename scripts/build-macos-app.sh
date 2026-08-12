#!/bin/sh
set -eu

REPO_ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
OUTPUT_DIR=${OUTPUT_DIR:-"$REPO_ROOT/dist"}
APP_NAME="Experiment Control Plane"
APP_PATH="$OUTPUT_DIR/$APP_NAME.app"
CONTENTS="$APP_PATH/Contents"
MACOS_DIR="$CONTENTS/MacOS"
RESOURCES_DIR="$CONTENTS/Resources"
ARCH=${MACOS_ARCH:-$(uname -m)}
MIN_MACOS=${MACOSX_DEPLOYMENT_TARGET:-13.0}

case "$ARCH" in
  arm64|x86_64) ;;
  *) echo "Unsupported MACOS_ARCH: $ARCH" >&2; exit 2 ;;
esac

command -v go >/dev/null 2>&1 || { echo "Go is required." >&2; exit 2; }
command -v xcrun >/dev/null 2>&1 || { echo "Xcode Command Line Tools are required." >&2; exit 2; }
xcrun --find swiftc >/dev/null 2>&1 || { echo "swiftc is required." >&2; exit 2; }
command -v iconutil >/dev/null 2>&1 || { echo "iconutil is required." >&2; exit 2; }
command -v sips >/dev/null 2>&1 || { echo "sips is required." >&2; exit 2; }

BUILD_ROOT=$(mktemp -d "${TMPDIR:-/tmp}/experiment-control-plane-build.XXXXXX")
LOCK_DIR="$OUTPUT_DIR/.macos-app-build.lock"
mkdir -p "$OUTPUT_DIR"
if ! mkdir "$LOCK_DIR" 2>/dev/null; then
  echo "Another macOS app build is already using $OUTPUT_DIR" >&2
  exit 3
fi
trap 'rm -rf "$BUILD_ROOT" "$LOCK_DIR"' EXIT INT TERM
STAGING_APP="$BUILD_ROOT/$APP_NAME.app"
STAGING_CONTENTS="$STAGING_APP/Contents"
STAGING_MACOS="$STAGING_CONTENTS/MacOS"
STAGING_RESOURCES="$STAGING_CONTENTS/Resources"
mkdir -p "$STAGING_MACOS" "$STAGING_RESOURCES"

echo "Building Go binaries for darwin/${ARCH}…"
(
  cd "$REPO_ROOT"
  CGO_ENABLED=0 GOOS=darwin GOARCH="$ARCH" go build -trimpath -ldflags="-s -w" -o "$STAGING_MACOS/controlplane" ./cmd/controlplane
  CGO_ENABLED=0 GOOS=darwin GOARCH="$ARCH" go build -trimpath -ldflags="-s -w" -o "$STAGING_MACOS/worker" ./cmd/worker
)

echo "Building native launcher…"
xcrun swiftc \
  -parse-as-library \
  -swift-version 6 \
  -warnings-as-errors \
  -O -whole-module-optimization \
  -target "$ARCH-apple-macosx$MIN_MACOS" \
  -framework AppKit -framework Security -framework WebKit \
  -o "$STAGING_MACOS/ExperimentControlPlaneLauncher" \
  "$REPO_ROOT/scripts/macos/ExperimentControlPlaneLauncher.swift"

cp "$REPO_ROOT/scripts/macos/Info.plist" "$STAGING_CONTENTS/Info.plist"
"$REPO_ROOT/scripts/macos/generate-icon.sh" "$STAGING_RESOURCES/AppIcon.icns"

chmod 755 "$STAGING_MACOS/ExperimentControlPlaneLauncher" "$STAGING_MACOS/controlplane" "$STAGING_MACOS/worker"
plutil -lint "$STAGING_CONTENTS/Info.plist" >/dev/null

if command -v codesign >/dev/null 2>&1; then
  echo "Applying an ad-hoc signature…"
  codesign --force --deep --sign - "$STAGING_APP"
  codesign --verify --deep --strict "$STAGING_APP"
fi

OLD_APP="$BUILD_ROOT/previous.app"
if [ -e "$APP_PATH" ]; then
  mv "$APP_PATH" "$OLD_APP"
fi
if ! mv "$STAGING_APP" "$APP_PATH"; then
  if [ -e "$OLD_APP" ]; then mv "$OLD_APP" "$APP_PATH"; fi
  exit 1
fi
rm -rf "$OLD_APP"

echo "Built: $APP_PATH"
echo "Run:   open \"$APP_PATH\""
