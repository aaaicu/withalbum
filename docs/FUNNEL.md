# Access from outside the home through Tailscale Funnel

Status: **implemented and tested with a simulated Funnel** (server funnel
listener, PROXY header, route classification, pairing fields, `/me`, the
macOS menu bar helper). **Not yet run against a real Tailscale Funnel** or
with a phone on a mobile network. No app implements the `public_url`
candidate yet (app contract below).

한국어 설치 안내는 [아래 "집 밖에서 접속 켜기 (사용자 안내)"](#집-밖에서-접속-켜기-사용자-안내)에 있습니다.

## Why Funnel

Family members reach the home server from anywhere **without installing a
VPN app**, **without a port forward** on the home router and **without a
rented server**: Tailscale Funnel gives the Mac a public HTTPS name
`https://<machine>.<tailnet>.ts.net` and carries connections to it over
Tailscale. Only the Mac needs a Tailscale account. The M35 relay
([`RELAY_DESIGN.md`](RELAY_DESIGN.md)) stays as a second route for those
who run one.

## What Tailscale's documentation says (checked 2026-10-25)

| Question | Answer | Source |
|---|---|---|
| Can Funnel pass TLS through to a local port without terminating it? | **Yes.** `tailscale funnel --tcp=<port> tcp://<host:port>` sets up a *raw TCP forwarder*; `--tls-terminated-tcp` is the variant that decrypts in tailscaled first. Tailscale's relay servers never terminate TLS: they "look at the SNI name in the TLS ClientHello, and then proxy those encrypted TCP connections to your Tailscale node". In tailscaled, a TCP handler without `TerminateTLS` forwards the bytes unchanged (`ipn/ipnlocal/serve.go`, `forwardTCPWithProxyProtocol`). | [funnel CLI](https://tailscale.com/docs/reference/tailscale-cli/funnel), [serve CLI](https://tailscale.com/kb/1242/tailscale-serve), [Funnel announcement](https://tailscale.com/blog/introducing-tailscale-funnel/), [Funnel feature page](https://tailscale.com/docs/features/tailscale-funnel) ("Funnel relay servers do not decrypt the traffic"), [`ipn/serve.go`](https://github.com/tailscale/tailscale/blob/main/ipn/serve.go) |
| Which SNI must the client send? | The node's MagicDNS name, `<machine>.<tailnet>.ts.net` (the relay routes on it; `AllowFunnel` is keyed by "SNI:port"). A client without SNI, or with another name, is not routed. | blog post above; `ipn.ServeConfig.AllowFunnel` comment |
| Ports | Public side **443, 8443, 10000** only. The local target can be any port. | feature page, CLI page |
| Client IP | Optional **PROXY protocol** v1/v2 header for TCP forwarders (`--proxy-protocol=2`). For HTTP proxying tailscaled sets `X-Forwarded-For` and `Tailscale-Funnel-Request`, but the server does not use HTTP proxying. | CLI page, `serve.go` |
| Bandwidth | "subject to non-configurable bandwidth limits"; no number is published. Measure. | feature page |
| Requirements | Tailscale ≥ 1.38.3, MagicDNS on, HTTPS certificates on, a `funnel` node attribute in the tailnet policy (`"nodeAttrs": [{"target": ["autogroup:member"], "attr": ["funnel"]}]`). Beta feature, all plans. | feature page |
| macOS variants | The variant comparison lists Funnel as **supported only by the open source `tailscaled`**, not by the Mac App Store or Standalone apps; the CLI page says port sharing works on all variants (only file serving needs the open source one). The two pages disagree: **verify on the real Mac**; the open source variant is the documented one (no GUI, no auto-update). | [macOS variants](https://tailscale.com/docs/concepts/macos-variants), CLI page |
| CLI paths on macOS | Standalone/App Store: `/Applications/Tailscale.app/Contents/MacOS/Tailscale` (or a link the app installs in `/usr/local/bin`); open source via Homebrew: `/opt/homebrew/bin/tailscale` (Apple silicon) or `/usr/local/bin/tailscale`. | [CLI reference](https://tailscale.com/kb/1080/cli) |
| Persistence | `--bg` keeps the forwarder across reboots and tailscaled restarts. `tailscale funnel --tcp=443 tcp://… off` removes it; `tailscale funnel reset` clears all. | CLI page |

Re-check these pages before release; Funnel is beta and may change.

## Design

```
 app (phone, LTE)                 Tailscale Funnel relay              Mac mini
 ────────────────                 ──────────────────────              ────────
 TCP to <machine>.<tailnet>.ts.net:443
 TLS 1.3 ClientHello, SNI = that name ──▶ routes by SNI, no decryption ──▶ tailscaled
                                                                       │ --tcp=443, raw bytes
                                                                       │ + PROXY v2 header (client IP)
                                                                       ▼
 pinned TLS 1.3 (server's own key) ═══════════════════════════════▶ 127.0.0.1:8788 funnel listener
 HTTP requests (login, upload…)    ═══════════════════════════════▶ same API handler, app routes only
```

- `withalbum-server -tls -funnel-url https://<machine>.<tailnet>.ts.net`
  (or `WITHALBUM_FUNNEL_URL`) opens a **second listener on loopback only**
  (`-funnel-listen`, `WITHALBUM_FUNNEL_LISTEN`, default `127.0.0.1:8788`).
  The address must be a loopback IP and differ from `WITHALBUM_LISTEN`; the
  server re-checks after binding that the socket is loopback. The main
  listener is unchanged: loopback by default, a LAN address only with the
  existing `-allow-lan -tls` rules. Funnel mode never binds a non-loopback
  address by itself.
- Requires `-tls` and an administrator who can log in (same gate as a LAN
  listener and the relay). The public URL must be a `*.ts.net` name with at
  least `<machine>.<tailnet>.ts.net` labels, port 443, 8443 or 10000, and
  nothing else.
- Tailscale command (the menu bar app runs it on request; the server logs
  it at startup): `tailscale funnel --bg --tcp=443 --proxy-protocol=2 tcp://127.0.0.1:8788`.
  **Never** `tailscale funnel 8788`, `--https` or `--tls-terminated-tcp`:
  those terminate TLS in tailscaled with a ts.net certificate, and the
  app's pin would not match (the connection fails closed; nothing is
  served in clear).
- **Route classification**: connections on the funnel listener are marked
  `funnel` and served exactly like relay connections: only `GET
  /api/v1/health`, `/api/v1/auth/*`, `/api/v1/me*`, `/api/v1/folders*`,
  `/api/v1/invitations*`, `/api/v1/sync/state`; the dashboard, the setup screen and
  `/api/v1/admin/*` answer `403 {"error":"not_available_via_relay","route":"funnel"}`,
  also for administrators; a non-canonical path is refused (shared
  `relayAllowed` table, one test covers every route).
- **Client IP and login limits**: the listener accepts an optional PROXY
  v1/v2 header (`internal/proxyproto`, fuzzed). With it, the client's
  public IP is the rate-limit and audit key, as for the LAN and the
  relay. Without it (or for loopback/unspecified addresses, `LOCAL`,
  `UNKNOWN`), every funnel connection shares the key `funnel-unknown`, so
  the per-IP login budget (10 at once, then 1 per 6 s) becomes a limit for
  the whole route; per-account limits (5 failures per account and IP, 20
  per account, 15 minutes) always apply. The per-account count of logins
  over the funnel or the relay is kept apart from the one of logins at
  home (M41): someone on the internet who knows a login name and fails its
  password can throttle that account's remote logins, never its login (or
  the pairing of a new phone) on the LAN or this Mac. Apps that are already
  signed in are not affected either way (refresh is not login-limited, and
  an unknown refresh token is refused without touching the database write
  lock). The header is trusted only
  because the listener is loopback: a local process could forge it, which
  only changes rate-limit keys (it is never used for authorization).
  Forwarding headers (`X-Forwarded-For`) are never read.
- **Exposure limits**: at most 128 connections at once on the funnel
  listener (more wait in the backlog), header and TLS handshake within
  10 s, JSON bodies (login, refresh, invitations…) within 30 s and 8 KiB,
  64 KiB headers, 60 s idle; uploads need a session and keep their
  existing limits (512 MiB per request, resumable sessions with reserved
  space).
- **Audit**: login success/failure, throttling, refresh, logout and device
  revocations record `"route":"funnel"` (or `"relay"`) next to the IP.
- **Lost phone**: `POST /api/v1/me/devices/revoke-others` signs out every
  other device of the account at once (also over the funnel and the
  relay); `DELETE /api/v1/me/devices/{id}` revokes one; administrators can
  revoke any device from the dashboard at home.

### Trust model of the funnel route

| Party | Sees | Cannot |
|---|---|---|
| Tailscale's Funnel relays | client IP, the SNI name (`<machine>.<tailnet>.ts.net`), connection times and byte counts | read or change anything inside TLS; impersonate the server (the app checks the pin) |
| `tailscaled` on the Mac | the same, plus it writes the PROXY header | read anything inside TLS (raw TCP forwarder; it never has the server key) |
| Anyone on the internet who learns the URL | reach health and the login endpoint | anything else without an account; administration at all |
| Network attackers | that a phone talks to a `ts.net` name | content |

The URL is **not a secret** (it is in the pairing QR, and a ts.net name
for which a certificate is ever issued appears in public certificate
transparency logs). Enabling Funnel makes the login endpoint
reachable from the internet: strong passwords, the rate limits above and
network isolation of the Mac ([`HOME_NETWORK_ISOLATION.md`](HOME_NETWORK_ISOLATION.md))
are the defence.

### Not supported: the TLS-terminated variant

If Funnel ever stopped offering raw TCP forwarding, the fallback would be
tailscaled terminating TLS with its ts.net certificate. Then tailscaled on
the same Mac would decrypt (Tailscale's relays would still see ciphertext
only), apps would have to trust the system CAs plus the exact ts.net host
name instead of the pin, and a compromise of the Mac's tailscaled would
expose passwords and photos in transit. It is not implemented, because
passthrough works; see `SECURITY.md`.

## App integration contract

Pairing payload (still `v=1`; parameters URL-encoded, order not significant):

```
withalbum://pair?public_route=funnel&public_url=https%3A%2F%2Fhome.example-tailnet.ts.net&spki_sha256=<64 hex>&url=<https origin>&v=1
```

- `public_url`: bare https origin, host ending in `.ts.net`, port absent or
  8443/10000. `public_route`: `funnel` (ignore unknown values). Both or
  neither; on any doubt ignore the public part — the rest stays valid.
- On a Mac that listens on loopback only (no LAN), `url` is the public URL
  too, so apps that know nothing about `public_url` still connect (with the
  same pin, sending the host as SNI as any TLS client does).
- After login over any pinned route, `GET /api/v1/me` returns
  `"public": {"url": "...", "route": "funnel"}` (absent when off) and
  `"connection_route": "direct" | "relay" | "funnel"`.

Connecting:

1. **Candidates in order: LAN `url` → `public_url` → relay.** Same rules as
   the relay contract: fall back only on network errors, remember the path
   that worked, re-try the LAN after network changes.
2. **Funnel connection:** TCP to the host:port of `public_url`, TLS 1.3 with
   **SNI = that host name** (required: Funnel routes on it), **trust = the
   paired SPKI pin**, exactly the LAN pinned client (no system CA, no
   host-name check — the server's own self-signed certificate arrives
   through the forwarder). A pin mismatch is a security stop, never a
   reason to try another path. HTTP/1.1 with the paired base URL; keep a
   separate connection pool per path.
3. Errors: connection refused/timeout/reset during the handshake = Funnel
   off or the Mac offline → next candidate, friendly message ("집 밖 연결이
   꺼져 있거나 집 Mac이 꺼져 있어요"). `403 not_available_via_relay` with
   `"route":"funnel"` → "집 Wi-Fi에서만 할 수 있어요".
4. Show the route: `connection_route` = `funnel` → "바깥에서 Tailscale로 연결됨".
5. Large uploads: resumable sessions as on the relay; Funnel bandwidth is
   limited, so expect slower transfers.
6. "내 다른 기기 로그아웃": `POST /api/v1/me/devices/revoke-others` → `{"revoked": n}`.

## Tests (what is and is not verified)

- `server/funnel_test.go`: URL and listener rules; a fake Funnel (SNI
  check, PROXY v2 header, raw byte relay) in front of the real funnel
  listener: health, login, `/me`, upload and download through it, admin
  and dashboard 403, unauthenticated 401, wrong pin and wrong SNI fail,
  the recorded forwarder bytes contain no password, token, path or photo;
  login audit has `route` and the header IP; access control over the
  funnel (another album's IDs 404, expired invitation 410, owner cannot
  accept for the invitee, viewer cannot upload, lost device revoked with
  `revoke-others`); login limits per header IP and shared without header.
- `internal/proxyproto`: v1/v2 parsing, malformed headers, `FuzzRead`.
- A local binary smoke test (curl with `--pinnedpubkey` and
  `--connect-to` to the funnel listener).
- `macos`: `swift test` parses `tailscale status --json` /
  `funnel status --json` shapes, CLI detection, commands, agent plist.
- **Not verified**: a real Tailscale Funnel (no account in CI), whether the
  macOS GUI variants accept `--tcp` Funnel, the exact JSON of current
  tailscale versions (parsed defensively), the PROXY header contents
  Funnel sends, Funnel bandwidth, phones on LTE, the app side.

## 집 밖에서 접속 켜기 (사용자 안내)

가족 휴대폰에는 아무것도 더 설치하지 않습니다. **Tailscale 가입·설치는 서버 Mac에서만** 합니다. 공유기 포트포워딩은 하지 않습니다.

### 1. 이 Mac에 Tailscale 설치·로그인

1. https://tailscale.com 에서 가입합니다(개인 무료 플랜).
2. Funnel은 공식 문서상 **오픈소스 tailscaled**에서 지원됩니다. 터미널에서:
   ```
   brew install tailscale
   sudo brew services start tailscale
   sudo tailscale up
   ```
   나오는 주소를 브라우저로 열어 로그인합니다. (App Store/Standalone 앱도 될 수 있지만 공식 표에는 "지원 안 함"으로 나와 있어요. 메뉴 막대 앱이 설치 방식을 알려 줍니다.)

### 2. Tailscale 관리 콘솔에서 세 가지 켜기

https://login.tailscale.com/admin 에서:

1. **DNS → MagicDNS 켜기**
2. **DNS → HTTPS Certificates 켜기**
3. **Access controls → 정책 파일에 Funnel 허용** 추가 후 저장:
   ```json
   "nodeAttrs": [
     { "target": ["autogroup:member"], "attr": ["funnel"] }
   ]
   ```
   (처음 Funnel을 켤 때 tailscale이 보여 주는 `https://login.tailscale.com/f/funnel?...` 링크를 눌러 켜도 됩니다.)

### 3. 메뉴 막대 앱에서 켜기

1. WithAlbum Server 메뉴 → **집 밖에서 접속 (Tailscale Funnel)…**
2. **상태 확인** → Tailscale 설치 방식, 로그인, 정책(Funnel/HTTPS/MagicDNS)이 보입니다. 앱은 누를 때만 `tailscale` 명령을 실행해요.
3. **Funnel 켜기** → 서버가 Funnel용 입구(이 Mac 안의 127.0.0.1:8788)와 함께 다시 시작되고, `tailscale funnel --bg --yes --tcp=443 --proxy-protocol=2 tcp://127.0.0.1:8788`이 실행됩니다. "켜짐 · 서버로 그대로 전달"이 나오면 성공입니다.
4. **새 페어링 QR 보기** → QR에 `https://<이 Mac>.<tailnet>.ts.net` 주소가 들어 있습니다. 이미 연결한 가족 기기는 다시 스캔하지 않아도 로그인 뒤 서버에서 주소를 배웁니다(새 앱 버전 필요).
5. 끌 때는 **Funnel 끄기**(Tailscale 전달을 끄고 서버를 원래대로 다시 시작).

"tailnet 정책에서 Funnel이 꺼져 있어요"가 나오면 2단계의 3을 하고, 앱에 나온 버튼으로 관리 콘솔을 엽니다.

### 4. LTE로 확인

1. 휴대폰 **Wi-Fi를 끄고** LTE/5G로 둡니다.
2. WithAlbum 앱에서 앨범 보기·사진 올리기·원본 받기를 해 봅니다. 연결 상태에 "바깥에서 Tailscale로 연결됨"이 보여야 합니다(앱 업데이트 후).
3. 큰 동영상도 올려 보고 걸린 시간을 적어 둡니다(Funnel에는 공개되지 않은 대역폭 제한이 있어요).
4. Mac을 재부팅하고 로그인한 뒤 다시 LTE로 확인합니다(`--bg`라 Tailscale 설정은 유지돼요. 서버는 로그인 시 시작).
5. 터미널에서 확인하려면: `curl -k https://<이 Mac>.<tailnet>.ts.net/api/v1/health` → `{"status":"ok",…}` (관리 화면 주소 `/`는 403이어야 정상).

### 꼭 같이 할 일

- 공개 주소는 인터넷의 누구나 접속을 **시도**할 수 있습니다. 가족 계정 비밀번호를 길게 하세요.
- 휴대폰을 잃어버리면 다른 기기의 앱에서 "내 다른 기기 로그아웃"을 하거나, 집에서 관리 화면 → 사용자 → 기기에서 지우세요.
- 홈캠·Home Assistant와 이 Mac을 네트워크에서 떼어 놓으세요: [`HOME_NETWORK_ISOLATION.md`](HOME_NETWORK_ISOLATION.md), 메뉴의 **내부망 격리 점검…**. 공유기로 나눌 수 없으면 Docker 구성([`DOCKER.md`](DOCKER.md) "Tailscale Funnel과 함께").
