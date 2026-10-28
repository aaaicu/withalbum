#!/bin/bash
# Builds the release archives of .goreleaser.yaml without GoReleaser and
# checks them: same targets, flags, version stamp, archive names and file
# list, plus checksums.txt. Nothing is published, tagged or committed.
#
#   bash scripts/release-dryrun.sh [-o OUT_DIR] [TAG]
#
# TAG defaults to v0.0.0-dryrun. OUT_DIR must be outside this repository
# (default: a new temporary directory, printed at the end).
set -euo pipefail
CALLER_DIR="$(pwd -P)"
cd "$(dirname "$0")/.."
REPO="$(pwd -P)"

OUT=""
while getopts "o:" opt; do
  case "$opt" in
    o) OUT="$OPTARG" ;;
    *) echo "Usage: bash scripts/release-dryrun.sh [-o OUT_DIR] [TAG]" >&2; exit 2 ;;
  esac
done
shift $((OPTIND - 1))
TAG="${1:-v0.0.0-dryrun}"
[[ "$TAG" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] || { echo "TAG must look like v1.2.3 or v1.2.3-alpha.1, got $TAG" >&2; exit 2; }
VERSION="${TAG#v}"

inside_repo() { case "$1/" in "$REPO"/*) return 0 ;; esac; return 1; }
if [[ -z "$OUT" ]]; then
  OUT="$(mktemp -d "${TMPDIR:-/tmp}/withalbum-release-dryrun.XXXXXX")"
else
  [[ "$OUT" == /* ]] || OUT="$CALLER_DIR/$OUT"
  # Refuse before creating anything: resolve the (existing) parent.
  parent="$(dirname "$OUT")"
  [[ -d "$parent" ]] || { echo "the parent of OUT_DIR must exist: $parent" >&2; exit 2; }
  OUT="$(cd "$parent" && pwd -P)/$(basename "$OUT")"
  if inside_repo "$OUT"; then echo "OUT_DIR must be outside the repository ($REPO)" >&2; exit 2; fi
  mkdir -p "$OUT"
fi
OUT="$(cd "$OUT" && pwd -P)"
if inside_repo "$OUT"; then echo "OUT_DIR must be outside the repository ($REPO)" >&2; exit 2; fi
[[ -z "$(ls -A "$OUT")" ]] || { echo "OUT_DIR $OUT is not empty" >&2; exit 2; }

fail() { echo "FAIL: $*" >&2; exit 1; }

# The archive file list, read from .goreleaser.yaml (plain paths/globs).
files_list() {
  awk '
    /^archives:/ { in_archives = 1; next }
    in_archives && /^[^ ]/ { in_archives = 0 }
    in_archives && /^    files:/ { in_files = 1; next }
    in_files && /^      - / { sub(/^      - /, ""); print; next }
    in_files { in_files = 0 }
  ' .goreleaser.yaml
}
PATTERNS="$(files_list)"
[[ -n "$PATTERNS" ]] || fail "no archive files found in .goreleaser.yaml"
if grep -qE '(src|dst):' <<< "$PATTERNS"; then fail "archive files must be plain paths in .goreleaser.yaml"; fi

# Targets: GoReleaser name -> GOOS GOARCH GOARM.
TARGETS="darwin_arm64:darwin:arm64: darwin_amd64_v1:darwin:amd64: linux_amd64_v1:linux:amd64: linux_arm64:linux:arm64: linux_arm_7:linux:arm:7"
for t in $TARGETS; do
  grep -qE "^      - ${t%%:*}$" .goreleaser.yaml || fail "target ${t%%:*} is not in .goreleaser.yaml"
done
[[ "$(grep -cE '^      - (darwin|linux)_' .goreleaser.yaml)" == 5 ]] || fail ".goreleaser.yaml has other targets than the 5 checked here"
grep -q -- '-X main.version={{.Version}}' .goreleaser.yaml || fail "ldflags in .goreleaser.yaml no longer set main.version"
grep -q -- '- -trimpath' .goreleaser.yaml || fail ".goreleaser.yaml does not build with -trimpath"

echo "== third-party licenses"
bash scripts/third-party-licenses.sh --check

export COPYFILE_DISABLE=1 # no AppleDouble files in archives made on macOS
# Expands one archive pattern: "dir/**/*" (every file below dir) or a
# plain path/glob. (macOS bash 3.2 has no globstar.)
expand() {
  case "$1" in
    */\*\*/\*) find "${1%/\*\*/\*}" -type f | LC_ALL=C sort ;;
    *\*\**) fail "unsupported pattern $1" ;;
    *) for f in $1; do [[ -e "$f" ]] && echo "$f"; done ;;
  esac
}
STAGE="$OUT/.stage"
mkdir -p "$STAGE"
LDFLAGS="-s -w -X main.version=$VERSION"

