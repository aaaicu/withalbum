# WithAlbum Server — threat model (M41, 2026-10)

Scope: this repository (Go server, relay, macOS menu bar app, scripts) as
of the M41 security review. The mobile apps are out of scope except for
the contract they rely on (pinning, pairing payload, API). Findings and
their status are in [`SECURITY_REVIEW_2026-10.md`](SECURITY_REVIEW_2026-10.md);
this document is the model they were looked for against. Keep it current
when a new entry point, route or actor appears.

Method: STRIDE per entry point (Spoofing, Tampering, Repudiation,
Information disclosure, Denial of service, Elevation of privilege). Every
threat names the mitigation in the code and the test that holds it; **GAP**
marks what is not mitigated or not tested.

## Assets

| Asset | Where | Why it matters |
|---|---|---|
| Originals (photos, videos) and their metadata (EXIF GPS, capture time, camera) | `.system/pool` on the storage volume, `blobs`/`assets` tables | the family's private data; loss is permanent |
| Previews | `.system/previews` | derived, re-encoded without metadata |
| Accounts, password hashes (Argon2id), sessions (token SHA-256 only), audit log | `.system/withalbum.db` | account takeover, repudiation |
| TLS private key (SPKI pin) | `.system/tls/server.pem` (0600) | impersonating the server to every paired app |
| Album membership and roles | `folders`, `folder_members`, `folder_invitations` | who may see what |
| Backups | backup target (`snapshots/`, `pool/`) | same as the library, plus restore authority |
| Availability of the home server | the Mac mini, its disk, the remote routes | family uploads and viewing |

## Actors

| Actor | Can reach | Assumed capability |
|---|---|---|
| Internet attacker | funnel URL (it is in the pairing QR and in CT logs; not a secret), relay rendezvous | any number of TCP connections from many addresses; knows or guesses login names; no account |
| LAN attacker, incl. a compromised IoT device | LAN listener (with `-allow-lan`), mDNS/ARP, the Mac's other services | same as internet attacker on the LAN; can spoof IPs on the LAN; cannot break TLS |
| Stolen family phone | everything its tokens allow, on every route, until revoked | holds a valid refresh token (app keychain); may be unlocked |
| Malicious album member (VIEWER/CONTRIBUTOR) | album routes of its albums | valid account; crafted uploads, comments, names |
| UPLOADER | upload routes of its albums | valid account; wants to learn what is in the album |
| Server administrator (SERVER_ADMIN account) | admin routes from home | manages accounts; should not read albums through the API |
| OS user / admin of the Mac | the disk, the processes | out of the server's control (files are not encrypted at rest) |
| Relay operator | the relay process and its traffic | sees metadata (who, when, how much), can drop or delay; cannot read the pinned inner TLS |
| Tailscale (Funnel relays, tailscaled) | the funnel route | sees client IP, SNI, timing, sizes; writes the PROXY header; cannot read the pinned inner TLS |

## Trust boundaries

1. **Internet ↔ Mac**: Funnel (raw TCP, TLS end to end to the server key) and the relay (outbound tunnel, TLS end to end inside). Only app routes are served (`httpapi.RemoteAllowed`); requests are marked with their route by the listener, never by anything the client sends (`httpapi.WithRoute`).
2. **LAN ↔ Mac**: a non-loopback listener exists only with `-allow-lan` + `-tls` + a usable administrator (`checkListen`, `listenAllowed`).
3. **This Mac ↔ server**: plain HTTP on loopback (dashboard, setup screen); `Host` must be loopback (DNS rebinding).
4. **Authenticated caller ↔ album**: every album route is authorized per request against the database (`middleware_authz.go`), writes re-check inside the transaction.
5. **Untrusted bytes ↔ parsers**: uploads feed EXIF/HEIF/QuickTime parsers (pure Go, bounded), the built-in image decoder (40 MP cap) and optional external tools (`sips`, `ffmpeg`).
6. **Backup disk ↔ library**: a backup is trusted content once its manifest and checksums verify (see "Backup/restore").

