# Access from outside the home: end-to-end encrypted relay (M35)

Status: **implemented and tested in Go** (relay `withalbum-relay`, home-server
client, integration test through a real relay process in memory). Not yet
measured on a public server with a phone on a mobile network, and no app
implements it yet. Operations: [`RELAY_OPERATIONS.md`](RELAY_OPERATIONS.md).

## Goal and properties

Family members reach the home server from anywhere without installing
anything extra (no VPN app), and without the home server opening a port to
the internet. The relay is a meeting point on a public host:

```
 app (phone, LTE)                   relay (public host, :443)             home server (Mac mini)
 ────────────────                   ─────────────────────────             ──────────────────────
 TCP+TLS 1.3 to relay host  ───────▶ outer TLS (public CA cert)
 "CONNECT <rid>"                    looks up tunnel <rid>   ◀──────────── one outbound TCP+TLS 1.3
                                    opens stream N on it                  connection ("tunnel"),
 inner TLS 1.3, pinned to the  ═══════════════ bytes only ═══════════════▶ authenticated by a
 server's SPKI (same pin as LAN)    (never decrypted)                     signature of its TLS key
 HTTP requests (login, upload…) ═══════════════════════════════════════▶ same API handler as LAN
```

| Property | How |
|---|---|
| Home server opens **no inbound port** | It dials out to the relay and keeps that connection; app connections arrive as streams inside it. |
| Relay sees **only ciphertext** | The app runs TLS 1.3 *inside* the relay stream, with the server's existing self-signed identity and the same SPKI pin as on the LAN. The relay never terminates that TLS and never has the server key. Proven by `server/relay_test.go` `TestRelayEndToEnd`: every byte the relay forwarded, recorded at both ends, contains neither the photo, the password, the token, the login name nor any request path (control: the same search finds all of them in the app's plaintext). |
| A malicious relay **cannot impersonate** the server | The pin check of the inner handshake fails for any other key (`TestRelayWrongPin`). |
| Nobody can **squat** a server's relay address | The rendezvous ID (rid) is derived from the server's public key, and the relay computes it itself from a key the server proves it holds, with a signature bound to that very TLS connection (`TestRelayRendezvousCannotBeSquatted`). |
| Off by default | `-relay host:port` / `WITHALBUM_RELAY`; requires `-tls` and a usable administrator, like a LAN listener. |

## Threat model

| Actor | Can | Cannot | Notes |
|---|---|---|---|
| **Relay operator** (or anyone who took over the relay host) | See metadata (below); refuse, delay or drop connections; connect to a home server like any app (it knows the rid) and try logins. | Read or change requests, responses, photos, passwords or tokens; impersonate the server (pin); register as another server (key proof). | The operator is trusted for availability only. A hostile relay can also lie about the client IP it passes on (see *login limits*). |
| **Network attacker** (Wi-Fi, carrier) | See that a device talks to the relay host, when and how much. | See the rid (inside the outer TLS), anything inside. An attacker with a mis-issued certificate for the relay host gains only what the relay operator has. | Apps verify the relay certificate normally (system roots, host name). |
| **Anyone on the internet who learns the rid** | Reach the server's login endpoint through the relay. | Anything beyond `/api/v1/health` without an account. | **This is the real exposure change of enabling the relay.** The rid is *not* a secret: it is in the pairing QR and can be computed from the pin. Protection: Argon2id passwords, login rate limits (per client IP, per account and IP, per account from any IP), per-server stream rate and caps at the relay, and administration routes refused through the relay. |
| **Stolen phone** | Use its stored session through the relay as from the LAN. | — | Revoke the device: the owner can do it from another of their devices (`DELETE /api/v1/me/devices/{id}` is served through the relay), an administrator from the dashboard at home. Refresh tokens expire (30 days, at most 90 after login). |
| **Malicious family member** (has an account) | Everything their album roles allow, from anywhere; open many connections, which counts against the household's shared per-server caps (32 streams, 5 new/s, 8 MiB/s by default). | See albums they are not in (same ACL as LAN); reach administration through the relay. | Same ACL code path as the LAN listener. |
| **Relay abuse** (strangers using the relay) | Register their own tunnels with self-made keys (keys are free) and use the relay as a generic tunnel. | Affect other servers beyond the shared global limits. | Limits: connections per IP and in total, new connections per second per IP, tunnels per IP and in total, streams per tunnel, bandwidth per tunnel, idle timeouts. Residual risk: a public relay is a free generic tunnel within those limits; accounts/quotas for the official relay are M44. |

