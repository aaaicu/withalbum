#!/bin/bash
# Scheduled backup on macOS: a LaunchAgent that runs
#   withalbum-server backup -to BACKUP_DIR -keep N
# every day at a fixed time (launchd StartCalendarInterval), optionally plus
# a weekly "verify -hash". Developer/administrator tool, not part of the app.
#
#   bash scripts/install-backup-macos.sh [--keep N] [--at HH:MM] [--verify-weekly] [--print] /path/to/withalbum-server BACKUP_DIR
#   bash scripts/install-backup-macos.sh --uninstall
#
#   BACKUP_DIR       registered once with: withalbum-server backup -init -to BACKUP_DIR
#   --keep N         snapshots to keep (default 14; originals are shared and stay)
#   --at HH:MM       daily start time (default 03:00). A Mac that is asleep then
#                    runs the job when it wakes (launchd coalesces missed runs).
#   --verify-weekly  also run "verify -hash" every Sunday at 04:00
#   --print          only print the plist(s) to stdout; install nothing
#
# The library is the one the server uses: WITHALBUM_ROOT if set when this
# script runs (it is written into the agent), else the storage registered by
# the setup screen. The backup runs while the server runs. A day without the
# backup disk fails with "does not exist" and writes nothing anywhere; see
# ~/Library/Logs/WithAlbum/backup.err. Exit status of each run is in the log.
set -euo pipefail
[[ "$(uname -s)" == Darwin ]] || { echo 'macOS only' >&2; exit 1; }
usage() { sed -n '7,16p' "$0" >&2; exit 2; }

AGENTS="$HOME/Library/LaunchAgents"
LOGS="$HOME/Library/Logs/WithAlbum"
BACKUP_LABEL=org.withalbum.backup
VERIFY_LABEL=org.withalbum.verify

if [[ "${1:-}" == --uninstall ]]; then
  for label in "$BACKUP_LABEL" "$VERIFY_LABEL"; do
    launchctl bootout "gui/$(id -u)/$label" 2>/dev/null || true
    rm -f "$AGENTS/$label.plist"
  done
  echo "Removed the WithAlbum backup/verify LaunchAgents (backups already written are untouched)."
  exit 0
fi