## E1. Main listener (loopback plain HTTP; LAN over TLS)

| STRIDE | Threat | Mitigation | Test |
|---|---|---|---|
| S | Impersonate the server on the LAN | Self-signed key pinned by SPKI SHA-256 in the pairing payload; TLS 1.3 only | `TestLANOverPinnedTLS`, `FuzzPairingPayload` (no field injection into the payload) |
| S | Guess passwords | Argon2id, per-IP / per-account-and-IP / per-account limits, dummy hash for unknown accounts | `internal/auth/ratelimit` tests, `TestLoginFailuresAreIndistinguishable` |
| S | Steal a session cookie via the dashboard | HttpOnly, SameSite=Strict, Secure on TLS, CSRF token for unsafe methods, JSON content type required | `TestBrowserSessionCSRF`, `auth_test.go` |
| T | Path traversal through IDs or filenames | IDs are database keys checked by regexp; pool paths come from server-computed SHA-256; stored names are one path element (`..` refused since M41) | `FuzzStoredFilename`, contract `UploadFilenames`, `TestAuthzMatrixCrossFolderIDs` |
| R | Deny an action | Audit rows in the same transaction as the change; denials summarized | `members_test.go`, `denials_internal_test.go` |
| I | IDOR across albums | Folder from path + membership per request; asset must be linked to that folder | `TestAuthzMatrix`, `TestAuthzMatrixCrossFolderIDs`, `TestACLMatrix` |
| I | Stored HTML/SVG runs in the server origin | Originals: `Content-Disposition: attachment`, CSP `sandbox`, nosniff | `TestStoredHTMLIsNotServedAsDocument` |
| I | DNS rebinding to the loopback listener | 421 unless Host is loopback | `TestLoopbackHostOnly` |
| D | Slow clients, big headers | ReadHeaderTimeout 10 s (also TLS handshake), 64 KiB headers, JSON 8 KiB within 30 s; uploads only after authentication | `security_test.go`; **GAP**: no per-client connection cap on the LAN (accepted, home LAN) |
| D | Disk filling | Free-space reserve, upload session reservation, 16 open sessions per account | `TestMinFreeBytes`, `uploads_test.go` |
| E | A role does more than allowed | One policy table, panic on undeclared routes, exhaustive role × route × transport matrix | `TestAuthzMatrix`, `TestAuthzMatrixCoversEveryRoute` |
| E | Server process compromise = host compromise | Runs as the logged-in user (LaunchAgent / systemd --user); refuses root since M41 | `TestRefuseRoot` |

## E2. Funnel listener (internet, loopback TCP 8788)

| STRIDE | Threat | Mitigation | Test |
|---|---|---|---|
| S | Tailscale or tailscaled reads or alters traffic | Raw TCP forwarder; inner TLS to the pinned key; TLS-terminating Funnel fails the pin | `TestFunnelTrust`, `TestFunnelEndToEnd` |
| S | Brute-force logins from the internet | Same limits as E1; PROXY header gives per-client IP | `TestFunnelLoginLimits` |
| D | **Lock a family member out by failing their password from the internet** | Per-account count kept separately for remote and home logins (M41): remote failures throttle remote logins only | `TestRemoteLoginFailuresDoNotLockHomeLogin`, `TestRemoteFailuresDoNotLockLocal` |
| D | Without a PROXY header all clients share `funnel-unknown` | Menu app always passes `--proxy-protocol=2`; home logins unaffected (separate keys) | `TestFunnelLoginLimits`; accepted residual |
| D | **Hold all 128 funnel connection slots** (slowloris, idle keep-alive) | 10 s header/handshake timeout, 60 s idle | **GAP (open)**: no per-client connection cap |
| D | Flood `POST /auth/refresh` with random tokens to hold the SQLite write lock | Unknown tokens are refused by a read-only lookup before any write transaction (M41) | `TestUnknownRefreshTokenTakesNoWriteLock` |
| D | Saturate the 2 Argon2id slots with login attempts | 503 busy after 5 s; per-IP buckets | **GAP (open, low)**: home logins can see 503 during an attack (signed-in apps are unaffected) |
| I | Reach the dashboard, setup or admin API from the internet | `RemoteOnly`: 403 `not_available_via_relay` for every non-app path, also for administrators | `TestAuthzMatrix` (funnel column), `TestFunnelAccessControl`, `TestRelayAllowed` |
| T | Forge the PROXY header | Trusted only on loopback; only a rate-limit/audit key, never authorization | `FuzzRead` (proxyproto), `TestFunnelTrust` |
| E | Bind the funnel listener to the LAN or internet | Loopback only, checked before and after bind | `TestCheckFunnel`, `TestFunnelListenerLoopbackOnly` |

