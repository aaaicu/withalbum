#!/usr/bin/env bash
# Developer-only systemd user service. Still localhost-only and unauthenticated.
set -euo pipefail
if [[ "$(uname -s)" != Linux ]]; then echo 'Linux only' >&2; exit 1; fi
if [[ $# -ne 2 ]]; then echo 'Usage: bash install-linux.sh /path/to/withalbum-server /existing/storage/root' >&2; exit 2; fi
BINARY="$1"
ROOT="$2"
[[ -f "$BINARY" && -x "$BINARY" ]] || { echo 'Expected an executable server binary' >&2; exit 2; }
[[ "$ROOT" == /* && -d "$ROOT" && -w "$ROOT" ]] || { echo 'Storage root must be an existing writable absolute directory' >&2; exit 2; }
[[ -f "$ROOT/.withalbum-volume.json" ]] || echo "WARNING: $ROOT is not initialized; the server will refuse to start until you run: WITHALBUM_ROOT=\"$ROOT\" $BINARY -init-storage" >&2
# This first installer intentionally restricts paths so systemd escaping cannot be bypassed.
[[ "$ROOT" =~ ^[a-zA-Z0-9_./-]+$ && "$HOME" =~ ^[a-zA-Z0-9_./-]+$ ]] || { echo 'Only simple paths without spaces or special characters are supported by this alpha installer' >&2; exit 2; }
command -v systemctl >/dev/null || { echo 'systemctl is required for this installer' >&2; exit 2; }
BIN_DIR="$HOME/.local/share/withalbum-server"
UNIT_DIR="$HOME/.config/systemd/user"
mkdir -p "$BIN_DIR" "$UNIT_DIR"
install -m 0755 "$BINARY" "$BIN_DIR/withalbum-server"
cat > "$UNIT_DIR/withalbum-server.service" <<SERVICE
[Unit]
Description=WithAlbum Server (experimental local-only)
After=default.target
[Service]
Type=simple
Environment=WITHALBUM_ROOT=$ROOT
Environment=WITHALBUM_LISTEN=127.0.0.1:8787
ExecStart=$BIN_DIR/withalbum-server
Restart=on-failure
RestartSec=10
NoNewPrivileges=true
PrivateTmp=true
[Install]
WantedBy=default.target
SERVICE
systemctl --user daemon-reload
systemctl --user enable --now withalbum-server.service
printf 'Installed systemd --user service: http://127.0.0.1:8787\n'
printf 'If the service exits, initialize the storage root once: WITHALBUM_ROOT=%s %s -init-storage\n' "$ROOT" "$BIN_DIR/withalbum-server"
