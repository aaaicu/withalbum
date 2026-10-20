#!/bin/bash
# Builds "WithAlbum Server.app" (menu bar app + bundled universal server)
# and a DMG with an Applications link. Unsigned: the bundle is only
# ad-hoc signed, so Gatekeeper on another Mac refuses to open it until the
# maintainer signs and notarizes it with scripts/sign-notarize-macos.sh.
#
#   bash scripts/build-macos-app.sh [--version X.Y.Z[-suffix]] [--out DIR]
#   bash scripts/build-macos-app.sh --dmg-only "/path/WithAlbum Server.app" /path/out.dmg
#
# Needs macOS 13+ with Xcode (swift, lipo, codesign, hdiutil) and Go.
# Output (default dist/macos, ignored by git):
#   WithAlbum Server.app, WithAlbumServer_<version>_unsigned.dmg (+ .sha256)
set -euo pipefail
if [[ "$(uname -s)" != Darwin ]]; then echo 'macOS only' >&2; exit 1; fi
REPO="$(cd "$(dirname "$0")/.." && pwd)"
APP_NAME="WithAlbum Server"

# make_dmg APP OUT: a compressed read-only image with the app and a link
# to /Applications (drag to install).
make_dmg() {
  local app="$1" out="$2" stage
  stage="$(mktemp -d)"
  ditto "$app" "$stage/$APP_NAME.app"
  ln -s /Applications "$stage/Applications"
  rm -f "$out"
  hdiutil create -quiet -volname "$APP_NAME" -srcfolder "$stage" -fs HFS+ -format UDZO -ov "$out"
  rm -rf "$stage"
  hdiutil verify -quiet "$out"
}

if [[ "${1:-}" == --dmg-only ]]; then
  [[ $# -eq 3 && -d "$2" ]] || { echo 'Usage: build-macos-app.sh --dmg-only APP OUT.dmg' >&2; exit 2; }
  make_dmg "$2" "$3"
  echo "$3"
  exit 0
fi

VERSION=""
OUT="$REPO/dist/macos"
while [[ $# -gt 0 ]]; do
  case "$1" in
    --version) VERSION="${2:?}"; shift 2 ;;
    --out) OUT="${2:?}"; shift 2 ;;
    *) echo "Unknown argument: $1" >&2; exit 2 ;;
  esac
done
if [[ -z "$VERSION" ]]; then
  VERSION="$(git -C "$REPO" describe --tags --match 'v*' --dirty 2>/dev/null || echo dev)"
  VERSION="${VERSION#v}"
fi
[[ "$VERSION" =~ ^[0-9A-Za-z.+-]+$ ]] || { echo "Bad version: $VERSION" >&2; exit 2; }
SHORT="$(printf '%s' "$VERSION" | grep -Eo '^[0-9]+\.[0-9]+\.[0-9]+' || true)"
SHORT="${SHORT:-0.0.0}"

for tool in go swift lipo strip codesign hdiutil plutil ditto; do
  command -v "$tool" >/dev/null || { echo "Missing $tool (install Xcode and Go)" >&2; exit 1; }
done

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
mkdir -p "$OUT"

echo "==> server $VERSION (darwin arm64 + amd64, CGO_ENABLED=0, -trimpath)"
for arch in arm64 amd64; do
  (cd "$REPO" && CGO_ENABLED=0 GOOS=darwin GOARCH="$arch" \
    go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o "$WORK/withalbum-server-$arch" ./server)
done
lipo -create -output "$WORK/withalbum-server" "$WORK/withalbum-server-arm64" "$WORK/withalbum-server-amd64"

echo "==> menu bar app (Swift, arm64 + x86_64, macOS 13+)"
swift build -c release --arch arm64 --arch x86_64 --package-path "$REPO/macos"
BIN_DIR="$(swift build -c release --arch arm64 --arch x86_64 --package-path "$REPO/macos" --show-bin-path)"

APP="$OUT/$APP_NAME.app"
rm -rf "$APP"
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Helpers" "$APP/Contents/Resources"
install -m 0755 "$BIN_DIR/WithAlbumServer" "$APP/Contents/MacOS/WithAlbumServer"
# Debug map symbols name the build machine's object files: strip them.
strip -S -x "$APP/Contents/MacOS/WithAlbumServer" 2>/dev/null
install -m 0755 "$WORK/withalbum-server" "$APP/Contents/Helpers/withalbum-server"
sed -e "s/@VERSION@/$VERSION/g" -e "s/@SHORT_VERSION@/$SHORT/g" "$REPO/macos/Support/Info.plist.in" > "$APP/Contents/Info.plist"
plutil -lint -s "$APP/Contents/Info.plist"
printf 'APPL????' > "$APP/Contents/PkgInfo"
# AGPL-3.0 text, notices and the third-party licenses travel with the app.
cp "$REPO/LICENSE" "$REPO/NOTICE" "$APP/Contents/Resources/"
ditto "$REPO/third_party" "$APP/Contents/Resources/third_party"

# Ad-hoc signature only (no identity): a consistent bundle seal for local
# use. Inside first, then the app.
codesign --force --sign - --timestamp=none "$APP/Contents/Helpers/withalbum-server"
codesign --force --sign - --timestamp=none "$APP"
codesign --verify --strict "$APP"

echo "==> checks"
"$APP/Contents/MacOS/WithAlbumServer" --self-check
for a in arm64 x86_64; do lipo "$APP/Contents/MacOS/WithAlbumServer" -verify_arch "$a"; lipo "$APP/Contents/Helpers/withalbum-server" -verify_arch "$a"; done
if grep -a -q -F -e "$REPO" -e "$HOME" "$APP/Contents/MacOS/WithAlbumServer" "$APP/Contents/Helpers/withalbum-server"; then
  echo "The binaries contain a build machine path ($REPO or $HOME)" >&2
  exit 1
fi

DMG="$OUT/WithAlbumServer_${VERSION}_unsigned.dmg"
echo "==> $DMG"
make_dmg "$APP" "$DMG"
(cd "$OUT" && shasum -a 256 "$(basename "$DMG")" > "$(basename "$DMG").sha256")
echo "Built (unsigned, not notarized):"
echo "  $APP"
echo "  $DMG"
echo "Sign and notarize for distribution: scripts/sign-notarize-macos.sh (docs/RELEASING.md)."