## E3. Relay path (internet → relay → outbound tunnel)

| STRIDE | Threat | Mitigation | Test |
|---|---|---|---|
| S | Someone registers as this server at the relay | Challenge signed with the server key, bound to the TLS exporter; rendezvous ID = hash of the pin | `TestRelayRendezvousCannotBeSquatted`, `TestProof` |
| S/I | Relay operator reads or alters traffic | Inner pinned TLS end to end; wrong key fails | `TestRelayWrongPin`, `TestRelayEndToEnd` |
| T | Malformed frames crash or confuse the mux | Length-bounded frames, credit windows, control-queue bound, max streams | `FuzzReadFrame`, `FuzzHandshake`, `TestMuxProtocolViolations`, `TestMuxMaxStreams` |
| D | Relay operator drops or delays | Accepted (availability); LAN and funnel routes are independent | — |
| I | Relay learns metadata | Accepted and documented (`RELAY_DESIGN.md`) | — |
| E | Admin routes over the relay | Same `RemoteOnly` table as the funnel | `TestAuthzMatrix` (relay column) |
| D | Relay-supplied client address chooses rate-limit keys | Only a rate-limit/audit key; scope comes from the listener | `TestRemoteLoginFailuresDoNotLockHomeLogin` |

## E4. Dashboard and setup screen (this Mac, loopback)

| STRIDE | Threat | Mitigation | Test |
|---|---|---|---|
| S | Another local user or a web page completes setup | Console one-time code (60 bits, 30 min, 5 tries, constant-time), session cookie + CSRF + same origin, loopback plain listener only | `TestSetupCodeLimits`, `TestSetupSessionAndCSRF`, `TestSetupOnlyOnPlainLoopback` |
| T | XSS in the dashboard | CSP with script/style hashes, Trusted Types, DOM via `textContent` only | `TestSecurityHeaders`, `TestDashboardDesignRules`; manual `DASHBOARD_CHECKLIST.md` |
| I | Admin reads albums through the dashboard | Admin routes never return album content; album routes are 404 for non-members | `TestAuthzMatrix` ("admin" column), `admin_test.go` |
| E | Setup routes in normal mode or on TLS | Separate route table, never registered there | `TestNormalModeHasNoSetupRoutes` |

## E5. macOS menu bar app and LaunchAgent

| STRIDE | Threat | Mitigation | Test |
|---|---|---|---|
| E | Installer or app escalates to root | No `sudo`, LaunchAgent in the user's domain, server refuses root | `TestRefuseRoot`; `scripts/*.sh` have no sudo (review) |
| T | Plist injection through paths or addresses | Plists written through `plutil`/structured APIs; addresses validated | `macos` Swift tests (`swift test`) |
| I | Logs leak secrets | Logs in a 0700 directory; no tokens/passwords logged (setup code is printed to the console by design) | review; **GAP**: no automated log scan |

## E6. Backup, restore and admin CLI

