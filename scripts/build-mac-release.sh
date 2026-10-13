#!/usr/bin/env bash
# Builds a release of Unscan for Apple silicon: the app and its disk image
# in build/release/darwin-arm64/, after the tests pass, and with the update
# key, the update that installed apps download, signed.
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
#   MYGO_UPDATER_PRIVATE_KEY  the key of "go tool mygo keygen", which signs the
#                           update: unscan-<version>-darwin-arm64.tar.gz, deltas
#                           from the versions before it, and update-darwin-arm64.json
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

if [[ -n "$notary" ]]; then
	# mygo build notarizes the disk image and staples the app before it
	# archives the app for updates, which install the app as archived:
	# notarized after the build, the update would lack the ticket. It reads
	# the profile from mygo.json, which has it for the build only.
	backup=$(mktemp)
	cp mygo.json "$backup"
	trap 'cp "$backup" mygo.json; rm -f "$backup"' EXIT
	plutil -extract macos raw -o - mygo.json >/dev/null 2>&1 || plutil -insert macos -dictionary mygo.json
	plutil -replace macos.notarize -dictionary mygo.json
	plutil -replace macos.notarize.keychainProfile -string "$notary" mygo.json
fi

echo "==> Building $name $version (release)"
rm -rf "$dir"
go tool mygo build -platform darwin/arm64 -o "$out" -sign="$identity"
if [[ -n "$notary" ]]; then
	cp "$backup" mygo.json
fi
if [[ ! -d "$app" || ! -f "$dmg" ]]; then
	echo "error: mygo build made no $app or $dmg" >&2
	exit 1
fi

echo "==> Verifying the signature"
codesign --verify --deep --strict "$app"

if [[ -n "$notary" ]]; then
	echo "==> Verifying the notarization"
	xcrun stapler validate "$dmg"
	xcrun stapler validate "$app"
	spctl --assess --type open --context context:primary-signature --verbose "$dmg"
	spctl --assess --type execute --verbose "$app"
fi

# The update: the archive the manifest names, and the manifest.
manifest="$dir/update-darwin-arm64.json"
archive=""
if [[ -n "${MYGO_UPDATER_PRIVATE_KEY:-}" ]]; then
	if [[ ! -f "$manifest" ]]; then
		echo "error: mygo build made no $manifest" >&2
		exit 1
	fi
	archive=$(plutil -extract url raw -o - "$manifest")
	archive="$dir/${archive##*/}"
	if [[ ! -f "$archive" ]]; then
		echo "error: mygo build made no $archive" >&2
		exit 1
	fi
fi

echo "==> Built $name $version"
echo "    $app ($(du -sh "$app" | awk '{print $1}'))"
echo "    $dmg ($(du -sh "$dmg" | awk '{print $1}'))"
if [[ -n "$archive" ]]; then
	for f in "$archive" "$dir"/*.delta "$manifest"; do
		if [[ -f "$f" ]]; then
			echo "    $f ($(du -sh "$f" | awk '{print $1}'))"
		fi
	done
fi
if [[ "$identity" == - ]]; then
	echo "warning: signed ad hoc, so Gatekeeper blocks it on other Macs; set MACOS_SIGNING_IDENTITY to ship it" >&2
elif [[ -z "$notary" ]]; then
	echo "warning: not notarized, so Gatekeeper warns on other Macs; set NOTARY_PROFILE to notarize it" >&2
fi
if [[ -z "$archive" ]]; then
	echo "warning: no update for installed apps; set MYGO_UPDATER_PRIVATE_KEY to sign one" >&2
fi
