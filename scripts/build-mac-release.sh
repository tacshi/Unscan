#!/usr/bin/env bash
# Builds a release of Unscan for Apple silicon: the app and its disk image
# in build/release/darwin-arm64/, after the tests pass.
#
#   scripts/build-mac-release.sh
#
# Environment:
#   MACOS_SIGNING_IDENTITY  signs with this identity, with the hardened runtime,
#                           e.g. "Developer ID Application: Jane Doe (TEAMID)";
#                           without it the app is signed ad hoc and runs on this
#                           Mac only ("security find-identity -v -p codesigning")
#   NOTARY_PROFILE          notarizes the disk image with this keychain profile,
#                           made once by "xcrun notarytool store-credentials",
#                           and staples the ticket to the image and the app
#   SKIP_TESTS=1            builds without running the tests first
#
# Intel Macs are left out: ONNX Runtime ships no build for them.
set -euo pipefail

case "${1:-}" in
"") ;;
-h | --help)
	awk 'NR > 1 && /^#/ { sub(/^# ?/, ""); print; next } NR > 1 { exit }' "$0"
	exit 0
	;;
*)
	echo "unknown argument: $1 (see --help)" >&2
	exit 2
	;;
esac

if [[ "$(uname -s)" != Darwin ]]; then
	echo "error: macOS releases need macOS, which signs and notarizes apps" >&2
	exit 1
fi
cd "$(dirname "$0")/.."

name=$(plutil -extract name raw -o - mygo.json)
version=$(plutil -extract version raw -o - mygo.json)
identity=${MACOS_SIGNING_IDENTITY:--}
notary=${NOTARY_PROFILE:-}
out=build/release
dir="$out/darwin-arm64"
app="$dir/$name.app"
dmg="$dir/$name $version arm64.dmg"

if [[ -n "$notary" && "$identity" == - ]]; then
	echo "error: notarizing needs a Developer ID: set MACOS_SIGNING_IDENTITY" >&2
	exit 1
fi

echo "==> Checking the models and ONNX Runtime"
go run ./cmd/fetchdeps -platforms darwin-arm64

if [[ "${SKIP_TESTS:-}" != 1 ]]; then
	echo "==> Testing"
	CGO_ENABLED=0 go test ./...
fi

echo "==> Building $name $version (release)"
rm -rf "$dir"
go tool mygo build -platform darwin/arm64 -o "$out" -sign="$identity"
if [[ ! -d "$app" || ! -f "$dmg" ]]; then
	echo "error: mygo build made no $app or $dmg" >&2
	exit 1
fi

echo "==> Verifying the signature"
codesign --verify --deep --strict "$app"

if [[ -n "$notary" ]]; then
	echo "==> Notarizing $(basename "$dmg") (usually a few minutes)"
	result=$(mktemp)
	trap 'rm -f "$result"' EXIT
	# notarytool exits 0 for rejected submissions too: read the verdict.
	xcrun notarytool submit "$dmg" --keychain-profile "$notary" --wait --output-format json >"$result"
	status=$(plutil -extract status raw -o - "$result" 2>/dev/null || echo unknown)
	if [[ "$status" != Accepted ]]; then
		id=$(plutil -extract id raw -o - "$result" 2>/dev/null || true)
		echo "error: notarization status: $status" >&2
		if [[ -n "$id" ]]; then
			echo "see: xcrun notarytool log $id --keychain-profile \"$notary\"" >&2
		fi
		exit 1
	fi
	xcrun stapler staple "$dmg"
	xcrun stapler staple "$app"
	spctl --assess --type open --context context:primary-signature --verbose "$dmg"
	spctl --assess --type execute --verbose "$app"
fi

echo "==> Built $name $version"
echo "    $app ($(du -sh "$app" | awk '{print $1}'))"
echo "    $dmg ($(du -sh "$dmg" | awk '{print $1}'))"
if [[ "$identity" == - ]]; then
	echo "warning: signed ad hoc, so Gatekeeper blocks it on other Macs; set MACOS_SIGNING_IDENTITY to ship it" >&2
elif [[ -z "$notary" ]]; then
	echo "warning: not notarized, so Gatekeeper warns on other Macs; set NOTARY_PROFILE to notarize it" >&2
fi
