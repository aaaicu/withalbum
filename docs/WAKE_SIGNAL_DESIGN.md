# New-photo notifications, step 2: content-free wake signals (M39 design)

Status: **design only, deferred.** Step 2 is on hold: it needs a gateway that
runs around the clock, and step 1 is enough for now. It will be revisited if
late notifications on iOS turn out to be a real problem. Step 1 (shipped in the app) needs
no server change: the phone wakes up on its own schedule (iOS
BGAppRefreshTask, Android WorkManager every 30 minutes), asks its home server
which albums changed (`GET /api/v1/sync/state`, M53), pulls only those, and
posts a local notification ("가족 여행 · 새 사진 3장이 올라왔어요") that it
composed itself. The operating system decides when the phone wakes, so a
notification can come minutes or hours late. Step 2 adds a push that only says
"wake up now"; everything the user reads is still fetched from the home server
by the phone.

## Goals and non-goals

- **The push carries no content.** No album name, no count, no uploader, no
  folder or asset ID, no server address. Apple (APNs) and Google (FCM) learn
  only that some device got a wake-up at some time.
- **The home server keeps no Apple or Google secret.** It cannot: every home
  server would need the app developer's APNs key and FCM credentials, which
  would then be on thousands of machines.
- **The gateway learns as little as the relay does** (`RELAY_DESIGN.md`): it
  forwards opaque wake requests and never sees requests, photos or accounts.
- **Off unless the user turns notifications on**, and the step 1 schedule keeps
  working when the gateway is down or not configured.
- Non-goal: notifications for comments, likes or mentions (later, same channel).

## Parties

```
 home server ──(1) wake <wake_token>──▶ push gateway ──(2) APNs / FCM──▶ phone
      ▲                                  (developer-run,                   │
      └──────────(3) GET /sync/state, GET .../sync (pinned TLS, LAN/relay/funnel)
```

| Party | Holds | Sees |
|---|---|---|
| Push gateway (operated like the relay, by the app developer) | APNs auth key, FCM service account; its own sealing key pair | wake tokens, the time of each wake, the home server's IP (or the relay's, if wakes go through the relay tunnel) |
| Home server | per device: an opaque wake token and the device's notification choices | which of its devices to wake, as today it knows which devices sync |
| Apple / Google | device push tokens (as for any app) | that a push with no user-visible content from this app arrived |
| Phone | its push token, the gateway's public key | everything, fetched from its own server |

## Flow

1. **Registration (phone).** The app gets its APNs device token (iOS) or FCM
   registration token (Android). It seals the token for the gateway (an X25519
   sealed box with the gateway public key built into the app, plus the
   platform and app bundle ID) and gets a *wake token*: an opaque blob only the
   gateway can open. The app sends it to its home server:
   `PUT /api/v1/me/devices/current/wake {"wake_token": "...", "muted_folders": [...]}`
   (session, served through relay and funnel). `DELETE` removes it; logging out
   or revoking the device removes it too. The server stores it on the
   `devices` row, never logs it, and never returns it.
2. **Trigger (home server).** After a commit that a reader should hear about
   (asset added or a hidden asset shown, by someone else), the server picks the
   devices of accounts that can read the folder now (OWNER, CONTRIBUTOR,
   VIEWER; never UPLOADER), skips the uploading device and devices that muted
   the folder, and coalesces: at most one wake per device per 5 minutes, sent
   after a 30 second quiet period so a burst of 200 uploads is one wake.
3. **Wake (gateway).** `POST /v1/wake {"tokens": [...]}` from the home server.
   The gateway opens each wake token, rate-limits per token (for example 20 a
   day) and per source address, and sends:
   - iOS: an alert push with `mutable-content: 1`, a fixed placeholder body
     ("새 소식이 있어요") and no custom keys. The app's Notification Service
     Extension replaces the body after fetching (below); if it cannot reach the
     home server in its ~30 seconds it leaves the placeholder.
   - Android: a high-priority FCM *data* message with no data fields. The
     app's messaging service runs briefly, fetches, and posts a local
     notification (or nothing, if there is nothing new for this device).
   The gateway answers which tokens Apple/Google reported as unregistered; the
   home server drops those.
