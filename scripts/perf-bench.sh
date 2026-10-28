#!/bin/bash
# Performance measurement on synthetic libraries (no real photos): builds
# the server and tools/perf, then measures startup, snapshot and feed
# paging, uploads, verify and verify -hash for each size. See
# docs/PERFORMANCE.md for the last recorded results.
#
#   bash scripts/perf-bench.sh [-n 100000,500000] [-w WORK_DIR] [-r REPORT.md] [-k]
#
#   -n  comma-separated library sizes (default 100000,500000)
#   -w  scratch directory (default: a new temporary directory); it needs
#       about 4 KiB of disk per asset (one tiny original file each) plus the
#       database (about 1 KiB per asset). Must be outside this repository.
#   -r  also write the results (Markdown) to this file
#   -k  keep the synthetic libraries
set -euo pipefail
CALLER_DIR="$(pwd -P)"
cd "$(dirname "$0")/.."
REPO="$(pwd -P)"
SIZES=100000,500000 WORK="" REPORT="" KEEP=""
while getopts "n:w:r:k" opt; do
  case "$opt" in
    n) SIZES="$OPTARG" ;;
    w) WORK="$OPTARG" ;;
    r) REPORT="$OPTARG" ;;
    k) KEEP="-keep" ;;
    *) sed -n '7,14p' "$0" >&2; exit 2 ;;
  esac
done
[[ "$SIZES" =~ ^[0-9]+(,[0-9]+)*$ ]] || { echo "-n expects sizes like 100000,500000" >&2; exit 2; }
inside_repo() { case "$1/" in "$REPO"/*) return 0 ;; esac; return 1; }
CREATED=0
if [[ -z "$WORK" ]]; then
  WORK="$(mktemp -d "${TMPDIR:-/tmp}/withalbum-perf.XXXXXX")"
  CREATED=1
else
  [[ "$WORK" == /* ]] || WORK="$CALLER_DIR/$WORK"
  mkdir -p "$WORK"
fi
WORK="$(cd "$WORK" && pwd -P)"
if inside_repo "$WORK"; then echo "WORK_DIR must be outside the repository ($REPO)" >&2; exit 2; fi
if [[ -n "$REPORT" && "$REPORT" != /* ]]; then REPORT="$CALLER_DIR/$REPORT"; fi
BIN="$WORK/bin"
cleanup() {
  local status=$?
  pkill -KILL -f "^$BIN/withalbum-server" 2>/dev/null || true
  rm -rf "$BIN"
  if [[ "$CREATED" -eq 1 && -z "$KEEP" ]]; then rm -rf "$WORK"; fi
  exit "$status"
}
trap cleanup EXIT INT TERM
mkdir -p "$BIN"
CGO_ENABLED=0 go build -trimpath -o "$BIN/withalbum-server" ./server
CGO_ENABLED=0 go build -trimpath -o "$BIN/perf" ./tools/perf
args=(-server "$BIN/withalbum-server" -work "$WORK" -n "$SIZES")
[[ -n "$REPORT" ]] && args+=(-report "$REPORT")
[[ -n "$KEEP" ]] && args+=("$KEEP")
"$BIN/perf" "${args[@]}"
