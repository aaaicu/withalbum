# Security Policy (prototype)

WithAlbum Server is **experimental**. Accounts, device sessions, album access control, backups and a TLS-gated LAN mode exist and are tested, but the code has not had an external review or real-device testing. Do not store unique, irreplaceable or private photos only on this server, and **never expose it to the internet** (no port forwarding, no public reverse proxy). The only supported routes from outside the home are the end-to-end encrypted relay and the Tailscale Funnel TCP route below, both of which keep the pinned TLS session end to end and serve app routes only.

- This repository does not yet offer a formal security disclosure channel. Configure GitHub private vulnerability reporting before relying on it.

## Network exposure (M15)

| Listen address (`WITHALBUM_LISTEN`) | Allowed when |
|---|---|
| loopback (`127.0.0.1`, `::1`, `localhost`) — default `127.0.0.1:8787` | always; plain HTTP or `-tls` |
| private LAN address (RFC 1918 IPv4, `fc00::/7` IPv6) or the wildcard `0.0.0.0` / `[::]` / `:port` | **all of** `-allow-lan`, `-tls`, and at least one active `SERVER_ADMIN` whose password is usable (`admin init` done) |
| public IP addresses, host names | never |

Every missing condition is named and the server refuses to start; nothing is listened on. Plain HTTP is therefore only ever reachable from the machine itself. A wildcard listener also binds interfaces that may be reachable from outside (for example a global IPv6 address); the server logs a warning, and the router must not forward the port.

On the plain loopback listener, requests whose `Host` is not `localhost` or a loopback IP are answered `421 invalid_host` (DNS-rebinding protection).

### TLS identity and pinning (decision D13)

- With `-tls` the server creates, on first use, an ECDSA P-256 key and a self-signed certificate in `WITHALBUM_ROOT/.system/tls/server.pem` (one file, mode `0600`, directory `0700`, written atomically). A file that is not a regular `0600` file, or cannot be parsed, is refused — it is never silently replaced, because a new key breaks every paired device.
- Devices do **not** trust the certificate through a CA or check the host name. They pin the **SHA-256 of the certificate's SubjectPublicKeyInfo** (SPKI). The pin and a pairing payload `withalbum://pair?v=1&url=<https URL>&spki_sha256=<hex>` are printed at startup and by `withalbum-server tls` (safe while the server runs), and shown to a `SERVER_ADMIN` in the dashboard (`GET /api/v1/admin/server/tls`). Compare the fingerprint with the console output before pairing; a page reached through an intercepted connection could show a forged one.
- The certificate is valid 820 days (Apple platforms reject longer server certificates) and is re-issued **with the same key** 60 days before it expires or when the listen IP is not in it, so the pin stays the same.
- TLS 1.3 only (all its cipher suites are AEAD with forward secrecy). No client certificates.
- The private key is never logged or printed; only fingerprints are.
- Browsers show a certificate warning for a self-signed certificate. Open the dashboard on the server machine (`https://localhost:<port>` or a plain loopback instance) where possible.
- Rotating the key: stop the server, move `.system/tls/server.pem` away, start with `-tls`, and pair every device again.
- Backups (`backup -to`) do not contain the TLS key. After `restore`, the old `.system` (with `tls/`) is kept as `.system.before-restore-<UTC>`; move its `tls/server.pem` back before starting to keep the pairing, or pair devices again (restore signs every device out anyway).
- curl example: `curl -k --pinnedpubkey 'sha256//<base64 pin>' https://<lan-ip>:8787/api/v1/health` (`-k` only skips CA/host-name checks; the pin is still enforced).

### Relay: access from outside the home (M35)

Off by default. With `-relay host:port` (or `WITHALBUM_RELAY`) the server **dials out** to a relay (`withalbum-relay`, public, AGPL) and keeps that one connection; it opens no inbound port. Apps connect to the relay and run the **same pinned TLS 1.3 session** with the server through it, so the relay forwards ciphertext only and never holds the server key. Full threat model, metadata list and protocol: [`docs/RELAY_DESIGN.md`](docs/RELAY_DESIGN.md).