### Metadata the relay sees (disclose to users)

- the rendezvous ID of each home server (a stable pseudonym; it does not reveal the owner),
- the IP addresses of the home server and of each app connection (TCP level; **not logged** unless the operator passes `-log-ips`),
- when a tunnel and each app connection start and end, and how long they last,
- the number of bytes in each direction per connection (this reveals activity and roughly how large uploaded or downloaded files are),
- how many connections run at once,
- the inner TLS ClientHello (cipher suites, and the SNI name if the app sends one — apps should send none or a fixed one, never the LAN address); the server certificate is encrypted in TLS 1.3.

The relay logs, as JSON lines: `tunnel_up`, `tunnel_down`, `tunnel_replaced` (rid, age, reason), `stream_end` (rid, stream number, duration, bytes each way, reason), refusals with a reason code. It never logs content, and keeps nothing on disk.

The home server learns the app's public IP (the relay passes it so that login limits apply per app); it is stored like a LAN client IP.

## Protocol (version 1)

All lines are ASCII, end with `\n` (a preceding `\r` is tolerated), at most 512 bytes, fields separated by one space. Answers: `OK[ <arg>]` or `ERR <code>`, after which the relay closes the connection.

### Outer layer

TCP to the relay host (default port 443), TLS **1.3 only**, certificate of the relay host from a public CA (or a private CA the operator distributes), verified normally by both apps and home servers. Clients should offer ALPN `withalbum-relay/1`; the relay does not require it.

### App connection

```
app → relay:  WITHALBUM-RELAY/1 CONNECT <rid>\n
relay → app:  OK\n                  (then raw bytes both ways: the inner TLS session)
          or: ERR <code>\n          (and close)
```

The app may start the inner TLS ClientHello right after the CONNECT line without waiting; the relay sends `OK\n` before any byte from the server. Nothing else is added: after `OK` the connection carries exactly the bytes of one inner TLS session, and closing it closes the stream.

Error codes: `offline` (no tunnel for this rid: the home server is off or not connected), `busy` (a capacity limit), `rate_limited` (too many new connections to this server), `bad_request`, `unsupported_version`, `shutting_down`. A connection refused before TLS (per-IP limits) is simply closed.

### Home server tunnel

```
server → relay:  WITHALBUM-RELAY/1 SERVE\n
relay → server:  CHALLENGE <nonce>\n                 32 random bytes, base64url without padding
server → relay:  PROVE <spki> <signature>\n         base64url without padding
relay → server:  OK <rid>\n | ERR <code>\n          unauthorized, busy, …
                 then frames both ways
```

- `spki`: the DER SubjectPublicKeyInfo of the server's TLS certificate (ECDSA P-256).
- `signature`: ECDSA P-256 / SHA-256 (ASN.1 DER) over `SHA-256("withalbum-relay/1 serve proof\0" || nonce || exporter)`, where `exporter` = TLS exporter of the outer connection, label `EXPORTER-withalbum-relay-serve`, no context, 32 bytes (RFC 8446 §7.5). The exporter binds the proof to this connection, so a proof cannot be replayed elsewhere or relayed by a third party. The context string keeps it distinct from TLS CertificateVerify signatures made with the same key.
- The relay verifies, computes `pin = SHA-256(spki)` and `rid = RendezvousID(pin)`, and answers `OK <rid>`; the server checks it is its own.
- If a tunnel for that rid exists, the new one **replaces** it (only the key holder can get here: it is the same server reconnecting, e.g. after a network change).

### Rendezvous ID

```
rid = lowercase base32 (RFC 4648 alphabet, no padding) of the first 20 bytes of
      SHA-256( "withalbum-relay/1 rendezvous id" || 0x00 || pin )
pin = the 32-byte SHA-256 of the SPKI (the same pin the app already has)
```

32 characters `[a-z2-7]`. Apps can compute it from the pin and **must** check that a rid in a pairing payload matches it (reject the relay part otherwise). Test vectors: pin = 32 zero bytes → `po5gfj7zpfgccc6nxpldqm3mjf5go5le`; pin = 31 zero bytes then `01` → `qvqz3bl57oarddduegea4k7n6yapugfm` (`internal/relay/proto_test.go`).

### Frames (tunnel only)

`type (1 byte) | stream ID (4 bytes, big endian) | length (4 bytes, big endian) | payload`

