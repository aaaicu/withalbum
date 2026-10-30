# Sign in with Google or Apple (M54)

An account on the home server can link one Google and one Apple identity and
then sign in with it instead of typing its password. Nothing else changes:
accounts are still created by an administrator (or, later, through an
invitation link), roles and album permissions are the same, and the password
keeps working.

## How it works

1. The app asks the server for a nonce: `POST /api/v1/auth/sign-in/nonce`
   (public; answers the nonce and the providers this server accepts).
2. The app runs the provider's own sign-in on the phone (Google Sign-In,
   Sign in with Apple) with that nonce — Google gets it as is, Apple gets its
   SHA-256 in hex — and receives an **ID token**, a JWT signed by the
   provider.
3. The app sends the token and the nonce to the server.
   - **Link** (once, signed in with the password):
     `POST /api/v1/me/sign-ins {provider, id_token, nonce, current_password}`.
   - **Sign in** (later): `POST /api/v1/auth/sign-in {provider, id_token,
     nonce, device_name, platform}` → the same tokens as a password login.
4. The server verifies the token itself (`internal/auth/oidc`):
   - RS256 signature with the provider's published key
     (`https://www.googleapis.com/oauth2/v3/certs`,
     `https://appleid.apple.com/auth/keys`; fetched on demand, cached for up
     to 6 hours, refetched at most once a minute for an unknown key id);
   - issuer (`https://accounts.google.com` or `accounts.google.com`;
     `https://appleid.apple.com`);
   - audience = one of **this app's client IDs** (below), so a token issued
     to any other app is refused;
   - expiry and issue time (2 minutes of clock skew);
   - the nonce: issued by this server, unused, less than 10 minutes old, and
     named in the token. Every nonce is used up by the first request that
     presents it, whatever the result.
5. The account is found by the provider's stable subject identifier (`sub`),
   **never by e-mail address**. The address in the token is stored only to
   show the account which identity is linked.

## What the server needs

Only the app's **public** client IDs — no secret, key or certificate of
Google or Apple is ever on the home server.

| Setting | Flag | Environment | Default |
|---|---|---|---|
| Google | `-google-client-ids` | `WITHALBUM_GOOGLE_CLIENT_IDS` | empty in source builds (off); release builds may embed the app's IDs with `-ldflags "-X main.defaultGoogleClientIDs=..."` |
| Apple | `-apple-client-ids` | `WITHALBUM_APPLE_CLIENT_IDS` | `aaaicu.withalbum.app` (the app's bundle ID) |

Values are comma-separated; `off` turns a provider off. For Google, list the
iOS client ID and the web ("server") client ID the Android app requests ID
tokens for. For Apple, the bundle ID covers native sign-in on iPhone; a
Services ID is needed only for a web flow (Android, browsers), which the app
does not use yet.

The only outbound connections are the two key set URLs above, made when
someone signs in or links. If they cannot be reached and no recent key is
cached, sign-in answers `503 provider_unavailable` and the password login
still works.

## Security notes

- **Linking needs the current password.** A stolen session or an unlocked
  phone must not be enough to add a lasting way into the account.
- **No account is created by a sign-in.** An identity no account linked gets
  `401 identity_not_linked`.
- A disabled account cannot sign in (`401 invalid_credentials`, as with the
  password). Disabling or a password reset still revokes every session.
- One identity per provider per account, one account per identity
  (`409 provider_linked` / `409 identity_in_use`).
- Unlinking (`DELETE /api/v1/me/sign-ins/{provider}`) keeps the sessions made
  with it; revoke devices to end them.
- A server administrator can see which providers an account linked
  (`GET /api/v1/admin/users/{user_id}/sign-ins`) and remove a link for a
  lost or taken-over Google or Apple account
  (`DELETE /api/v1/admin/users/{user_id}/sign-ins/{provider}`), but never
  add one: linking always needs the holder's password and their own ID
  token.
- Failed sign-ins count against the client address per provider (the login
  rate limiter, remote and home counted separately); they never lock any
  account's password login.
- Nonces are kept in memory (SHA-256 only): at most 32 outstanding per client
  address and 8192 in all, the oldest dropped first; a restart forgets them.
- Audit: `account.identity_link` (allowed / denied with a reason),
  `account.identity_unlink` (the administrator as actor when they unlinked),
  and `auth.login` with `"method": "google"` or
  `"apple"`. Tokens and subject identifiers are never logged.
- Google and Apple learn that this person used the app's sign-in, as with
  any app that offers it. They learn nothing about the home server.

## People (Apple Developer and Google Cloud)

- Apple: enable **Sign in with Apple** for the App ID `aaaicu.withalbum.app`
  and add the capability to the app target. App Store guideline 4.8 requires
  it when another social sign-in is offered.
- Google: in the Google Cloud project of the app, an OAuth client of type
  iOS (bundle ID `aaaicu.withalbum.app`), one of type Android (package and
  signing certificate SHA-1) and one of type Web (its ID is what the Android
  app asks tokens for). Client IDs are not secrets; client secrets are not
  needed and must not be added to this repository.
