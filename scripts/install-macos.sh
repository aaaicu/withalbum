#!/bin/bash
# Developer-only install to user's LaunchAgents (NOT a signed DMG).
set -euo pipefail
if [[ "$(uname -s)" != Darwin ]]; then echo 'macOS only' >&2; exit 1; fi
if [[ $# -ne 2 ]]; then echo 'Usage: bash install-macos.sh /path/to/withalbum-server /existing/storage/root' >&2; exit 2; fi
BINARY="$1"
ROOT="$2"
[[ -f "$BINARY" && -x "$BINARY" ]] || { echo 'Expected an executable server binary' >&2; exit 2; }
[[ "$ROOT" == /* && -d "$ROOT" && -w "$ROOT" ]] || { echo 'Storage root must be an existing writable absolute directory' >&2; exit 2; }
[[ -f "$ROOT/.withalbum-volume.json" ]] || echo "WARNING: $ROOT is not initialized; the server will refuse to start until you run: WITHALBUM_ROOT=\"$ROOT\" $BINARY -init-storage" >&2
[[ "$ROOT" != *$'\n'* && "$ROOT" != *$'\r'* ]] || { echo 'Unsupported newline in path' >&2; exit 2; }
BIN_DIR="$HOME/.local/share/withalbum-server"
PLIST_DIR="$HOME/Library/LaunchAgents"
PLIST="$PLIST_DIR/org.withalbum.server.plist"
LOG_DIR="$HOME/Library/Logs/WithAlbum"
mkdir -p "$BIN_DIR" "$PLIST_DIR" "$LOG_DIR"
install -m 0755 "$BINARY" "$BIN_DIR/withalbum-server"
# Encode XML metacharacters in paths.
xml_escape() { printf '%s' "$1" | sed -e 's/\&/\&amp;/g' -e 's/</\&lt;/g' -e 's/>/\&gt;/g' -e 's/"/\&quot;/g' -e "s/'/\\\&apos;/g"; }
EXE_XML="$(xml_escape "$BIN_DIR/withalbum-server")"
ROOT_XML="$(xml_escape "$ROOT")"
LOG_XML="$(xml_escape "$LOG_DIR")"
cat > "$PLIST" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>org.withalbum.server</string>
  <key>ProgramArguments</key><array><string>${EXE_XML}</string></array>
  <key>EnvironmentVariables</key><dict>
    <key>WITHALBUM_ROOT</key><string>${ROOT_XML}</string>
    <key>WITHALBUM_LISTEN</key><string>127.0.0.1:8787</string>
  </dict>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>${LOG_XML}/server.log</string>
  <key>StandardErrorPath</key><string>${LOG_XML}/error.log</string>
</dict>
</plist>
PLIST
plutil -lint "$PLIST" >/dev/null
launchctl bootout "gui/$(id -u)" "$PLIST" 2>/dev/null || true
if launchctl bootstrap "gui/$(id -u)" "$PLIST"; then
  echo "Installed user LaunchAgent. Open http://127.0.0.1:8787"
  echo "If the server keeps exiting, initialize the storage root once: WITHALBUM_ROOT=\"$ROOT\" \"$BIN_DIR/withalbum-server\" -init-storage"
else
  echo "Installed but launchctl bootstrap failed. Log in to the graphical desktop and try 'launchctl bootstrap gui/$(id -u) $PLIST'" >&2
fi
