# Security review, October 2026 (M41)

Internal adversarial review of the server after the internet routes
(Tailscale Funnel, M47; relay, M35) and the features added since M15:
likes/comments/mentions (M49), location and its hiding (M46), camera and
pixel size (M48), automatic backup uploads, idempotent album creation.
The threat model it was done against: [`THREAT_MODEL.md`](THREAT_MODEL.md).
The previous review (M15, LAN listener) is in [`../SECURITY.md`](../SECURITY.md).

This is **not** an external review. An independent human review is still
required before the server holds irreplaceable photos with a public route
enabled; the brief for it is at the end.

## Method

1. Threat model (STRIDE per entry point, actors from internet attacker to
   relay operator), each threat mapped to code and a test.
2. Automated checks, all triaged (below): `govulncheck` v1.9.0,
   `staticcheck` v0.8.2, `gosec` v2.30.0, `go test -race`, every fuzzer
   for 60 s or more, new fuzzers for the request parsers.
3. An exhaustive authorization matrix generated from the router's policy
   table: 65 routes × 10 kinds of caller × 3 transports (direct, relay,
   funnel), which fails on any route without an explicit classification
   (`internal/httpapi/authz_matrix_test.go`).
4. Manual review of the hot spots: authentication, sessions, refresh,
   CSRF, rate limits, every ID parameter, upload commit and resume, preview
   tools, media parsers, comments and mentions, location hiding, UPLOADER
   dedupe, dashboard CSP, admin vs albums, backup/restore paths,
   privileges of the processes, file permissions, TLS, logs.

## Findings

Severity is for a household server with the funnel or relay enabled.