KEEP=14 AT=03:00 VERIFY=0 PRINT=0
while [[ $# -gt 0 && "$1" == --* ]]; do
  case "$1" in
    --keep) [[ $# -ge 2 ]] || usage; KEEP="$2"; shift 2 ;;
    --at) [[ $# -ge 2 ]] || usage; AT="$2"; shift 2 ;;
    --verify-weekly) VERIFY=1; shift ;;
    --print) PRINT=1; shift ;;
    *) usage ;;
  esac
done
[[ $# -eq 2 ]] || usage
BIN="$1" DIR="$2"
for v in "$BIN" "$DIR" "${WITHALBUM_ROOT:-}" "${WITHALBUM_CONFIG:-}"; do
  [[ "$v" != *$'\n'* && "$v" != *$'\r'* ]] || { echo 'Unsupported newline in argument' >&2; exit 2; }
done
[[ "$KEEP" =~ ^[1-9][0-9]*$ ]] || { echo '--keep must be a positive number' >&2; exit 2; }
[[ "$AT" =~ ^([01][0-9]|2[0-3]):([0-5][0-9])$ ]] || { echo '--at must be HH:MM (24 h)' >&2; exit 2; }
HOUR=$((10#${BASH_REMATCH[1]})) MINUTE=$((10#${BASH_REMATCH[2]}))
[[ "$BIN" == /* && -f "$BIN" && -x "$BIN" ]] || { echo 'Expected the absolute path of an executable withalbum-server' >&2; exit 2; }
[[ "$DIR" == /* ]] || { echo 'BACKUP_DIR must be an absolute path' >&2; exit 2; }
if [[ -d "$DIR" && ! -f "$DIR/.withalbum-backup-target.json" ]]; then
  echo "WARNING: $DIR is not a registered backup target yet; register it once with: $BIN backup -init -to \"$DIR\"" >&2
elif [[ ! -d "$DIR" ]]; then
  echo "NOTE: $DIR is not there now (backup disk not connected?); runs fail until it is." >&2
fi

# plist <label> <log name> <args...>: builds the agent with plutil, so every
# value is escaped correctly.
plist() {
  local label="$1" log="$2" file
  shift 2
  file="$(mktemp "${TMPDIR:-/tmp}/withalbum-agent.XXXXXX")"
  plutil -create xml1 "$file"
  plutil -insert Label -string "$label" "$file"
  plutil -insert ProgramArguments -array "$file"
  local i=0 a
  for a in "$BIN" "$@"; do
    plutil -insert "ProgramArguments.$i" -string "$a" "$file"
    i=$((i + 1))
  done
  plutil -insert EnvironmentVariables -dictionary "$file"
  plutil -insert EnvironmentVariables.PATH -string /usr/bin:/bin:/usr/sbin:/sbin "$file"
  [[ -n "${WITHALBUM_ROOT:-}" ]] && plutil -insert EnvironmentVariables.WITHALBUM_ROOT -string "$WITHALBUM_ROOT" "$file"
  [[ -n "${WITHALBUM_CONFIG:-}" ]] && plutil -insert EnvironmentVariables.WITHALBUM_CONFIG -string "$WITHALBUM_CONFIG" "$file"
  [[ -n "${WITHALBUM_VOLUME_ID:-}" ]] && plutil -insert EnvironmentVariables.WITHALBUM_VOLUME_ID -string "$WITHALBUM_VOLUME_ID" "$file"
  plutil -insert StartCalendarInterval -dictionary "$file"
  if [[ "$label" == "$VERIFY_LABEL" ]]; then
    plutil -insert StartCalendarInterval.Weekday -integer 0 "$file"
    plutil -insert StartCalendarInterval.Hour -integer 4 "$file"
    plutil -insert StartCalendarInterval.Minute -integer 0 "$file"
  else
    plutil -insert StartCalendarInterval.Hour -integer "$HOUR" "$file"
    plutil -insert StartCalendarInterval.Minute -integer "$MINUTE" "$file"
  fi
  plutil -insert LowPriorityIO -bool true "$file"
  plutil -insert Nice -integer 10 "$file"
  plutil -insert StandardOutPath -string "$LOGS/$log.log" "$file"
  plutil -insert StandardErrorPath -string "$LOGS/$log.err" "$file"
  plutil -lint -s "$file"
  echo "$file"
}

files=("$(plist "$BACKUP_LABEL" backup backup -to "$DIR" -keep "$KEEP")")
labels=("$BACKUP_LABEL")
if [[ "$VERIFY" -eq 1 ]]; then
  files+=("$(plist "$VERIFY_LABEL" verify verify -hash)")
  labels+=("$VERIFY_LABEL")
fi

if [[ "$PRINT" -eq 1 ]]; then
  for f in "${files[@]}"; do cat "$f"; rm -f "$f"; done
  exit 0
fi

mkdir -p "$AGENTS" "$LOGS"
chmod 700 "$LOGS"
for i in "${!files[@]}"; do
  label="${labels[$i]}" dest="$AGENTS/${labels[$i]}.plist"
  launchctl bootout "gui/$(id -u)/$label" 2>/dev/null || true
  mv "${files[$i]}" "$dest"
  chmod 644 "$dest"
  launchctl bootstrap "gui/$(id -u)" "$dest"
  echo "Installed $label ($dest)"
done
echo "Daily backup at $(printf '%02d:%02d' "$HOUR" "$MINUTE") to $DIR, keeping $KEEP snapshots. Run it now: launchctl kickstart gui/$(id -u)/$BACKUP_LABEL"
echo "Logs: $LOGS/backup.log and backup.err. Remove with: bash $0 --uninstall"
