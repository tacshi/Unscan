#!/usr/bin/env bash
# Builds a debug Unscan.app for Apple silicon, quickly: MyGo's development
# features (the inspector of native UI) are kept, the app is signed ad hoc,
# and no disk image is made.
#
#   scripts/build-mac-debug.sh          # build/debug/darwin-arm64/Unscan.app
#   scripts/build-mac-debug.sh --open   # and start it
#
# Intel Macs are left out: ONNX Runtime ships no build for them.
set -euo pipefail

open_app=false
for arg in "$@"; do
	case "$arg" in
	--open) open_app=true ;;
	-h | --help)
		awk 'NR > 1 && /^#/ { sub(/^# ?/, ""); print; next } NR > 1 { exit }' "$0"
		exit 0
		;;
	*)
		echo "unknown argument: $arg (see --help)" >&2
		exit 2
		;;
	esac
done

if [[ "$(uname -s)" != Darwin ]]; then
	echo "error: macOS builds need macOS, which signs apps" >&2
	exit 1
fi
cd "$(dirname "$0")/.."

name=$(plutil -extract name raw -o - mygo.json)
out=build/debug
app="$out/darwin-arm64/$name.app"

echo "==> Checking the models and ONNX Runtime"
go run ./cmd/fetchdeps -platforms darwin-arm64

echo "==> Building $name (debug)"
go tool mygo build -debug -skip-dmg -platform darwin/arm64 -o "$out"

echo "==> Built $app ($(du -sh "$app" | awk '{print $1}'))"
if $open_app; then
	open "$app"
fi