| # | Severity | Area | Finding | Status | Test |
|---|---|---|---|---|---|
| 1 | Medium | Rate limits (funnel/relay) | The per-account login failure limit (20 per 15 min) was shared by every route. Anyone on the internet who knew a login name could fail its password 20 times every 15 minutes and keep that person from logging in **anywhere**, including pairing a new phone at home. | **Fixed**: the per-account count is kept separately for remote (relay, funnel) and home logins (`ratelimit.Scope`, `httpapi.rateScope`). | `TestRemoteLoginFailuresDoNotLockHomeLogin` (failed before the fix: home login 429), `TestRemoteFailuresDoNotLockLocal` |
| 2 | Medium | Refresh (funnel/relay) | `POST /auth/refresh` is public and not rate limited, and opened a `BEGIN IMMEDIATE` transaction for any well-formed token. A flood of random tokens from the internet held the SQLite write lock that uploads and logins need (they wait up to 5 s, then 503 busy). | **Fixed**: a read-only lookup refuses tokens the server never issued before any write transaction. | `TestUnknownRefreshTokenTakesNoWriteLock` (failed before the fix: SQLITE_BUSY after 5 s) |
| 3 | Medium | Release toolchain | Release builds used `go-version-file: go.mod`, i.e. Go **1.26.0**, the minimum version, so every published binary would carry the standard-library vulnerabilities fixed since (net/http, crypto/tls, crypto/x509). `govulncheck` with the local Go 1.27.2 also reports GO-2026-6809 (crypto/x509, reachable from `relay.Dial`), fixed in 1.27.3. | **Fixed**: releases build with the latest stable Go (`check-latest`) and run `govulncheck` (linux and darwin) first; CI runs `govulncheck` on every PR and weekly. Developers: update local Go to ≥ 1.27.3. | CI `security-checks` job, `release.yml` |
| 4 | Low | Upload filenames | A stored filename could be `..` (e.g. `X-Filename: ..` or `..%2F..`). It is echoed as the asset's `filename` and in `Content-Disposition`; an app that saves a download under that name inside a folder would write to the parent folder. | **Fixed**: `..` is replaced by the SHA-256 like other unusable names; downloads never send `.`/`..` as a filename (old rows). | `FuzzStoredFilename` (found it), contract `UploadFilenames` |
| 5 | Low | Preview tools | A small HEIC/TIFF can declare a huge canvas; `sips` decodes all of it in memory (no limit), pushing the Mac mini into swap. | **Fixed**: images above 120 megapixels are not handed to `sips` (preview `failed`, original kept). | `TestSipsRefusesHugeCanvas` |
| 6 | Low | Display spoofing | Comments, album names and uploader device labels accepted Unicode bidirectional overrides (U+202A–U+202E, U+2066–U+2069), and album names/device labels control characters, letting one member make text display differently to others (e.g. a disguised name or link). | **Fixed**: refused with the existing 400 codes (`invalid_comment`, `invalid_folder_name`, `invalid_device_id`); U+200E/U+200F stay allowed. | `TestNamesOthersSeeAreSingleLinePlainText`, `FuzzParseComment`, `TestPlainLabel` |
| 7 | Low | Process privilege | Nothing stopped the server from running as root, where any parser or tool flaw would own the host. | **Fixed**: the server refuses to start as root unless `WITHALBUM_ALLOW_ROOT=1` (containers). Installers already run it as the user. | `TestRefuseRoot` |
| 8 | Info | Tests | `staticcheck` SA4006 in a restore test showed a missing assertion (a refused restore must leave the library untouched); two other SA4006 were dead stores. | **Fixed**: assertion added. | `TestRestoreRefusesNonEmptyAndKeepsReplaced` |
| 9 | Medium | Funnel availability | The funnel listener serves at most 128 connections; with a 10 s header timeout and 60 s idle timeout, an unauthenticated client can keep all slots busy with very little traffic (slowloris / idle keep-alive). Effect: the remote route is unavailable; home access is not affected; no data is exposed. | **Open**: needs a per-client connection cap keyed on the PROXY address (read lazily per connection today) and a shorter idle timeout on the funnel listener; verify with real Funnel PROXY headers first. | — |
| 10 | Low | Argon2id slots | Login hashing is limited to 2 at a time (memory bound); remote login attempts can keep both busy, so home logins may get 503 `busy` during an attack. Signed-in apps (refresh) are unaffected. | **Open** (consider a reserved slot for the home route). | — |
| 11 | Low | Audit growth | Every failed login writes an audit row; under a sustained distributed attack the audit log grows without retention. Bounded by finding 10 to tens of rows per second. | **Open** (summarize like denials, or prune). | — |
| 12 | Low | Tool memory | `ffmpeg -max_alloc` bounds single allocations only; neither tool has a total memory limit (90 s timeout, one at a time). | **Open** (rlimits on Linux, launchd resource limits on macOS). | — |
| 13 | Medium (privacy expectation) | Location hiding | `hide_location` / `hide_my_location` hide the API field on every asset path (snapshot, sync, social, activity), but **originals are served byte for byte with their GPS tags**; any member who may download an original can read the location. | **Accepted, documented** in `API_v2.yaml`; the app must say so next to the setting. Stripping metadata on download would change the "original unchanged" guarantee and needs a product decision. | `TestHideLocation`, `TestHideMyLocation`, `TestLocationWithSocial`, `TestPreviewsCarryNoLocation` |
| 14 | Low | UPLOADER dedupe | Same steps and 202 receipt for new and known bytes; a known large file is re-hashed, so response time can differ slightly. | **Accepted** (documented since M22). | `dedup_test.go` |
| 15 | Low | PROXY header | Trusted from any local process on the Mac (loopback listener). It only chooses a rate-limit/audit key. Without it, all funnel clients share `funnel-unknown` (remote logins can be starved; home logins are not, finding 1). | **Accepted**; the menu app always uses `--proxy-protocol=2`. | `FuzzRead`, `TestFunnelLoginLimits` |
| 16 | Info | Backup trust | A backup that verifies (manifest + checksums) is restored as is, including accounts and administrators. Paths come only from validated SHA-256 names (no traversal). | **Accepted**: keep backup disks under the owner's control. | `TestRestoreRefusesDamagedBackup`, `TestRestoreCrashRecovery` |
| 17 | Info | Dependencies | `govulncheck` module-level: GO-2026-5932 (`golang.org/x/crypto/openpgp`, unmaintained) — the package is not imported. | **Accepted** (not reachable). | CI `security-checks` |