for t in $TARGETS; do
  IFS=: read -r _ goos goarch goarm <<< "$t"
  arch="$goarch${goarm:+v$goarm}"
  name="withalbum-server_${VERSION}_${goos}_${arch}"
  dir="$STAGE/$name"
  mkdir -p "$dir"
  echo "== build $goos/$arch"
  GOOS="$goos" GOARCH="$goarch" GOARM="$goarm" CGO_ENABLED=0 \
    go build -trimpath -ldflags "$LDFLAGS" -o "$dir/withalbum-server" ./server
  # Archive files, with their paths kept (GoReleaser's default).
  while IFS= read -r pattern; do
    matched=0
    while IFS= read -r f; do
      [[ -f "$f" ]] || continue
      mkdir -p "$dir/$(dirname "$f")"
      cp -p "$f" "$dir/$f"
      matched=1
    done < <(expand "$pattern")
    [[ $matched == 1 ]] || fail "archive file pattern matches nothing: $pattern"
  done <<< "$PATTERNS"
  if [[ "$goos" == darwin ]]; then
    (cd "$dir" && zip -qrX "$OUT/$name.zip" .)
  elif tar --version 2>/dev/null | grep -q bsdtar; then
    tar -czf "$OUT/$name.tar.gz" --uid 0 --gid 0 --uname root --gname root -C "$dir" .
  else
    tar -czf "$OUT/$name.tar.gz" --owner=0 --group=0 --numeric-owner -C "$dir" .
  fi
done

echo "== checksums"
(cd "$OUT" && shasum -a 256 withalbum-server_* > checksums.txt && shasum -a 256 -c checksums.txt >/dev/null) || fail "checksums"

echo "== checks"
host="$(go env GOOS)_$(go env GOARCH)"
for t in $TARGETS; do
  IFS=: read -r _ goos goarch goarm <<< "$t"
  arch="$goarch${goarm:+v$goarm}"
  bin="$STAGE/withalbum-server_${VERSION}_${goos}_${arch}/withalbum-server"
  info="$(go version -m "$bin")"
  grep -q "path	github.com/aaaicu/withalbum/server" <<< "$info" || fail "$goos/$arch: not built from ./server"
  grep -q "build	-trimpath=true" <<< "$info" || fail "$goos/$arch: not built with -trimpath"
  grep -q "build	CGO_ENABLED=0" <<< "$info" || fail "$goos/$arch: cgo enabled"
  # (-trimpath leaves -ldflags out of the build info: look for the value.)
  grep -q -a -F "$VERSION" "$bin" || fail "$goos/$arch: version not stamped"
  # No path of the build machine in the binary.
  if [[ -n "${HOME:-}" ]] && grep -q -a -F "$HOME" "$bin"; then fail "$goos/$arch: binary contains \$HOME"; fi
  if grep -q -a -F "$REPO" "$bin"; then fail "$goos/$arch: binary contains the source path"; fi
  if [[ "${goos}_$goarch" == "$host" && -z "$goarm" ]]; then
    got="$("$bin" -version)"
    [[ "$got" == "withalbum-server $VERSION" ]] || fail "-version printed '$got', want 'withalbum-server $VERSION'"
    echo "-version ($goos/$arch): $got"
  fi
done
# The macOS install script must stay executable inside the zip.
probe="$OUT/.probe"
mkdir -p "$probe"
(cd "$probe" && unzip -q "$OUT/withalbum-server_${VERSION}_darwin_arm64.zip")
[[ -x "$probe/scripts/install-macos.sh" && -x "$probe/withalbum-server" ]] || fail "zip lost the executable bit"
for f in LICENSE NOTICE third_party/go-modules/MODULES.txt third_party/seed-design/LICENSE-APACHE-2.0.txt; do
  [[ -f "$probe/$f" ]] || fail "zip lacks $f"
done
rm -rf "$probe" "$STAGE"

echo
echo "Dry run of $TAG: OK. Artifacts in $OUT (nothing published):"
(cd "$OUT" && ls -l withalbum-server_* checksums.txt | awk '{print $5, $NF}')
echo
cat "$OUT/checksums.txt"
echo
echo "Contents of withalbum-server_${VERSION}_linux_arm64.tar.gz:"
tar -tzf "$OUT/withalbum-server_${VERSION}_linux_arm64.tar.gz" | sed 's|^\./||' | grep -v '^$' | grep -v '/$' | sort
