#!/bin/bash
# Restore drill: back up the library, restore the new snapshot into a fresh
# temporary storage root, verify every original there and start a server on
# it once. The library itself is only read (backup is safe while the server
# runs); nothing is restored over it.
#
#   bash scripts/restore-drill.sh [-k] [-n KEEP] [-t TMP_PARENT] /path/to/withalbum-server BACKUP_DIR
#   bash scripts/restore-drill.sh --demo [-k] /path/to/withalbum-server
#
#   BACKUP_DIR  a backup target registered once with "backup -init -to DIR"
#   -n KEEP     pass -keep KEEP to the backup (remove older snapshots)
#   -t DIR      parent of the temporary restore root (default: $TMPDIR). It
#               needs room for a full copy of the library.
#   -k          keep the temporary restore root afterwards
#   --demo      make a small throwaway library and backup target in a
#               temporary directory first (to try the drill without data)
#
# The library is the one the server binary would use: WITHALBUM_ROOT if set,
# else the storage registered by the setup screen (WITHALBUM_CONFIG).
# Exit status: 0 drill passed, 1 a step failed (its output is shown), 2 usage.
set -euo pipefail

usage() { sed -n '7,17p' "$0" >&2; exit 2; }
DEMO=0 KEEP_TMP=0 KEEP_SNAPS="" TMP_PARENT="${TMPDIR:-/tmp}"
if [[ "${1:-}" == --demo ]]; then DEMO=1; shift; fi
while getopts "kn:t:" opt; do
  case "$opt" in
    k) KEEP_TMP=1 ;;
    n) KEEP_SNAPS="$OPTARG" ;;
    t) TMP_PARENT="$OPTARG" ;;
    *) usage ;;
  esac
