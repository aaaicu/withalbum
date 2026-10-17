# Security Policy (prototype)

The current code is **not ready to be exposed to any network**. It has no user authentication, album access controls, CSRF protection, HTTPS, or proven crash-safe metadata transactions.

- It deliberately refuses to bind on non-loopback addresses.
- Do not put a public reverse proxy in front of the local port.
- Do not store unique, irreplaceable or private photos on this prototype.
- Do not disable this guard before authentication, authorization, TLS, restore tests, and security review exist.
- This repository does not yet offer a formal security disclosure channel. Configure GitHub private vulnerability reporting before going public.

**Production security requirements:** unique server identity; initial admin bootstrap; device pairing/revocation; folder-scoped ACLs; rate limiting; safe file parsers; CSRF/session controls; TLS; audit logs; protected stored originals; secret rotation; encrypted backup options; resilient backup restore and rollback.


## Administrator accounts and recovery (console only)

The first administrator can only be created at the server console, never over HTTP:

```bash
WITHALBUM_ROOT=/path/to/root withalbum-server admin init -login <name>
```

`admin init` takes over the locked bootstrap account (`admin`) that owns every folder created before accounts existed, keeping its user id, so those folders keep their owner. It works once; a second run is refused without changes. Passwords are Argon2id hashes (PHC string with its parameters); they are typed twice on the terminal without echo, or read as one line from stdin when stdin is not a terminal. They are never accepted as command-line arguments and never printed or logged. Avoid passing a password through `echo`/`printf` in an interactive shell, because shells may keep it in their history; use the terminal prompt or a file with restricted permissions.

**Lost administrator password:** stop the server (the admin commands need the storage lock and refuse with `LOCKED` while it runs), then at the server console run

```bash
WITHALBUM_ROOT=/path/to/root withalbum-server admin reset-password -login <name>
```

Anyone with console access to the server and write access to the storage can do this; protect the machine and its user account accordingly. Every account change is recorded in the `audit_log` table (without passwords or hashes). Login sessions do not exist yet; when they do, a reset will also revoke them.

## v1.4 계정/앨범 권한 설계 상태

`internal/authz`는 **권한 정책을 검증하기 위한 독립 라이브러리**입니다. **현재 HTTP 서버에서 그 정책을 사용하고 있지 않습니다.** 계정 생성·비밀번호 해시는 콘솔 명령(`admin`)으로만 구현되어 있고, HTTP 로그인·토큰, 초대 및 ACL·TLS는 아직 미구현입니다. 이 프로토타입은 루프백 전용으로 개발자만 시험해야 하며, 인터넷/LAN에 포트를 공개하거나 실사용 원본의 유일 저장소로 사용하면 안 됩니다.

최종 정책은 [ACCESS_CONTROL_v1.4.md](docs/ACCESS_CONTROL_v1.4.md) 참조. 실제 릴리스에 앞서 HTTP 접근 제한, 각종 다운로드 IDOR 및 권한 회수, 미리보기 우회, 업로드 전용 정보 누출에 대한 통합 검증이 필요합니다.
