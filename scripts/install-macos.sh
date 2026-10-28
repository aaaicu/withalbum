#!/bin/bash
# Developer-only install to the user's LaunchAgents (NOT a signed DMG).
#
#   bash install-macos.sh [--lan ADDR:PORT] /path/to/withalbum-server [/storage/root]
#
# With a storage root (console path): the LaunchAgent pins it with
# WITHALBUM_ROOT. Initialize it once first:
#   WITHALBUM_ROOT=/storage/root ./withalbum-server -init-storage
# Without one (setup screen path): the server uses the storage registered by
# the setup screen in the per-user config file (WITHALBUM_CONFIG, default
# ~/Library/Application Support/WithAlbum/server.json). If nothing is
# registered yet, the agent starts in setup mode and writes the one-time
# setup code to the error log.
# --lan serves the LAN with -tls -allow-lan on ADDR:PORT (a private address
# of this Mac); it needs an administrator, so finish the setup first.
set -euo pipefail
usage() { echo 'Usage: bash install-macos.sh [--lan ADDR:PORT] /path/to/withalbum-server [/existing/storage/root]' >&2; exit 2; }
if [[ "$(uname -s)" != Darwin ]]; then echo 'macOS only' >&2; exit 1; fi
LAN=""
if [[ "${1:-}" == --lan ]]; then
  [[ $# -ge 2 ]] || usage
  LAN="$2"
  shift 2
fi
[[ $# -eq 1 || $# -eq 2 ]] || usage
BINARY="$1"
ROOT="${2:-}"
no_newline() { [[ "$1" != *$'\n'* && "$1" != *$'\r'* ]] || { echo 'Unsupported newline in argument' >&2; exit 2; }; }
no_newline "$BINARY"
no_newline "$ROOT"
no_newline "$LAN"
no_newline "${WITHALBUM_CONFIG:-}"
[[ -f "$BINARY" && -x "$BINARY" ]] || { echo 'Expected an executable server binary' >&2; exit 2; }
if [[ -n "$LAN" ]]; then
  lan_re='^[][0-9A-Fa-f.:]+:[0-9]+$'
  [[ "$LAN" =~ $lan_re ]] || { echo '--lan expects ADDR:PORT, for example 192.168.0.10:8787 (the server accepts only private addresses)' >&2; exit 2; }
fi

CONFIG="${WITHALBUM_CONFIG:-$HOME/Library/Application Support/WithAlbum/server.json}"
config_root() { plutil -extract storage_root raw -o - "$CONFIG" 2>/dev/null || true; }

if [[ -n "$ROOT" ]]; then
  [[ "$ROOT" == /* && -d "$ROOT" && -w "$ROOT" ]] || { echo 'Storage root must be an existing writable absolute directory' >&2; exit 2; }
  [[ -f "$ROOT/.withalbum-volume.json" ]] || echo "WARNING: $ROOT is not initialized; the server will refuse to start until you run: WITHALBUM_ROOT=\"$ROOT\" $BINARY -init-storage" >&2
  if [[ -f "$CONFIG" ]]; then
    registered="$(config_root)"
    if [[ -n "$registered" && "$registered" != "$ROOT" ]]; then
      echo "WARNING: the setup screen registered $registered in $CONFIG; this LaunchAgent uses $ROOT instead (WITHALBUM_ROOT wins)." >&2
    fi
  fi
else
  if [[ -f "$CONFIG" ]]; then
    registered="$(config_root)"
    [[ -n "$registered" ]] || { echo "$CONFIG is not a WithAlbum server config; fix or remove it, or pass a storage root" >&2; exit 2; }
    echo "Using the storage registered by the setup screen: $registered"
    [[ -d "$registered" ]] || echo "WARNING: $registered is not there now (disk not connected?). The server refuses to start until it is; it never creates it." >&2
  elif [[ -n "$LAN" ]]; then
    echo "--lan needs a registered storage and an administrator: run the setup screen (or -init-storage and admin init) first" >&2
    exit 2
  else
    echo "No storage registered yet: the server will start in setup mode on http://127.0.0.1:8787/setup (this Mac only)."
  fi
fi

BIN_DIR="$HOME/.local/share/withalbum-server"
PLIST_DIR="$HOME/Library/LaunchAgents"
PLIST="$PLIST_DIR/org.withalbum.server.plist"
LOG_DIR="$HOME/Library/Logs/WithAlbum"
mkdir -p "$BIN_DIR" "$PLIST_DIR" "$LOG_DIR"
# Logs can contain the one-time setup code: owner-only.
chmod 700 "$LOG_DIR"
install -m 0755 "$BINARY" "$BIN_DIR/withalbum-server"
# Encode XML metacharacters in paths.
xml_escape() { printf '%s' "$1" | sed -e 's/\&/\&amp;/g' -e 's/</\&lt;/g' -e 's/>/\&gt;/g' -e 's/"/\&quot;/g' -e "s/'/\\\&apos;/g"; }
EXE_XML="$(xml_escape "$BIN_DIR/withalbum-server")"
LOG_XML="$(xml_escape "$LOG_DIR")"
ARGS_XML="<string>${EXE_XML}</string>"
LISTEN="127.0.0.1:8787"
if [[ -n "$LAN" ]]; then
  ARGS_XML="${ARGS_XML}<string>-tls</string><string>-allow-lan</string>"
  LISTEN="$LAN"
fi
ENV_XML="    <key>WITHALBUM_LISTEN</key><string>$(xml_escape "$LISTEN")</string>"
if [[ -n "$ROOT" ]]; then
  ENV_XML="${ENV_XML}
    <key>WITHALBUM_ROOT</key><string>$(xml_escape "$ROOT")</string>"
fi
if [[ -n "${WITHALBUM_CONFIG:-}" ]]; then
  ENV_XML="${ENV_XML}
    <key>WITHALBUM_CONFIG</key><string>$(xml_escape "$WITHALBUM_CONFIG")</string>"
fi
cat > "$PLIST" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>org.withalbum.server</string>
  <key>ProgramArguments</key><array>${ARGS_XML}</array>
  <key>EnvironmentVariables</key><dict>
${ENV_XML}
  </dict>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ThrottleInterval</key><integer>30</integer>
  <key>StandardOutPath</key><string>${LOG_XML}/server.log</string>
  <key>StandardErrorPath</key><string>${LOG_XML}/error.log</string>
</dict>
</plist>
PLIST
plutil -lint "$PLIST" >/dev/null
launchctl bootout "gui/$(id -u)" "$PLIST" 2>/dev/null || true
if launchctl bootstrap "gui/$(id -u)" "$PLIST"; then
  if [[ -n "$LAN" ]]; then
    echo "Installed user LaunchAgent serving https://$LAN (TLS, LAN). Key fingerprint: ${ROOT:+WITHALBUM_ROOT=\"$ROOT\" }WITHALBUM_LISTEN=$LAN \"$BIN_DIR/withalbum-server\" tls"
  else
    echo "Installed user LaunchAgent. Open http://127.0.0.1:8787 on this Mac"
  fi
  if [[ -z "$ROOT" && ! -f "$CONFIG" ]]; then
    echo "Setup code: grep \"SETUP: open\" \"$LOG_DIR/error.log\" | tail -1"
    echo "A locked or expired code needs a restart: launchctl kickstart -k gui/$(id -u)/org.withalbum.server"
  fi
  echo "Logs: $LOG_DIR. If the server keeps exiting, read error.log (missing disk, uninitialized root, LAN without an administrator)."
else
  echo "Installed but launchctl bootstrap failed. Log in to the graphical desktop and try 'launchctl bootstrap gui/$(id -u) $PLIST'" >&2
fi