done
shift $((OPTIND - 1))
if [[ "$DEMO" -eq 1 ]]; then [[ $# -eq 1 ]] || usage; else [[ $# -eq 2 ]] || usage; fi
BIN="$1"
[[ -f "$BIN" && -x "$BIN" ]] || { echo "not an executable: $BIN" >&2; exit 2; }
BIN="$(cd "$(dirname "$BIN")" && pwd -P)/$(basename "$BIN")"
[[ -z "$KEEP_SNAPS" || "$KEEP_SNAPS" =~ ^[1-9][0-9]*$ ]] || { echo "-n must be a positive number" >&2; exit 2; }
[[ -d "$TMP_PARENT" ]] || { echo "-t: $TMP_PARENT does not exist" >&2; exit 2; }

DRILL="$(mktemp -d "$TMP_PARENT/withalbum-restore-drill.XXXXXX")"
SERVER_PID=""
cleanup() {
  local status=$?
  if [[ -n "$SERVER_PID" ]]; then kill "$SERVER_PID" 2>/dev/null || true; wait "$SERVER_PID" 2>/dev/null || true; fi
  if [[ "$KEEP_TMP" -eq 0 ]]; then rm -rf "$DRILL"; else echo "kept $DRILL" >&2; fi
  exit "$status"
}
trap cleanup EXIT INT TERM

step() { printf '\n== %s\n' "$*"; }
fail() { echo "DRILL FAILED: $*" >&2; exit 1; }

# A port nobody listens on, on loopback.
free_port() {
  local p
  for _ in $(seq 1 50); do
    p=$((20000 + RANDOM % 30000))
    if ! (exec 3<>"/dev/tcp/127.0.0.1/$p") 2>/dev/null; then echo "$p"; return; fi
  done
  fail "no free loopback port"
}

wait_health() { # url
  for _ in $(seq 1 100); do
    if out="$(curl -fsS --max-time 2 "$1/api/v1/health" 2>/dev/null)"; then echo "$out"; return 0; fi
    sleep 0.1
  done
  return 1
}

if [[ "$DEMO" -eq 1 ]]; then
  step "demo: a throwaway library with a few originals"
  export WITHALBUM_ROOT="$DRILL/demo-library" WITHALBUM_CONFIG="$DRILL/demo-config.json"
  unset WITHALBUM_VOLUME_ID
  mkdir "$WITHALBUM_ROOT" "$DRILL/demo-backup"
  "$BIN" -init-storage >/dev/null
  printf '%s\n' "demo-drill-password" | "$BIN" admin init -login drill >/dev/null
  port="$(free_port)"
  WITHALBUM_LISTEN="127.0.0.1:$port" "$BIN" >"$DRILL/demo-server.log" 2>&1 &
  SERVER_PID=$!
  wait_health "http://127.0.0.1:$port" >/dev/null || fail "demo server did not start: $(tail -3 "$DRILL/demo-server.log")"
  token="$(curl -fsS -H 'Content-Type: application/json' \
    -d '{"login_name":"drill","password":"demo-drill-password","device_name":"drill","platform":"macos"}' \
    "http://127.0.0.1:$port/api/v1/auth/login" | sed -n 's/.*"access_token":"\([^"]*\)".*/\1/p')"
  [[ -n "$token" ]] || fail "demo login"
  folder="$(curl -fsS -H "Authorization: Bearer $token" -H 'Content-Type: application/json' -d '{"name":"drill"}' \
    "http://127.0.0.1:$port/api/v1/folders" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')"
  [[ -n "$folder" ]] || fail "demo folder"
  for i in 1 2 3 4 5; do
    head -c $((4096 * i + i)) /dev/urandom >"$DRILL/demo-$i.bin"
    curl -fsS -o /dev/null -H "Authorization: Bearer $token" -H "Idempotency-Key: drill-$i" -H "X-Filename: demo-$i.bin" \
      --data-binary "@$DRILL/demo-$i.bin" "http://127.0.0.1:$port/api/v1/folders/$folder/assets" || fail "demo upload $i"
  done
  BACKUP_DIR="$DRILL/demo-backup"
  "$BIN" backup -init -to "$BACKUP_DIR" >/dev/null
  # The server keeps running during the backup, as it would for real.
else
  BACKUP_DIR="$2"
fi

step "1. backup (the library is only read)"
args=(backup -to "$BACKUP_DIR" -json)
[[ -n "$KEEP_SNAPS" ]] && args+=(-keep "$KEEP_SNAPS")
if ! "$BIN" "${args[@]}" >"$DRILL/backup.json" 2>"$DRILL/backup.err"; then
  cat "$DRILL/backup.err" >&2
  fail "backup"
fi
SNAP="$(plutil -extract path raw -o - "$DRILL/backup.json" 2>/dev/null || sed -n 's/^  "path": "\(.*\)",$/\1/p' "$DRILL/backup.json")"
BLOBS="$(plutil -extract blobs raw -o - "$DRILL/backup.json" 2>/dev/null || sed -n 's/^  "blobs": \([0-9]*\),$/\1/p' "$DRILL/backup.json")"
[[ -n "$SNAP" && -d "$SNAP" && -n "$BLOBS" ]] || fail "cannot read the snapshot path from the backup report"
echo "snapshot $SNAP ($BLOBS originals)"

if [[ -n "$SERVER_PID" ]]; then kill "$SERVER_PID"; wait "$SERVER_PID" 2>/dev/null || true; SERVER_PID=""; fi

step "2. restore -check (every original of the snapshot hashed)"
"$BIN" restore -from "$SNAP" -check || fail "restore -check"

step "3. restore into a fresh temporary storage root"
RESTORED="$DRILL/restored"
mkdir "$RESTORED"
# The drill root is its own volume: never the library's pin or config.
drill_env=(env -u WITHALBUM_VOLUME_ID WITHALBUM_ROOT="$RESTORED" WITHALBUM_CONFIG="$DRILL/drill-config.json")
"${drill_env[@]}" "$BIN" -init-storage >/dev/null || fail "-init-storage of the drill root"
"${drill_env[@]}" "$BIN" restore -from "$SNAP" || fail "restore"

step "4. verify -hash on the restored root"
if ! "${drill_env[@]}" "$BIN" verify -hash -json >"$DRILL/verify.json" 2>"$DRILL/verify.err"; then
  cat "$DRILL/verify.json" "$DRILL/verify.err" >&2
  fail "verify -hash"
fi
got="$(plutil -extract blobs raw -o - "$DRILL/verify.json" 2>/dev/null || sed -n 's/^  "blobs": \([0-9]*\),$/\1/p' "$DRILL/verify.json")"
hashed="$(plutil -extract hashed raw -o - "$DRILL/verify.json" 2>/dev/null || sed -n 's/^  "hashed": \([0-9]*\),$/\1/p' "$DRILL/verify.json")"
[[ "$got" == "$BLOBS" && "$hashed" == "$BLOBS" ]] || fail "restored root has $got originals ($hashed hashed), the snapshot $BLOBS"
echo "verify -hash: OK, $hashed originals"

step "5. start a server on the restored root (loopback, temporary port)"
port="$(free_port)"
"${drill_env[@]}" WITHALBUM_LISTEN="127.0.0.1:$port" "$BIN" >"$DRILL/restored-server.log" 2>&1 &
SERVER_PID=$!
health="$(wait_health "http://127.0.0.1:$port")" || fail "server on the restored root did not start: $(tail -3 "$DRILL/restored-server.log")"
[[ "$health" == *'"storage":"ok"'* && "$health" == *'"integrity":"ok"'* ]] || fail "health $health"
grep "recovery:" "$DRILL/restored-server.log" || true
kill "$SERVER_PID"; wait "$SERVER_PID" 2>/dev/null || true; SERVER_PID=""
echo "health: $health"

printf '\nRESTORE DRILL PASSED: snapshot %s, %s originals restored and verified by SHA-256.\n' "$(basename "$SNAP")" "$BLOBS"