| Type | Name | Stream | Payload |
|---|---|---|---|
| 0 | DATA | ≠ 0 | 1–16384 bytes |
| 1 | WINDOW | ≠ 0 | uint32 credit increment, > 0 |
| 2 | OPEN | ≠ 0 | relay → server only; the app's IP as text, ≤ 64 bytes |
| 3 | FIN | ≠ 0 | empty; the sender sends no more data (half close) |
| 4 | RST | ≠ 0 | empty; abort both directions |
| 5 / 6 | PING / PONG | 0 | 8 bytes, echoed |

- Only the relay opens streams, with increasing IDs 1, 2, 3, …; an ID going back, a frame for a never-opened stream, an unknown type, a bad length (checked from the header, before the payload is read), DATA after FIN, or data beyond the window closes the whole tunnel.
- **Flow control**: each stream starts with 256 KiB of credit in each direction. A receiver grants credit back (WINDOW) as the application reads, in batches of at least 128 KiB, so at most 256 KiB is buffered per stream and a slow phone only slows its own stream. Throughput per stream ≈ 256 KiB / RTT (≈ 2.5 MB/s at 100 ms), within the per-tunnel bandwidth limit.
- **Keepalive**: the home server sends PING every 20 s; the relay closes a tunnel silent for 75 s, the server reconnects when the relay is silent for 60 s.
- The reading side never blocks on a write (answers are queued), so two peers cannot deadlock.

## Decisions

1. **Transport: TLS 1.3 over TCP port 443, a text handshake, and our own small framing — not WebSocket, QUIC, HTTP/2 or yamux.**
   - QUIC needs a large dependency, UDP is blocked on many networks, and Dart has no QUIC socket API.
   - WebSocket adds HTTP upgrade parsing and message framing on both sides and gives an app a message stream instead of a socket, so the inner TLS would still need a bridge; its one advantage (passing HTTP-only proxies) matters little for phones.
   - HTTP/2 or yamux (MPL-2.0) would add dependencies and features we do not need. The framing here is about 400 lines of Go, standard library only, with fixed per-type sizes, fuzzed (`FuzzReadFrame`, `FuzzHandshake`).
   - The **app side has no framing at all**: one app TCP connection is one stream. Only the home server multiplexes, so the app work is small.
2. **One multiplexed tunnel per home server** (instead of the server dialing a new connection per app connection): one NAT mapping to keep alive, no extra TLS handshake from home per request, instant reaction to new app connections.
3. **The rid is derived from the pin**, and the server proves its key with a connection-bound signature using its existing TLS key. No accounts or secrets at the relay, nothing to register, nothing a relay can forge; a phone paired before the relay existed can compute the rid itself.
4. **Pairing payload stays `v=1`** and gains optional `relay` and `rid` parameters. The current app rejects any `v` other than `1`, so bumping the version would break every installed app; unknown parameters are ignored by it.
5. **Through the relay, only app routes are served** (an allowlist: health, `/api/v1/auth/*`, `/api/v1/me*`, `/api/v1/folders*`, `/api/v1/invitations*`). The dashboard page and every `/api/v1/admin/*` route answer `403 not_available_via_relay`, also for administrators; a path not in canonical form is refused. Administration stays on the Mac or the home network, so a stolen admin password alone is not enough to manage accounts from the internet. A test fails when a new route is added without classifying it. The setup screen never runs with `-tls`, so it is never reachable either.
6. **Relay connections are treated as authenticated users over pinned TLS**, exactly like LAN clients, with the same ACL code. The relay does not need `-allow-lan`; it does need `-tls` and an administrator who can log in (same reason as the LAN gate).
7. **Login limits** use the app's IP as reported by the relay (`OPEN` payload), so one family member's mistakes do not lock out the others. The relay is not trusted beyond that: a loopback or unparsable value becomes `relay-unknown`. A lying relay can evade the per-IP limit; the per-account limits still apply.
8. **Separate binary `withalbum-relay` (`./relay`)**, not a server subcommand: the public host then carries no storage, SQLite or API code at all, and the two are deployed and updated independently. Same module, AGPL-3.0, public — anyone can audit and self-host it.
9. **Relay certificate from files** (`-cert`, `-key`, re-read when they change, e.g. after certbot renews). Built-in ACME (autocert) was left out because it needs a new dependency (`golang.org/x/net`); it can be added later.
10. **Operation: an official relay plus self-hosting.** The default is off; the address of an official relay is configured, not built in (none runs yet). A self-hosted relay with a private CA is supported (`WITHALBUM_RELAY_CA_FILE` on the server; the app would need the same CA — not planned for v1 apps).
11. **Reconnect** with exponential backoff from 1 s to 2 min and "equal jitter" (a random half of the delay), reset after a tunnel lived a minute.

