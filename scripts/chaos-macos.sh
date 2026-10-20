#!/bin/bash
# Chaos test on macOS: the library lives on an hdiutil sparse disk image
# standing in for an external disk, the real server binary runs on it, and
# every round injects one failure (SIGKILL mid-upload or at a fault point,
# forced detach without sync, detach + reattach with a new device number,
# full disk, SIGTERM). After each round the server is restarted and
# "verify -hash" plus the API must agree with what the server acknowledged.
# See tools/chaos/main.go for the exact checks and docs/CHAOS_RESULTS.md.
#
#   bash scripts/chaos-macos.sh [-n ROUNDS] [-s SEED] [-w WORK_DIR] [-r REPORT.md] [-k] [-v]
#
#   -n  rounds (default 100)
#   -s  random seed (default: from the clock; printed in the report)
#   -w  scratch directory for images and mount points (default: a new
#       temporary directory). Must be outside this repository.
#   -r  also write the results table (Markdown) to this file
#   -k  keep WORK_DIR (images and server logs) afterwards
#   -v  print the server's log lines
#
# Only disk images created in WORK_DIR are attached (with -nobrowse, at
# mount points inside WORK_DIR); real disks are never touched. Every image
# attached from WORK_DIR is detached and every server started by the test is
# killed on exit, also on ^C or failure.
set -euo pipefail
[[ "$(uname -s)" == "Darwin" ]] || { echo "chaos-macos.sh needs macOS (hdiutil)" >&2; exit 2; }
CALLER_DIR="$(pwd -P)"
cd "$(dirname "$0")/.."
REPO="$(pwd -P)"

ROUNDS=100 SEED=0 WORK="" REPORT="" KEEP=0 VERBOSE=""
while getopts "n:s:w:r:kv" opt; do
  case "$opt" in
    n) ROUNDS="$OPTARG" ;;
    s) SEED="$OPTARG" ;;
    w) WORK="$OPTARG" ;;
    r) REPORT="$OPTARG" ;;
    k) KEEP=1 ;;
    v) VERBOSE="-v" ;;
    *) sed -n '9,18p' "$0" >&2; exit 2 ;;
  esac
done
[[ "$ROUNDS" =~ ^[0-9]+$ && "$ROUNDS" -ge 1 ]] || { echo "-n must be a positive number" >&2; exit 2; }
[[ "$SEED" =~ ^[0-9]+$ ]] || { echo "-s must be a number" >&2; exit 2; }

inside_repo() { case "$1/" in "$REPO"/*) return 0 ;; esac; return 1; }
if [[ -z "$WORK" ]]; then
  WORK="$(mktemp -d "${TMPDIR:-/tmp}/withalbum-chaos.XXXXXX")"
else
  [[ "$WORK" == /* ]] || WORK="$CALLER_DIR/$WORK"
  mkdir -p "$WORK"
fi
WORK="$(cd "$WORK" && pwd -P)"
if inside_repo "$WORK"; then echo "WORK_DIR must be outside the repository ($REPO)" >&2; exit 2; fi
if [[ -n "$REPORT" && "$REPORT" != /* ]]; then REPORT="$CALLER_DIR/$REPORT"; fi
RUN="$WORK/run-$(date -u +%Y%m%dT%H%M%SZ)"
mkdir "$RUN"

# Detach every image attached from WORK (by image path) and kill servers
# started from RUN's binaries.
cleanup() {
  local status=$?
  pkill -KILL -f "^$RUN/bin/withalbum-server" 2>/dev/null || true
  pkill -KILL -f "^$RUN/bin/chaos" 2>/dev/null || true
  local dev
  while read -r dev; do
    [[ -n "$dev" ]] && hdiutil detach -force -quiet "$dev" 2>/dev/null || true
  done < <(hdiutil info 2>/dev/null | awk -v w="$WORK/" '
    /^image-path/ { img = substr($0, index($0, ":") + 2); inwork = (index(img, w) == 1) }
    inwork && /^\/dev\/disk[0-9]+[ \t]/ { print $1; inwork = 0 }')
  if hdiutil info 2>/dev/null | grep -q "image-path *: $WORK/"; then
    echo "WARNING: an image from $WORK is still attached; detach it with: hdiutil info / hdiutil detach -force <dev>" >&2
  fi
  if [[ "$KEEP" -eq 0 ]]; then
    rm -rf "$RUN"
    rmdir "$WORK" 2>/dev/null || true
  else
    echo "kept $RUN" >&2
  fi
  exit "$status"
}
trap cleanup EXIT INT TERM

mkdir "$RUN/bin"
echo "building the server with -tags chaos (test build only) and the chaos driver"
CGO_ENABLED=0 go build -tags chaos -trimpath -o "$RUN/bin/withalbum-server" ./server
CGO_ENABLED=0 go build -trimpath -o "$RUN/bin/chaos" ./tools/chaos

args=(-server "$RUN/bin/withalbum-server" -work "$RUN" -rounds "$ROUNDS" -seed "$SEED" -report "$RUN/report.md")
[[ -n "$VERBOSE" ]] && args+=("$VERBOSE")
set +e
"$RUN/bin/chaos" "${args[@]}"
status=$?
set -e
if [[ -n "$REPORT" && -f "$RUN/report.md" ]]; then
  cp "$RUN/report.md" "$REPORT"
  echo "results table written to $REPORT"
fi
exit "$status"
