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

Anyone with console access to the server and write access to the storage can do this; protect the machine and its user account accordingly. Every account change is recorded in the `audit_log` table (without passwords or hashes). A reset signs out every device of the account (all its sessions are revoked in the same transaction); disabling an account does the same.

## v1.4 계정/앨범 권한 설계 상태

`internal/authz`의 정책은 HTTP 서버의 모든 관리자·앨범 라우트에서 매 요청 적용됩니다(`internal/httpapi/middleware_authz.go`). 최초 관리자는 콘솔 명령(`admin init`)으로만 만들 수 있고, 대시보드 페이지·health·login·refresh 외 모든 API는 세션이 필요합니다. 서버 관리자도 소유·멤버가 아닌 앨범의 데이터는 읽을 수 없습니다(404). 초대·역할 변경·추방·소유권 이전은 앨범 OWNER만 할 수 있고 같은 트랜잭션에서 감사 로그에 남습니다. TLS는 미구현입니다. 이 프로토타입은 루프백 전용으로 개발자만 시험해야 하며, 인터넷/LAN에 포트를 공개하거나 실사용 원본의 유일 저장소로 사용하면 안 됩니다.

최종 정책은 [ACCESS_CONTROL_v1.4.md](docs/ACCESS_CONTROL_v1.4.md) 참조. 실제 릴리스에 앞서 HTTP 접근 제한, 각종 다운로드 IDOR 및 권한 회수, 미리보기 우회, 업로드 전용 정보 누출에 대한 통합 검증이 필요합니다.