| STRIDE | Threat | Mitigation | Test |
|---|---|---|---|
| T | Crafted manifest writes outside the library | Every path comes from validated SHA-256 names; journal names checked; staging + atomic swap | `TestRestoreRefusesDamagedBackup`, `TestRestoreCrashRecovery`, `readJournal` checks |
| T | Tampered backup replaces accounts on restore | **Accepted**: a verified backup is trusted content (its DB includes users and admins); keep backup disks under the owner's control | — |
| R | Restore hides who did what | Every session is revoked on restore; audit log restored as backed up | `TestRestoreRevokesSessions` |
| I | Backup readable by others | Backup target chmod 0700, DB copies 0600 | review (`server/maint.go`) |
| E | Admin CLI while the server runs | Storage lock required | `TestAdminRefusesWhileServerRuns` |

## E7. Media pipeline (uploads → parsers, previews)

| STRIDE | Threat | Mitigation | Test |
|---|---|---|---|
| D/E | Malformed EXIF/HEIF/QuickTime | Pure Go, ReaderAt budget (1 MiB, 4096 reads), box/IFD/segment count limits | `FuzzExtract`, `FuzzDuration`, `FuzzExtractLocation`, `FuzzISO6709`, `FuzzExtractMediaInfo`, `FuzzStillIdentifier`, `FuzzVideoIdentifier` |
| D | Decompression bomb in the built-in decoder | `DecodeConfig` first, 40 MP cap, one decode at a time, panic recovered | `blobstore` tests |
| D | Decompression bomb through `sips` | Refused above 120 MP before conversion (M41), 256 MiB input, 90 s, one at a time | `TestSipsRefusesHugeCanvas`; **GAP (open, low)**: no total memory limit for the tools |
| E | Argument or protocol injection into `ffmpeg`/`sips` | Absolute tool path, argument array, no shell, minimal env, `-protocol_whitelist file`, `-format_whitelist`, source is a hex pool path | `TestRunToolMinimalEnvironment`, `TestConverterContract` |
| T | Tool modifies the original | Original 0400, size/mode/mtime compared before and after | `TestConverterContract` (tamper mode) |
| I | Location leaks | API hides `location` per album/uploader setting on every asset JSON path (snapshot, sync events, social); previews carry no metadata. **Originals are byte for byte, GPS included** (documented) | `TestHideLocation`, `TestHideMyLocation`, `TestLocationWithSocial`, `TestPreviewsCarryNoLocation` |
| I | UPLOADER learns whether bytes already exist | Same steps and 202 receipt for every outcome; resumable uploads need all bytes | `dedup_test.go`; accepted residual timing difference (re-hash of an existing large file) |
| S | Display spoofing in names others see | Comments, album names and device labels refuse control characters and bidi overrides (M41) | `TestNamesOthersSeeAreSingleLinePlainText`, `FuzzParseComment`, `TestPlainLabel` |

## Actor view (summary)

| Actor | Worst realistic outcome today | Main controls |
|---|---|---|
| Internet attacker | Remote route unavailable (connection slots, Argon2 slots); guessing weak passwords slowly | RemoteOnly table, rate limits (scoped), pinned TLS, strong passwords |
| LAN attacker / IoT | Same as internet on the LAN; attacks on other Mac services | TLS-gated LAN listener; **network isolation** (`HOME_NETWORK_ISOLATION.md`) |
| Stolen phone | Full access of that account until revoked (90-day absolute session end) | `revoke-others`, admin revoke, password change revokes others |
| Malicious member | Reads what its role allows (incl. GPS in originals it can download); spam within limits | per-request ACL, social rate limits, plain-text names |
| UPLOADER | Learns nothing but its own receipts (small timing residual) | 202 receipts, route policy |
| Server admin (account) | Manages accounts, cannot read albums by API; can reassign archived albums only | authz, matrix test |
| Mac OS admin | Everything on disk | out of scope (disk encryption, FileVault) |
| Relay operator / Tailscale | Metadata, availability | end-to-end pinned TLS |