## App integration contract

### Pairing payload

```
withalbum://pair?relay=<host:port>&rid=<rid>&spki_sha256=<64 hex>&url=<https origin>&v=1
```

(parameters URL-encoded, order not significant). `relay` and `rid` are optional; when present both must be present, `relay` must be `host:port`, and `rid` must equal `RendezvousID(pin)` — otherwise ignore the relay part (the LAN part stays valid). An app paired without them learns them, once logged in over a pinned connection, from `GET /api/v1/me`:

```json
{"user": {...}, "device_id": "...", "relay": {"endpoint": "relay.example.org:443", "rendezvous_id": "abcd…"}}
```

(`relay` is absent when the server has no relay). Administrators also see `"relay": {"endpoint", "rendezvous_id", "connected"}` in `GET /api/v1/admin/server/tls` (LAN only).

### Connecting

1. **Candidates, in order:** the paired LAN `url` first (direct pinned TLS, connect timeout about 2 s), then the relay. A "happy eyeballs" variant is fine: start the relay attempt if the LAN attempt has not connected within ~300 ms, use whichever completes first, prefer LAN when both do. Remember the path that worked; re-try LAN first after a network change (Wi-Fi ↔ cellular) and periodically while on the relay, so that a phone that comes home moves back to the LAN.
2. **Fall back only on network errors** (connection refused, timeout, unreachable, relay `ERR offline/busy/rate_limited`). **Never** fall back on a pin mismatch or TLS policy error — on either path that is a security stop (`TlsPinRejected`), not a reason to try the other path.
3. **Relay path:**
   1. TCP to `relay` host:port, TLS 1.3 with the **system trust store and host-name check** (a separate `SecurityContext(withTrustedRoots: true)`), ALPN `withalbum-relay/1` (optional).
   2. Write `WITHALBUM-RELAY/1 CONNECT <rid>\n`.
   3. Read one line (≤ 512 bytes). `OK` → continue; `ERR <code>` → map: `offline` = "the home server is off or not online", `busy`/`rate_limited` = retry with backoff, others = error.
   4. Run the **same pinned TLS client as for the LAN** over that connection (TLS 1.3, no trusted roots, SPKI pin check during and after the handshake). Send no SNI, or a fixed name; never the LAN address.
   5. Send HTTP/1.1 requests with the paired base URL unchanged (the `Host` header is not checked on TLS). Keep a separate HTTP client or connection pool per path, so a LAN connection is never mixed up with a relay one.
4. **Dart notes (to verify on devices):** the current `connectionFactory` returns a `SecureSocket`. For the relay it must return the *inner* pinned socket. Two ways: (a) nested TLS — `SecureSocket.connect` to the relay, then `SecureSocket.secure(outer, context: pinned, onBadCertificate: pinCheck)`; reading the `OK` line first requires care (read it via `RawSecureSocket` and pass the subscription, or send the CONNECT line and start the inner handshake immediately, treating an early close as "relay refused"); (b) a loopback bridge — listen on `127.0.0.1:0` in the app, pipe each accepted local socket to a new relay connection after the `OK` line, and let the existing pinned client connect to the local port. (b) works with any TLS stack; (a) avoids the extra hop. Either way the pin check is the existing code.
5. **Through the relay**, `GET /` and `/api/v1/admin/*` answer `403 {"error":"not_available_via_relay"}`; hide administration screens when on the relay path.
6. Large uploads already use resumable sessions; a relay or tunnel drop surfaces as a broken connection: ask `GET …/uploads/{id}` for the offset and continue (tested through a tunnel drop in `TestRelayEndToEnd`).

## What is not done

- No app implementation (app milestone; contract above).
- Not measured on a public host with a phone on LTE (needs a person, see RELAY_OPERATIONS.md).
- No accounts, quotas or billing for an official relay (M44); no geographic relays; no ACME.
- Browser access from outside the home is out of scope (a browser cannot pin).
- The dashboard has no relay setting screen yet; configuration is the `-relay` flag or `WITHALBUM_RELAY` (the menu bar app does not set it yet).
