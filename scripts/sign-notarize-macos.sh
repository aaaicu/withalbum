#!/bin/bash
# Signs "WithAlbum Server.app" with a Developer ID Application identity,
# notarizes it with Apple and builds the distributable DMG. Run by the
# maintainer on their own Mac; nothing secret is stored in the repository.
#
#   WITHALBUM_SIGN_IDENTITY="Developer ID Application: <name> (<team>)" \
#   WITHALBUM_NOTARY_PROFILE=<keychain profile> \
#     bash scripts/sign-notarize-macos.sh "dist/macos/WithAlbum Server.app" [--version X.Y.Z-suffix]
#
# The notary credentials live only in the maintainer's keychain, stored once
# by hand with:  xcrun notarytool store-credentials <profile>
# (it asks for the app-specific password or API key interactively). This
# script never takes a password, key or team ID as an argument.
#
# Steps: sign the bundled server and the app (hardened runtime, secure
# timestamp, macos/Support/WithAlbumServer.entitlements = no exceptions),
# notarize a zip of the app and staple it, build the DMG from the stapled
# app, sign + notarize + staple the DMG, then check with spctl.
set -euo pipefail
if [[ "$(uname -s)" != Darwin ]]; then echo 'macOS only' >&2; exit 1; fi
REPO="$(cd "$(dirname "$0")/.." && pwd)"
usage() { echo 'Usage: WITHALBUM_SIGN_IDENTITY=... WITHALBUM_NOTARY_PROFILE=... bash sign-notarize-macos.sh APP [--version V]' >&2; exit 2; }
[[ $# -ge 1 ]] || usage
APP="$1"; shift
VERSION=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --version) VERSION="${2:?}"; shift 2 ;;
    *) usage ;;
  esac
done
IDENTITY="${WITHALBUM_SIGN_IDENTITY:-}"
PROFILE="${WITHALBUM_NOTARY_PROFILE:-}"
[[ -n "$IDENTITY" ]] || { echo 'Set WITHALBUM_SIGN_IDENTITY to your "Developer ID Application: …" identity (security find-identity -v -p codesigning)' >&2; exit 2; }
[[ -n "$PROFILE" ]] || { echo 'Set WITHALBUM_NOTARY_PROFILE to the keychain profile created with: xcrun notarytool store-credentials <profile>' >&2; exit 2; }
[[ "$IDENTITY" == "Developer ID Application:"* ]] || { echo 'WITHALBUM_SIGN_IDENTITY must be a "Developer ID Application" identity' >&2; exit 2; }
[[ -d "$APP/Contents/Helpers" && -x "$APP/Contents/Helpers/withalbum-server" ]] || { echo "Not a WithAlbum Server.app: $APP" >&2; exit 2; }
if [[ -z "$VERSION" ]]; then
  VERSION="$(plutil -extract WithAlbumServerVersion raw -o - "$APP/Contents/Info.plist")"
fi
[[ "$VERSION" =~ ^[0-9A-Za-z.+-]+$ ]] || { echo "Bad version: $VERSION" >&2; exit 2; }
ENTITLEMENTS="$REPO/macos/Support/WithAlbumServer.entitlements"
OUT="$(cd "$(dirname "$APP")" && pwd)"
DMG="$OUT/WithAlbumServer_${VERSION}.dmg"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

sign() { codesign --force --timestamp --options runtime --entitlements "$ENTITLEMENTS" --sign "$IDENTITY" "$@"; }

echo "==> sign"
sign "$APP/Contents/Helpers/withalbum-server"
sign "$APP"
codesign --verify --strict --deep --verbose=2 "$APP"

echo "==> notarize the app"
ditto -c -k --keepParent "$APP" "$WORK/app.zip"
xcrun notarytool submit "$WORK/app.zip" --keychain-profile "$PROFILE" --wait
xcrun stapler staple "$APP"
xcrun stapler validate "$APP"

echo "==> DMG"
bash "$REPO/scripts/build-macos-app.sh" --dmg-only "$APP" "$DMG" >/dev/null
codesign --force --timestamp --sign "$IDENTITY" "$DMG"
xcrun notarytool submit "$DMG" --keychain-profile "$PROFILE" --wait
xcrun stapler staple "$DMG"
xcrun stapler validate "$DMG"

echo "==> Gatekeeper"
spctl --assess --type execute --verbose=2 "$APP"
spctl --assess --type open --context context:primary-signature --verbose=2 "$DMG"
(cd "$OUT" && shasum -a 256 "$(basename "$DMG")" > "$(basename "$DMG").sha256")
echo "Signed and notarized: $DMG"
echo "Check it on another Mac (downloaded, so it is quarantined): docs/RELEASING.md"