- Requires `-tls` and an administrator who can log in (same gate as a LAN listener); `-allow-lan` is not needed.
- The server proves its TLS key to the relay with a signature bound to the outer TLS connection (exporter); the rendezvous ID is derived from the key's pin, so no one can register under another server's ID, and a malicious relay cannot impersonate the server (the app's pin check fails).
- **Enabling the relay makes the login endpoint reachable from the internet** for anyone who knows the rendezvous ID (it is in the pairing QR and derived from the pin; it is not a secret). Use strong passwords; login limits apply per app IP (as reported by the relay), per account and IP, and per account.
- Through the relay only app routes are served: `GET /` (dashboard) and `/api/v1/admin/*` answer `403 not_available_via_relay`, also for administrators. Administer on the Mac or the home network.
- The relay operator can see: rendezvous IDs, IP addresses, connection times and durations, byte counts per connection, the inner TLS ClientHello. Never content. The relay logs no IPs unless started with `-log-ips`, and keeps nothing on disk.
- Relay abuse limits: connections and new connections per IP, tunnels per IP and in total, streams and new streams per server, bandwidth per server, handshake and idle timeouts; strict framing (fuzzed) that closes a tunnel on any violation.
- The connection to the relay itself is TLS 1.3 verified against the system roots (or `WITHALBUM_RELAY_CA_FILE`).

### Public route through Tailscale Funnel (M47)

Off by default. With `-tls -funnel-url https://<machine>.<tailnet>.ts.net` (or `WITHALBUM_FUNNEL_URL`) the server opens a **second listener on loopback only** (`-funnel-listen`, default `127.0.0.1:8788`; a non-loopback address is refused, also after binding) for a Tailscale Funnel **raw TCP forwarder** (`tailscale funnel --bg --tcp=443 --proxy-protocol=2 tcp://127.0.0.1:8788`). Details, sources and the app contract: [`docs/FUNNEL.md`](docs/FUNNEL.md).

- **TLS passes through end to end.** Funnel's relays route by SNI without decrypting, and tailscaled forwards the bytes; the app runs the same pinned TLS 1.3 session with the server's own key as on the LAN. Neither Tailscale's servers nor tailscaled on the Mac can read or change it, and a wrong key fails the pin. A TLS-terminating Funnel (`tailscale funnel 8788`, `--https`, `--tls-terminated-tcp`) is **not supported**: tailscaled on the same Mac would then decrypt and apps would have to trust a CA-issued ts.net certificate instead of the pin; such a setup fails closed because the pin does not match.
- **The login endpoint becomes reachable from the internet** for anyone who knows the URL (it is in the pairing QR; not a secret). Only app routes are served (same table as the relay); the dashboard, the setup screen and `/api/v1/admin/*` answer `403 not_available_via_relay` with `"route":"funnel"`, also for administrators.
- Login limits: per client IP from the optional PROXY v1/v2 header, else one shared key `funnel-unknown` for the whole route; per-account limits always. The header is accepted only on the loopback listener and only used as a rate-limit/audit key. Forwarding headers are never read.
- Exposure limits on the funnel listener: 128 connections at once, 10 s for the PROXY header and the TLS handshake, JSON bodies 8 KiB within 30 s (all listeners), 64 KiB headers. Audit entries of logins, refreshes, logouts and device revocations carry `"route":"funnel"` or `"relay"`.
- Lost device: `POST /api/v1/me/devices/revoke-others` signs out every other device of the account (served on every route).
- Tailscale sees client IPs, the ts.net name, times and byte counts. Funnel is a beta service with unpublished bandwidth limits.
- Funnel does not protect the rest of the home network: isolate the Mac from cameras and home automation ([`docs/HOME_NETWORK_ISOLATION.md`](docs/HOME_NETWORK_ISOLATION.md)); the menu bar app has a connection check for typed addresses.

### Response hardening

Every response carries `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY`, `Referrer-Policy: no-referrer`, `Cross-Origin-Resource-Policy: same-origin` and `Content-Security-Policy: default-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'` (the dashboard page sets its own: its single inline script and single inline style are allowed by SHA-256 hash only — no `'unsafe-inline'` —, `connect-src 'self'`, `img-src blob:` (album preview tiles the script fetched through the session API; no image is loaded by URL), `form-action 'none'`, `frame-ancestors 'none'`, and `require-trusted-types-for 'script'`; the page writes the DOM with `textContent` only; the setup page is the same with `img-src 'none'`). Downloaded originals additionally get `sandbox` and `Content-Disposition: attachment`, so a stored HTML or SVG file never runs as a page of the server origin.

`Strict-Transport-Security: max-age=31536000` (no `includeSubDomains`, no `preload`) is sent only over TLS and only when `Host` is a DNS name other than `localhost`: browsers ignore HSTS for IP addresses and on connections with certificate errors, and HSTS for `localhost` would force HTTPS on every other local server of the machine.

Cookies are `Secure` automatically on TLS requests (`HttpOnly` and `SameSite=Strict` always).

## External preview tools (M22)

HEIC and other still images are converted with macOS `sips`, videos with
`ffmpeg`, only if they are installed (`WITHALBUM_PREVIEW_TOOLS=off`
disables both). Uploaded files are untrusted input to these tools, so:

- they run directly with an argument list, never through a shell, with a
  minimal environment (no inherited secrets), no stdin, bounded captured
  output, a 90 s timeout and their own process group (killed as a whole);
- they read the original from the pool, where it is mode 0400; its size,
  mode and modification time are compared before and after each run;
- they write only into a fresh directory under `.system/work`, removed
  afterwards and at startup;
- their output is accepted only if it decodes as a JPEG of sane size and
  dimensions, and the stored preview is re-encoded by the server;
- `ffmpeg` may only open local files (`-protocol_whitelist file`) with the
  usual container demuxers (`-format_whitelist`), so a playlist or concat
  file disguised as a movie cannot make it read other files or the network;
  it runs single-threaded with a 256 MiB allocation cap. Go cannot cap a
  child's total memory without cgo or a shell; conversions run one at a
  time and still images over 256 MiB are not converted.

Keep the tools updated; decoder vulnerabilities in them are outside the
server's control. Without the tools nothing is lost: the original is stored
and its preview reported as "unsupported".

## Live Photo bundles (M26): threat model

A Live Photo is uploaded as two originals (a HEIC/JPEG still and a
QuickTime video) through an upload bundle and committed as one asset.

- **Parsing untrusted files.** The server reads only the Apple content
  identifier: from the still's EXIF MakerNote and from the movie's
  `moov/meta` keys. The containers are walked by `internal/capturetime`
  with its fixed limits (at most 1 MiB read in a bounded number of reads,
  counted boxes, segments and items, metadata blocks cut at 256 KiB, the
  movie data box skipped by seeking); `internal/livephoto` then walks at
  most 1024 directory entries with checked offsets and accepts an
  identifier of at most 64 printable ASCII characters. Nothing is allocated
  by a size read from the file. Both parsers have fuzz tests. Pure Go, no
  external tool.
- **No partial visibility.** Neither original becomes an asset, event or
  receipt until both are hash-verified, in the pool, and committed in one
  transaction that re-checks the caller's permission and the volume.
- **No overwrite.** An existing original is never replaced: the same still
  with another video is refused (409) for readers of the folder.
- **No oracle for upload-only members.** An UPLOADER gets the same 202
  receipt (echoing only what it sent) whether the Live Photo was new,
  attached to an existing photo, already present or conflicting (then it is
  stored as an asset of its own), so it learns nothing about the folder.
  The pairing check depends only on the uploaded bytes. Bundles, like
  upload sessions, are visible only to the account, device and folder that
  created them, and every byte of both components must be sent.
- **Attach only within the folder.** Attaching a video to an existing
  photo changes only an asset that no other folder shares.
- Downloads of the video (`.../components/paired_video`) have the same
  access checks and headers (attachment, sandbox CSP, no-store) as
  `/original`.

## Video duration and idempotent folder creation (M31)

- **Parsing untrusted files.** The playing time of a movie is read by
  `internal/capturetime.Duration` from `moov/mvhd` (and `moov/mvex/mehd`
  for fragmented movies) with the same limits as the capture time: at most
  1 MiB in a bounded number of reads, counted boxes, the movie data box
  skipped by seeking, nothing allocated by a size read from the file. The
  value is computed without overflow and a length over one week is
  discarded. Pure Go, fuzz-tested (`FuzzDuration`), read after the commit
  by the capture worker, never on the upload path.
- **Same visibility as the capture time.** `duration_ms` is part of the
  asset only for readers of the folder; an UPLOADER still gets only its
  202 receipt.
- **Folder creation keys.** An `Idempotency-Key` on `POST /folders` is
  scoped to the account and the session's device; another account's key
  never matches. A retry is authorized exactly like a new request and
  answers with the folder as the caller sees it now (404 once it is
  archived), so a stored key never grants access the caller lost.

## Photo and video location (M46)

A location says where someone was, often where they live. It is treated
as sensitive personal data.

- **Parsing untrusted files.** `internal/capturetime.ExtractLocation`
  reads the EXIF GPS directory (JPEG, TIFF, HEIF/HEIC/AVIF) and the
  QuickTime/MP4 ISO 6709 location (`com.apple.quicktime.location.ISO6709`,
  `udta/©xyz`) with the limits of the capture time (1 MiB, counted reads,
  boxes and entries, the movie data skipped). Pure Go, fuzz-tested
  (`FuzzExtractLocation`, `FuzzISO6709`), run by the capture worker after
  the commit, never on the upload path. Out-of-range values, zero
  denominators, a void GPS status, other datums and 0°, 0° count as no
  location. Altitude, heading and speed are not read or stored.
- **Who sees it.** `location` is part of the asset only for readers of the
  album (OWNER, CONTRIBUTOR, VIEWER). An UPLOADER gets only its 202
  receipt, as before; an outsider and a server administrator who is not a
  member get 404. The album owner can hide locations from members
  (`PATCH /api/v1/folders/{id}` `{"hide_location": true}`, audited
  `folder.set_hide_location`): members then get `null`, the owner still
  sees them. Each account can also hide the location of the photos it
  uploaded from everyone else, in every album (`PATCH /api/v1/me/settings`
  `{"hide_my_location": true}`, audited `user.set_hide_my_location`); the
  album owner does not override it. Both settings are re-read for every
  request.
- **What hiding does not do.** Originals are kept byte for byte (the
  integrity rule), GPS tags included. Anyone allowed to download an
  original can read its location from the file, whatever the setting. To
  share photos without a location, remove it on the device before
  uploading. Previews are re-encoded from decoded pixels by the server
  (`writeJPEGPreview`, also after sips/ffmpeg) and carry no EXIF; this is
  tested with geotagged sources and a converter that copies metadata.
- **Server logs and the relay** never contain coordinates: the worker
  logs only hashes and errors, and the relay only sees encrypted bytes.
- **Map tiles** are fetched by the app, not the server; see the app's
  privacy notice (tile requests reveal the viewed area, never photos).

## Setup screen (M24): threat model

The browser setup screen lets the person at the Mac pick the storage disk and create the first administrator without typing console commands. It runs **before any account exists**, so it is the most exposed moment of the server's life. Design and threat model, written before the implementation:

**What it protects.** Who becomes the first administrator (and therefore owns every folder of an existing, unclaimed storage); which directory receives the originals; the setup secrets themselves.

**Who could attack it, and how.**

| Attacker | Attack | Defence |
|---|---|---|
| Device on the LAN or internet | Reach the setup routes and claim the administrator first | Setup mode exists only on the **plain loopback listener** (`127.0.0.1`/`::1`/`localhost`, no `-tls`, no `-allow-lan`). The server refuses to start setup mode on any other listen configuration; LAN and TLS listeners never get a setup route (they require an administrator anyway, M15). The bound address is re-checked after `listen` (`checkBoundAddr`). |
| Web page in the user's browser (another site) | Cross-site requests to `127.0.0.1` (CSRF), or DNS rebinding of its own name to `127.0.0.1` | `Host` must be loopback (`421 invalid_host`, same as normal mode); every setup API call needs `Content-Type: application/json` (no CORS preflight is ever answered), a same-origin `Origin`/`Sec-Fetch-Site` when the browser sends them, and after unlocking a per-session CSRF token header. The session cookie is `HttpOnly`, `SameSite=Strict`, `Path=/setup`. |
| Another local account on the same Mac, a browser extension, a local malicious page or process | Talk to the loopback port directly | A **one-time setup code** printed only on the console that started the server: 12 characters from a 32-letter alphabet (60 bits, `crypto/rand`), kept only in memory as a SHA-256, compared in constant time, valid **30 minutes**, **single use** (the first correct entry consumes it and opens one setup session), and **5 wrong entries disable it** until the server is restarted (a new code is printed then). Nothing about the machine — not even the disk list — is answered before the code is entered. |
| Script injection into the setup page | Run script in the page that holds the session | Strict CSP: the single inline script and style are allowed by SHA-256 hash only, `default-src 'none'`, `connect-src 'self'`, `img-src 'none'`, `form-action 'none'`, `frame-ancestors 'none'`, Trusted Types required; the page writes the DOM with `textContent` only. Paths and volume names are data, never markup. |
| Wrong disk (M1) | Data written to the internal disk because an external disk was unplugged | The server never creates a storage root on its own. The setup screen only registers a directory the user picked: an existing directory, or one new `WithAlbum` folder directly under a disk the user explicitly selected — and only after re-checking that the disk is still mounted (its device differs from `/Volumes`). Disks under `/Volumes` that are not mounted are shown but cannot be chosen. Registration uses the same `storage.Init` / `storage.Open` volume checks as the console, and the chosen root and volume id are pinned in the per-user config file (below). |
| Copied marker / returning disk (M37) | A copy of the volume marker (or of the whole library) on another disk is opened as the library; or a disk that was detached comes back and the running server resumes writing through database handles and an instance lock that belonged to the old mount | The marker records the disk UUID (macOS `getattrlist` volume UUID, Linux `/dev/disk/by-uuid`); a different or unreadable UUID refuses to start with a `SECURITY` error until an operator runs `-adopt-disk`. Older markers adopt the UUID of their disk on the first start (under the instance lock, logged). Once a write-time volume check fails, the running server keeps refusing (503 `storage_unavailable`, health `unavailable`) until it is restarted. Tested on hdiutil disk images (`docs/CHAOS_RESULTS.md`), not on real external disks. |
| Long-running exposure | Setup mode left open | Setup routes exist only while the server is in setup mode. When the administrator is created, the same process switches to normal mode and the setup routes are gone (`404`); a restarted server never enters setup mode again for that storage. |

**Information shown.** After the code is entered: candidate directories with their path, file system type, free/total space, and whether they hold WithAlbum data. Paths are acceptable here because only the person at the console can unlock the screen and only over loopback; nothing of this is reachable in normal mode.

**Where the choice is kept.** Without `WITHALBUM_ROOT`, the chosen storage root and its volume id are written to a per-user config file (`WITHALBUM_CONFIG`, default `os.UserConfigDir()/WithAlbum/server.json`, mode `0600`, directory `0700`, written atomically). Later starts and the console commands use it; `WITHALBUM_ROOT` always takes precedence. A root named in that file that is missing or unregistered stops the server (fail closed) instead of entering setup mode, because it means the registered disk is not there.

**Switching to normal mode without a restart.** Setup mode and normal mode use the same plain loopback listener. Registering the storage runs the normal startup sequence (`openApp`: volume check, instance lock, metadata, reconciliation), and the lock is held from then on, so no console command can change the storage meanwhile. Creating the administrator uses the same `ClaimBootstrap` transaction as `admin init` (audit rows `account.bootstrap_claim` and `setup.complete`, `via=setup`); only then is the request handler atomically replaced by the normal API handler built from that same opened storage. No setup state survives the switch.

**Accepted risks.** Anyone who can read the server's console output (the terminal, or the LaunchAgent log file) can complete setup within the code's lifetime; protect that log like the console. A local process can burn the code with 5 wrong guesses (denial of service until restart). The plain loopback listener is not encrypted; other local users cannot read loopback traffic of another user's browser on macOS, but a local administrator (root) can.

## Administrator accounts and recovery (console only)

The first administrator can be created at the server console or, on the machine itself, with the setup screen above; never over the network:

```bash
WITHALBUM_ROOT=/path/to/root withalbum-server admin init -login <name>
```

`admin init` takes over the locked bootstrap account (`admin`) that owns every folder created before accounts existed, keeping its user id, so those folders keep their owner. It works once; a second run is refused without changes. Passwords are Argon2id hashes (PHC string with its parameters); they are typed twice on the terminal without echo, or read as one line from stdin when stdin is not a terminal. They are never accepted as command-line arguments and never printed or logged. Avoid passing a password through `echo`/`printf` in an interactive shell, because shells may keep it in their history; use the terminal prompt or a file with restricted permissions.

**Lost administrator password:** stop the server (the admin commands need the storage lock and refuse with `LOCKED` while it runs), then at the server console run

```bash
WITHALBUM_ROOT=/path/to/root withalbum-server admin reset-password -login <name>
```

Anyone with console access to the server and write access to the storage can do this; protect the machine and its user account accordingly. Every account change is recorded in the `audit_log` table (without passwords or hashes). A reset signs out every device of the account (all its sessions are revoked in the same transaction); disabling an account does the same.

## v1.4 계정/앨범 권한 설계 상태

`internal/authz`의 정책은 HTTP 서버의 모든 관리자·앨범 라우트에서 매 요청 적용됩니다(`internal/httpapi/middleware_authz.go`). 최초 관리자는 콘솔 명령(`admin init`)으로만 만들 수 있고, 대시보드 페이지·health·login·refresh 외 모든 API는 세션이 필요합니다. 서버 관리자도 소유·멤버가 아닌 앨범의 데이터는 읽을 수 없습니다(404). 초대·역할 변경·추방·소유권 이전은 앨범 OWNER만 할 수 있고 같은 트랜잭션에서 감사 로그에 남습니다. LAN 공개는 위 조건(TLS + 관리자 + 명시적 플래그)에서만 가능하며, 인터넷 공개나 실사용 원본의 유일 저장소로 사용하면 안 됩니다.

최종 정책은 [ACCESS_CONTROL_v1.4.md](docs/ACCESS_CONTROL_v1.4.md) 참조.

## Security review (M15, 2026-10)

Scope: the change that allows a LAN listener, and the whole unauthenticated and authenticated surface it exposes. Method: code reading of every route and middleware, plus tests (`server/listen_test.go`, `internal/tlsid`, `internal/httpapi/security_test.go`, `authz_test.go`) and a binary smoke test over a real private LAN address with a pinned client.

| # | Area | Finding | Risk | Status |
|---|---|---|---|---|
| 1 | Downloads | Originals were served with the Content-Type sniffed at upload; an uploaded HTML (or XHTML/SVG-as-XML) file opened by URL ran as a page of the server origin, next to the readable CSRF cookie and the session cookies (stored XSS by any CONTRIBUTOR). | High | **Fixed:** `Content-Security-Policy: …; sandbox` + `Content-Disposition: attachment` on originals; test `TestStoredHTMLIsNotServedAsDocument`. |
| 2 | Transport | Non-loopback listen must not be possible without TLS, a usable administrator and an explicit flag; public addresses and names must be refused. | High | **Implemented:** `checkListen` (table test `TestCheckListen`), `listenAllowed` (`TestListenAllowedNeedsUsableAdmin`), smoke test of every refusal. |
| 3 | Headers | No frame, referrer or CSP protection on API responses. | Medium | **Fixed:** `securityHeaders` on every response; HSTS policy as above. |
| 4 | Rate limiting | The per-IP login limit keyed on the full IPv6 address: one LAN host can rotate through its /64 (privacy addresses) for a fresh budget. | Medium | **Fixed:** IPv6 keys use the /64 prefix (`rateKey`); the per-account limit was already independent of the IP. Forwarding headers stay untrusted (TCP peer only). |
| 5 | Preview decoder | Concurrent uploads each decoded an image of up to 40 MP (16-bit PNG: ~320 MiB) at the same time: memory exhaustion by an authenticated uploader on small devices. | Medium | **Mitigated:** previews are decoded one at a time. The 40 MP cap stays; a streaming decoder / lower cap for Pi is follow-up. |
| 6 | DNS rebinding | A web page could re-point its own name at 127.0.0.1 and talk to the plain loopback listener (login attempts, health). | Medium | **Fixed:** `421 invalid_host` unless Host is loopback (plain listener). On TLS the pin/certificate check of browsers blocks it. |
| 7 | Limits | `MaxHeaderBytes` was 1 MiB. | Low | **Fixed:** 64 KiB. `ReadHeaderTimeout` 10 s (also bounds the TLS handshake), `IdleTimeout` 60 s. No `ReadTimeout`/`WriteTimeout` on purpose (multi-minute uploads/downloads over Wi-Fi); bodies are only read after authentication, upload size is capped at 512 MiB and by the free-space reserve. |
| 8 | Health | Unauthenticated `GET /api/v1/health` returns `status`, `version`, `storage` (ok/unavailable) and `integrity` (ok/warning) — no paths, volume ids or counts. | Low | **Accepted:** the app needs version and state before login; the booleans reveal no data. |
| 9 | Cookies | `Secure` is set from `r.TLS`; verified over TLS (test and smoke). `__Host-` prefixes are not used. | Low | Accepted; prefix rename would break existing browser sessions only, follow-up. |
| 10 | Logs | `http: TLS handshake error from <ip>` lines from LAN scanners are logged without rate limit. | Low | Accepted (no secrets in them). |
| 11 | Connections | No cap on concurrent connections per client. | Low | Deferred (home LAN; Argon2id work is already bounded to 2 concurrent hashes, 503 busy beyond). |
| 12 | Auth/session/CSRF | Re-checked: 256-bit opaque tokens stored as SHA-256, constant-time compare, refresh rotation with reuse detection, account re-read per request, CSRF token required for cookie-authenticated unsafe methods, JSON content type required (no CORS preflight answered), dummy hash against account enumeration. | — | No change needed. |
| 13 | Routes | Re-checked all 33 declared routes: 4 public (dashboard page, health, login, refresh), every other path under `/api/v1/` including unknown ones is 401 without a session — over TLS too (`TestLANOverPinnedTLS`, smoke sweep of 30 paths). New `GET /api/v1/admin/server/tls` is SERVER_ADMIN only and returns fingerprints only. | — | Verified. |
| 14 | File paths | Pool/preview paths derive only from SHA-256 values computed by the server or validated as 64 lowercase hex on import; ids in URLs are only database keys; upload filenames are reduced to a base name and only stored/echoed (now RFC 2231-encoded in Content-Disposition). | — | No change needed. |
| 15 | Error messages | Unexpected errors are logged and answered with a fixed code (`internal_error`, `metadata_read_failed`, …); no paths, SQL or stack traces reach clients. The password policy message names only the rule. | — | No change needed. |

Not covered: external penetration test, fuzzing of the image decoders, real-device iOS/Android pinning implementation, behavior behind a reverse proxy (not supported; `-trust-proxy` is a follow-up).