4. **Fetch (phone).** The extension/service does exactly what step 1's
   background check does: `GET /sync/state`, pull the changed albums, compute
   the new photos with the local new-photo tracker, compose the text. The same
   SPKI pin and route selection (LAN, relay, funnel) apply.

## Why a wake token instead of the raw push token

- The home server never handles the APNs/FCM token, so a compromised home
  server cannot push to the phone through any other path, and cannot link the
  phone to other services that know its push token.
- The gateway needs no account database: everything it needs is in the sealed
  token, and it can rotate its sealing key by accepting old and new keys for a
  while (the app re-registers on launch).
- Abuse is bounded per token: a stolen wake token lets its holder wake one
  phone a limited number of times, with a placeholder text.

## Threat notes

| Actor | Could | Mitigation |
|---|---|---|
| Gateway operator | Know how often a device is woken, from which home server IP | Coalescing; wakes may go through the relay tunnel so the gateway sees the relay's address; no content exists to leak |
| Apple / Google | Same timing metadata as any push | Content-free payload; Android data message without fields |
| Network attacker | See home server → gateway traffic | TLS to the gateway with system roots |
| Someone with a wake token | Wake the phone repeatedly | Per-token daily cap at the gateway; placeholder text only; the home server can rotate by re-registration |
| Compromised home server | Wake its own users' phones | Same cap; the phone shows only what that server already serves |

## iOS specifics

- Notification Service Extension target `aaaicu.withalbum.app.NotificationService`,
  sharing the app group already used by the share extension for the
  connection record and the pinned TLS identity. It runs the fetch in Swift
  (no Flutter engine in an extension): `GET /sync/state` and the feeds of the
  changed albums, counting `asset_added` events not from this device since its
  stored cursors. The extension must not advance the app's cursors (the app's
  index is not shared); it keeps its own small "already announced" record.
- An extension cannot fully drop a notification without the filtering
  entitlement, so the server-side mute (step 1 of the flow) matters on iOS.
- Needs: APNs auth key (`.p8`) on the gateway only, the Push Notifications
  capability and the extension target in the app. **People:** the Apple
  Developer account holder creates the key; it is never committed.

## Android specifics

- `FirebaseMessagingService` receives the data message, enqueues an expedited
  WorkManager job that runs the existing background check (`purpose:
  "notify"`), and returns. No Google Play services → no FCM; step 1 still
  works. An optional UnifiedPush distributor could replace FCM later with the
  same wake-token idea.
- Needs: a Firebase project, `google-services.json` kept out of git (like
  `GoogleService-Info.plist`), the FCM service account on the gateway only.

## Server work (when approved)

- Migration: `devices.wake_token` (nullable), `device_notify_mutes(device_id,
  folder_id)`.
- Routes: `PUT|DELETE /api/v1/me/devices/current/wake`, added to the remote
  allowlist; `server_features` `wake_signals`.
- A wake dispatcher next to the preview worker: hooks on the commit paths that
  already append `asset_added`, a per-device coalescing timer, a gateway
  client with a configurable URL (`-push-gateway`, off by default) and
  backoff. Nothing is sent when no gateway is configured.
- The gateway itself: a small separate program like `withalbum-relay`
  (`withalbum-pushgw`), stateless apart from rate-limit counters.

## Open decisions (for the project owner)

1. Who runs the gateway, on which host, and whether the relay host doubles as
   the gateway.
2. Placeholder text on iOS when the home server cannot be reached.
3. Whether to include comments, likes and mentions in the same wake
   (recommended: yes, as a second trigger kind, still content-free).