Re-checked without change: route policy table and remote route table
(every route classified, matrix green on all three transports); IDOR on
`{folderID}`, `{assetID}`, `{uploadID}`, `{bundleID}`, `{commentID}`,
`{userID}`, `{deviceID}`, `{invitationID}` (unknown or other-folder IDs
are 404 of the kind of the ID, never another folder's data); refresh
rotation, reuse detection, CSRF, cookie flags; upload size caps (512 MiB
per request, 64 GiB per session with reserved space, 16 open sessions per
account), chunk and final SHA-256 checks; preview tool invocation (no
shell, absolute paths, argument arrays, whitelists, original 0400 and
compared); EXIF/HEIF/QuickTime parsers (bounded reads, fuzzed); comment
mentions (only current readers, re-checked in the transaction); activity
feed and `mentionable` (readers only, never UPLOADER albums); dashboard
CSP (hashes, Trusted Types, no HTML sinks); admin routes (no album
content, archived reassign only); backup/restore paths; TLS (1.3 only,
pinned); file modes (`.system` 0700, key and DB 0600, backup target
0700, log directory 0700); logs (no tokens or passwords).

### Automated checks and their triage

| Tool | Result |
|---|---|
| `govulncheck ./...` | 1 symbol-level stdlib finding with Go 1.27.2 (finding 3); 1 module-level, not reachable (finding 17). |
| `staticcheck ./...` | 3 × SA4006, fixed (finding 8); clean now. |
| `gosec ./...` | 277 raw findings. **Fixed or justified one by one** (19 `#nosec` comments with a reason): G402 (pinned client config, pin verified in `VerifyConnection`), G404 (reconnect jitter), G204 (tool path from `FindConverters`, argument array), G103 (getattrlist syscall), G302 (directory 0700), G124 ×3 (cookie `Secure` is set on every TLS listener; plain HTTP exists on loopback only), G202 ×4 (constant SQL fragments and `?` placeholders; values are always arguments), G703 ×5 (legacy import: the operator's own storage root and validated SHA-256 names), G706 ×2 (integers only). **Excluded as a class in CI after triage**: G104 (151, unchecked `Close`/`Remove`/`Rollback` on cleanup and error paths), G115 (64, integer conversions: in production code all bounded — `ParseUint(…, 32)`, sizes checked positive, `max(…, 0)`; the rest in test helpers and `tools/`), G304 (32, file paths built from the storage root, the backup target or validated SHA-256 names). `tools/` (chaos, perf harness; never shipped) is excluded. |
| `go test -race -count=1 ./...` | green |
| Fuzzers (60 s each, plus 15 s in CI) | `FuzzParseCursor`, `FuzzParseSocialCursor`, `FuzzParseComment`, `FuzzStoredFilename` (found finding 4 on its seeds), `FuzzDecodeJSON`, `FuzzUploadOffset`, `FuzzPairingPayload` (new); `FuzzRead` (PROXY), `FuzzReadFrame`, `FuzzHandshake` (relay), `FuzzExtract`, `FuzzDuration`, `FuzzExtractLocation`, `FuzzISO6709`, `FuzzExtractMediaInfo`, `FuzzStillIdentifier`, `FuzzVideoIdentifier` (existing): no crash. |

The `security-checks` CI job (`.github/workflows/ci.yml`) runs
`govulncheck` (linux and darwin), `staticcheck`, `gosec` with the
exclusions above and short fuzz runs on every PR and weekly.

## Brief for an external reviewer

**What it is.** A Go (no cgo) self-hosted photo server for one household,
normally on a Mac mini with an external disk. Apps pin the server's
self-signed key (SPKI SHA-256). From outside the home it is reachable only
through (a) Tailscale Funnel as a raw TCP forwarder to a loopback
listener, TLS end to end, or (b) an outbound tunnel to a relay, TLS end to
end inside. The repository is public (AGPL-3.0).

**Scope.** `server/` (startup, listeners, CLI), `internal/httpapi`
(routes, authz middleware, uploads, social, dashboard), `internal/auth`
(passwords, sessions, rate limits), `internal/authz`,
`internal/meta/sqlitestore` (queries, transactions), `internal/blobstore`
(pool, previews, external tools), `internal/capturetime`,
`internal/livephoto` (parsers), `internal/proxyproto`, `internal/relay` and
`relay/`, `internal/backup`, `internal/tlsid`, `macos/` (menu bar app),
`scripts/`. Out of scope: the mobile apps (private), Tailscale itself.

**How to run.**

```sh
go test -race -count=1 ./...                        # everything (about 5 min)
go test ./internal/httpapi -run 'TestAuthzMatrix'   # the route x role x transport matrix
go test ./internal/httpapi -run '^$' -fuzz '^FuzzParseComment$' -fuzztime 5m
go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...
(cd macos && swift test)
# A local server: docs/INSTALL_MACOS.md; funnel route: docs/FUNNEL.md
```

**Where to look first.**

1. `internal/httpapi/middleware_authz.go` (`routePolicies`, `handleAlbum`)
   and `internal/httpapi/route.go` (`RemoteAllowed`, `RemoteOnly`): is any
   route reachable with less than it should need, on any transport?
2. `internal/auth/session/session.go` (login, refresh rotation and grace,
   revocation) and `internal/auth/ratelimit`: token handling, timing,
   lockout behavior across routes.
3. `internal/httpapi/uploads.go`, `bundles.go`, `httpapi.go`
   (`commitOriginal`) with `internal/blobstore`: upload resume, hash
   checks, commit order, UPLOADER indistinguishability.
4. `internal/capturetime`, `internal/livephoto`, `internal/blobstore/preview.go`,
   `convert.go`: parsing untrusted media in process and through
   `sips`/`ffmpeg`.
5. `internal/meta/sqlitestore/social.go` (`readerCond`, activity) and
   `sync.go`/`bundles.go` (location hiding): data visible to whom.
6. `server/funnel.go`, `internal/proxyproto`, `server/relay.go`,
   `internal/relay`: the internet-facing listeners and the relay protocol
   (`docs/RELAY_DESIGN.md`).
7. `internal/backup/restore.go`: restore of a backup disk.
8. Open items 9–12 above, and whether the accepted ones (13–17) are
   acceptable.

**Not verified by this review**: real Tailscale Funnel (PROXY header
contents, behavior under load), real iPhone/Android pinning, Linux preview
tools, behavior on a real external disk under power loss.
