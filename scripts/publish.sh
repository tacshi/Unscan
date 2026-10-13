#!/usr/bin/env bash
# Publishes a release of Unscan to GitHub: the disk image that
# scripts/build-mac-release.sh builds, signed with a Developer ID and
# notarized, on a release under a new tag, v<version>.
#
#   scripts/publish.sh 2026.10.0                 # build, review the notes, publish
#   scripts/publish.sh 2026.10.0 --no-upload     # build into build/publish only
#   scripts/publish.sh 2026.10.0 --allow-branch  # release from a branch, not main
#
# Release notes come from release-notes/Unscan-<version>.md, used verbatim
# when it exists. Otherwise they are drafted from the commits since the last
# v* tag and shown for review: [y]es publishes them, [e]dit opens them in
# $EDITOR and shows them again, anything else stops.
#
# Environment:
#   MACOS_SIGNING_IDENTITY  the Developer ID to sign with (DEVELOPER_ID_APPLICATION
#                           works too); by default the keychain's only
#                           "Developer ID Application" identity
#   NOTARY_PROFILE          the notarytool keychain profile, "Unscan" by default,
#                           made once by "xcrun notarytool store-credentials Unscan"
#
# The committed mygo.json is never edited: the version is the build's for
# the run only, so a release needs no commit of its own.
set -euo pipefail

die() {
	echo "error: $*" >&2
	exit 1
}

version=""
upload=true
allow_branch=false
for arg in "$@"; do
	case "$arg" in
	--no-upload) upload=false ;;
	--allow-branch) allow_branch=true ;;
	-h | --help)
		awk 'NR > 1 && /^#/ { sub(/^# ?/, ""); print; next } NR > 1 { exit }' "$0"
		exit 0
		;;
	-*)
		echo "unknown argument: $arg (see --help)" >&2
		exit 2
		;;
	*)
		[[ -z "$version" ]] || die "one version, please: $version or $arg"
		version=${arg#v}
		;;
	esac
done
if [[ -z "$version" ]]; then
	echo "usage: scripts/publish.sh <version> [--no-upload] [--allow-branch] (see --help)" >&2
	exit 2
fi
[[ "$(uname -s)" == Darwin ]] || die "releases are built on macOS, which signs and notarizes apps"
cd "$(dirname "$0")/.."

for tool in go git xcrun plutil; do
	command -v "$tool" >/dev/null 2>&1 || die "needs $tool"
done
if $upload; then
	command -v gh >/dev/null 2>&1 || die "needs the GitHub CLI (gh) to upload: brew install gh"
fi

name=$(plutil -extract name raw -o - mygo.json)
tag="v$version"
stage=build/publish

# ------------------------------------------------------------------ checks

echo "==> Checking the signing identity and the notary profile"
identity=${MACOS_SIGNING_IDENTITY:-${DEVELOPER_ID_APPLICATION:-}}
if [[ -z "$identity" ]]; then
	ids=$(security find-identity -v -p codesigning | sed -n 's/.*"\(Developer ID Application: [^"]*\)"$/\1/p' | sort -u)
	case $(printf '%s\n' "$ids" | grep -c . || true) in
	0) die "no Developer ID Application identity in the keychain; set MACOS_SIGNING_IDENTITY" ;;
	1) identity=$ids ;;
	*) die "several Developer ID Application identities; set MACOS_SIGNING_IDENTITY to one of:"$'\n'"$ids" ;;
	esac
fi
security find-identity -v -p codesigning | grep -qF "\"$identity\"" ||
	die "no signing identity \"$identity\" in the keychain"
notary=${NOTARY_PROFILE:-Unscan}
xcrun notarytool history --keychain-profile "$notary" >/dev/null 2>&1 ||
	die "the notarytool keychain profile \"$notary\" doesn't work; make it with: xcrun notarytool store-credentials $notary"
echo "    signing as $identity, notarizing with \"$notary\""

branch=$(git branch --show-current)
if [[ "$branch" != main ]] && ! $allow_branch; then
	die "releases are cut from main, and this is ${branch:-a detached HEAD} (--allow-branch to release from it)"
fi
[[ -z "$(git status --porcelain)" ]] || die "the working tree has changes; commit or stash them first"
if git rev-parse -q --verify "refs/tags/$tag" >/dev/null; then
	die "the tag $tag already exists"
