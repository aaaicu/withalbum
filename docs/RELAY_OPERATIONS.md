# Running a relay (self-hosting `withalbum-relay`)

The relay lets apps reach a home server that opens no port (design and
threat model: [`RELAY_DESIGN.md`](RELAY_DESIGN.md)). It forwards encrypted
bytes only, keeps nothing on disk and logs no content. Status:
**experimental**, tested in Go, not yet run on a public host with real
phones.

## What you need

- A small Linux (or macOS) host with a **public IP** and **TCP port 443**
  open to the internet. CPU and memory needs are small; bandwidth is what
  you pay for: every photo uploaded or downloaded through the relay crosses
  it twice (in and out).
- A **DNS name** for it, e.g. `relay.example.org` (A/AAAA record to the host).
- A **certificate** for that name from a public CA (Let's Encrypt via
  certbot, lego, acme.sh, …), as PEM files.
- Go 1.26+ to build (no release archive yet).

## Build

```sh
git clone https://github.com/aaaicu/withalbum && cd withalbum
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(git describe --tags --always)" -o withalbum-relay ./relay
# cross-build for a Raspberry Pi / ARM VPS:
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -o withalbum-relay ./relay
```

## Certificate

Example with certbot in standalone mode (needs port 80 free during issuance
and renewal) — any ACME client that writes PEM files works:

```sh
sudo certbot certonly --standalone -d relay.example.org
# files: /etc/letsencrypt/live/relay.example.org/{fullchain,privkey}.pem
```

The relay re-reads `-cert`/`-key` at most once a minute when their
modification time changes, so renewals need no restart. Give the relay user
read access to the key (e.g. a deploy hook that copies both files to
`/etc/withalbum-relay/` with owner `withalbum-relay`, mode 0600).

## Run

```sh
withalbum-relay -listen :443 \
  -cert /etc/withalbum-relay/fullchain.pem -key /etc/withalbum-relay/privkey.pem \
  -health 127.0.0.1:9443
curl -s http://127.0.0.1:9443/healthz   # {"status":"ok","tunnels":0,"streams":0,"connections":0,"draining":false}
```

Every flag also has an environment variable for the main settings
(`WITHALBUM_RELAY_LISTEN`, `WITHALBUM_RELAY_HEALTH`, `WITHALBUM_RELAY_CERT`,
`WITHALBUM_RELAY_KEY`). `-health` must be a loopback or private address
(it reports counts only, but has no authentication).

### Limits (defaults)

| Flag | Default | Meaning |
|---|---|---|
| `-max-conns` | 4096 | connections at once, all kinds |
| `-max-conns-per-ip` | 128 | connections at once from one IP (IPv6: per /64) |
| `-conn-rate`, `-conn-burst` | 5/s, 60 | new connections per IP |
| `-max-tunnels` | 2000 | home servers connected at once |
| `-max-tunnels-per-ip` | 16 | home servers behind one IP (carrier NAT shares addresses) |
| `-max-streams` | 32 | app connections to one home server at once |
| `-stream-rate`, `-stream-burst` | 5/s, 40 | new app connections to one home server |
| `-bandwidth` | 8388608 (8 MiB/s) | bytes per second through one home server, each direction; `-1` unlimited |
| `-idle-timeout` | 5m | close an app connection that moves no byte |
| `-drain` | 30s | on SIGTERM, time for running connections to finish |

Fixed: TLS 1.3 only; handshake must finish within 10 s; a tunnel silent for
75 s is closed (home servers ping every 20 s).

### Logs

JSON lines on stderr: `relay_start`, `tunnel_up`, `tunnel_down`,
`tunnel_replaced`, `stream_end` (rid, duration, bytes each way, reason),
`connect_refused`/`serve_refused`/`conn_refused` (reason), `relay_draining`,
`relay_stop`. Client IPs are **not** logged unless `-log-ips` is given; if
you enable it for abuse handling, say so in your privacy notice and rotate
the logs. Log level: `-log-level debug|info|warn|error`.

## systemd (Linux)

`/etc/systemd/system/withalbum-relay.service`:

```ini
[Unit]
Description=WithAlbum relay
After=network-online.target
Wants=network-online.target

[Service]
User=withalbum-relay
ExecStart=/usr/local/bin/withalbum-relay -listen :443 -health 127.0.0.1:9443 \
  -cert /etc/withalbum-relay/fullchain.pem -key /etc/withalbum-relay/privkey.pem
Restart=always
RestartSec=2
KillSignal=SIGTERM
TimeoutStopSec=40
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
PrivateDevices=true
ProtectKernelTunables=true
ProtectControlGroups=true
RestrictAddressFamilies=AF_INET AF_INET6
LimitNOFILE=16384

[Install]
WantedBy=multi-user.target
```

```sh
sudo useradd --system --no-create-home --shell /usr/sbin/nologin withalbum-relay
sudo systemctl daemon-reload && sudo systemctl enable --now withalbum-relay
journalctl -u withalbum-relay -f
```

## launchd (macOS host)

`/Library/LaunchDaemons/org.withalbum.relay.plist` (port 443 needs a
LaunchDaemon, i.e. root; or listen on a high port and forward 443 to it in
the firewall):

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>org.withalbum.relay</string>
  <key>ProgramArguments</key><array>
    <string>/usr/local/bin/withalbum-relay</string>
    <string>-listen</string><string>:443</string>
    <string>-health</string><string>127.0.0.1:9443</string>
    <string>-cert</string><string>/usr/local/etc/withalbum-relay/fullchain.pem</string>
    <string>-key</string><string>/usr/local/etc/withalbum-relay/privkey.pem</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardErrorPath</key><string>/var/log/withalbum-relay.log</string>
</dict></plist>
```

## Connect a home server

On the home server (which must already run with `-tls` and have an
administrator):

```sh
WITHALBUM_RELAY=relay.example.org:443 withalbum-server -tls            # loopback + relay
WITHALBUM_RELAY=relay.example.org:443 withalbum-server -tls -allow-lan  # LAN + relay
withalbum-server tls -relay relay.example.org:443                      # pairing payloads with the relay
```

The server logs `relay: connected to … (rendezvous id …)`; the relay logs
`tunnel_up` with the same ID. Pair phones again with the new payload (or let
the app read the relay from `GET /api/v1/me` while at home).

A relay with a certificate from a **private CA**: set
`WITHALBUM_RELAY_CA_FILE=/path/ca.pem` on the home server. Apps trust only
the system roots, so for phones use a public CA.

## Operator checklist

- [ ] Port 443/TCP reachable from a mobile network (`openssl s_client -connect relay.example.org:443 -tls1_3 -alpn withalbum-relay/1`).
- [ ] Certificate renewal tested (`certbot renew --dry-run`), files readable by the service user.
- [ ] `curl http://127.0.0.1:9443/healthz` from the host only; port 9443 not exposed.
- [ ] Bandwidth limit and provider traffic quota match; monitor `/healthz` counts.
- [ ] Privacy notice for users lists the metadata in RELAY_DESIGN.md.
- [ ] Update: replace the binary and `systemctl restart` (running transfers get `-drain` seconds; apps resume uploads).
