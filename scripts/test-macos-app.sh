#!/bin/sh
set -eu

REPO_ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
APP_PATH=${1:-"$REPO_ROOT/dist/Experiment Control Plane.app"}
CONTENTS="$APP_PATH/Contents"

test -x "$CONTENTS/MacOS/ExperimentControlPlaneLauncher"
test -x "$CONTENTS/MacOS/controlplane"
test -x "$CONTENTS/MacOS/worker"
test -f "$CONTENTS/Resources/AppIcon.icns"
plutil -lint "$CONTENTS/Info.plist" >/dev/null

test "$(plutil -extract CFBundleExecutable raw "$CONTENTS/Info.plist")" = "ExperimentControlPlaneLauncher"
test "$(plutil -extract CFBundleIdentifier raw "$CONTENTS/Info.plist")" = "dev.chilltongx.experiment-control-plane"
test "$(plutil -extract NSAppTransportSecurity.NSAllowsLocalNetworking raw "$CONTENTS/Info.plist")" = "true"

file "$CONTENTS/MacOS/ExperimentControlPlaneLauncher" | grep -q 'Mach-O'
file "$CONTENTS/MacOS/controlplane" | grep -q 'Mach-O'
file "$CONTENTS/MacOS/worker" | grep -q 'Mach-O'
"$CONTENTS/MacOS/controlplane" -version >/dev/null

# The helper lookup used by Bundle.url(forAuxiliaryExecutable:) must resolve
# both bundled Go executables from Contents/MacOS.
LOOKUP_SWIFT=$(mktemp "${TMPDIR:-/tmp}/experiment-control-plane-bundle-check.XXXXXX.swift")
LOOKUP_BINARY=${LOOKUP_SWIFT%.swift}
trap 'rm -f "$LOOKUP_SWIFT" "$LOOKUP_BINARY"' EXIT INT TERM
printf '%s\n' \
  'import Foundation' \
  'guard CommandLine.arguments.count == 2,' \
  '      let bundle = Bundle(path: CommandLine.arguments[1]),' \
  '      bundle.url(forAuxiliaryExecutable: "controlplane") != nil,' \
  '      bundle.url(forAuxiliaryExecutable: "worker") != nil else { exit(1) }' \
  > "$LOOKUP_SWIFT"
xcrun swiftc "$LOOKUP_SWIFT" -o "$LOOKUP_BINARY"
"$LOOKUP_BINARY" "$APP_PATH"

if command -v codesign >/dev/null 2>&1; then
  codesign --verify --deep --strict "$APP_PATH"
fi

echo "macOS app bundle validation passed: $APP_PATH"