fi
repo=""
if $upload; then
	[[ -n "$branch" ]] || die "a detached HEAD can't be checked against origin; check out a branch"
	repo=$(gh repo view --json nameWithOwner --jq .nameWithOwner) || die "gh can't tell this repository's GitHub repository"
	git fetch -q origin "$branch" --tags
	[[ "$(git rev-parse HEAD)" == "$(git rev-parse "origin/$branch")" ]] ||
		die "HEAD is not origin/$branch; push or pull first, so that the tag names a published commit"
	if git rev-parse -q --verify "refs/tags/$tag" >/dev/null; then
		die "the tag $tag already exists on origin"
	fi
	if gh release view "$tag" --repo "$repo" >/dev/null 2>&1; then
		die "$repo already has a release $tag"
	fi
fi

# ------------------------------------------------------------------- notes

# These notes are published, on the release page of a public repository, so
# drafts from commit messages are always reviewed before an upload.
rm -rf "$stage"
mkdir -p "$stage"
notes_src="release-notes/$name-$version.md"
notes="$stage/notes.md"
if [[ -f "$notes_src" ]]; then
	cp "$notes_src" "$notes"
	echo "==> Notes: $notes_src (as written)"
else
	prev=$(git describe --tags --abbrev=0 --match 'v*' 2>/dev/null || true)
	if [[ -n "$prev" ]]; then
		range=("$prev..HEAD")
		echo "==> Notes: drafted from $prev..HEAD"
	else
		# The first release: capped, so that the whole history isn't its notes.
		range=(-n 30 HEAD)
		echo "==> Notes: drafted from the last 30 commits (no v* tag yet)"
	fi
	git log --no-merges --pretty=tformat:'- %s' "${range[@]}" |
		grep -viE '^- (chore|ci|build|test|style|refactor|docs)(\([^)]*\))?!?:' |
		grep -viE '^- (release|bump|merge|wip)([^[:alnum:]]|$)' >"$notes" || true

	if $upload && [[ ! -t 0 ]]; then
		die "drafted notes need a review before they are published. Write them in
  $notes_src
and run again: it is used as written, and may be empty for no summary."
	fi
	if $upload; then
		while :; do
			printf '\n--- release notes for %s %s (%s) ---\n' "$name" "$version" "$notes"
			if grep -qE '[^[:space:]]' "$notes"; then
				cat "$notes"
			else
				echo "(no summary)"
			fi
			printf -- '---\n\nPublish these notes? [y]es / [e]dit / [N]o: '
			read -r reply
			case "$reply" in
			y | Y) break ;;
			e | E) "${VISUAL:-${EDITOR:-vi}}" "$notes" ;;
			*) die "stopped. To keep this draft and edit it outside a release:
  mkdir -p release-notes && cp $notes $notes_src
then edit that file and run again: it is used as written." ;;
			esac
		done
	fi
fi

# ------------------------------------------------------------------- build

# The build reads its version from mygo.json, which has this one for the
# run only, and is put back as committed however the script ends.
backup=$(mktemp)
cp mygo.json "$backup"
trap 'cp "$backup" mygo.json; rm -f "$backup"' EXIT
plutil -replace version -string "$version" mygo.json

echo "==> Building $name $version at $(git rev-parse --short HEAD)"
MACOS_SIGNING_IDENTITY=$identity NOTARY_PROFILE=$notary scripts/build-mac-release.sh

cp "$backup" mygo.json
dmg="build/release/darwin-arm64/$name $version arm64.dmg"
[[ -f "$dmg" ]] || die "the build made no $dmg"
xcrun stapler validate "$dmg" >/dev/null || die "$dmg is not notarized"
asset="$stage/$name-$version-arm64.dmg"
cp "$dmg" "$asset"
echo "==> $asset"
echo "    sha256 $(shasum -a 256 "$asset" | awk '{print $1}')"

if ! $upload; then
	echo "==> Built into $stage; nothing was published"
	exit 0
fi

# ----------------------------------------------------------------- publish

echo "==> Publishing $tag to $repo"
if grep -qE '[^[:space:]]' "$notes"; then
	git tag -a "$tag" -F "$notes"
else
	git tag -a "$tag" -m "$name $version"
fi
git push origin "$tag"
# gh uploads the disk image to a draft, and publishes it once it is there.
gh release create "$tag" "$asset" --repo "$repo" --verify-tag --latest \
	--title "$name $version" --notes-file "$notes" ||
	die "the tag $tag is pushed, but its release isn't published. Finish it with:
  gh release create $tag '$asset' --repo $repo --verify-tag --latest --title '$name $version' --notes-file $notes"
echo "==> Published https://github.com/$repo/releases/tag/$tag"
