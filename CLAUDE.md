# CLAUDE.md — 위드앨범 개발 에이전트의 작업 원칙

<!-- Claude Code에게: 이 문서는 프로젝트 루트의 지속 규칙이다. 작업마다 README.md 및 최신 사용자 변경사항과 함께 읽을 것. -->

## 프로젝트 본질

- 제품: **위드앨범 / WithAlbum**. 임시 옛 이름 `PhotoHub`는 더 이상 공식 제품명이 아님.
- 최고 우선순위: **Apple Silicon Mac mini + 외장 SSD/HDD에서 안전하게 구동되는 셀프호스팅 서버**. 같은 Go 코어를 Linux AMD64/ARM64, Pi ARMv7, NAS로 이식.
- 별도 서버 없는 사용자: **Google Drive**는 iOS/Android `GoogleDriveAdapter`로 제공할 *계획*, 현재 코드 없음.
- 모바일 UX: iOS 기본 사진/Android 갤러리의 공유 버튼 → 위드앨범 앱 → 앨범 선택 → 동기화 → 원격 원본 검증 후 앱 내부 사본을 경량화. 기본 사진 앱 원본 삭제 안 함.
- 정방향 + 역방향 갤러리, SHA-256 CAS, 원본 무변환 보관, 원본 요청시 다운로드, 안전한 큐.
- P0: 서버 계정·폴더 소유권·초대·VIEWER/UPLOADER/CONTRIBUTOR 권한; 외장하드/SQLite 무결성과 복구.

## 신뢰도 및 기준 문서

1. 현재 사용자의 명시적 요구
2. 루트 [`README.md`](README.md) — 최신 통합 사용/설계/코드 상태와 **HTML 주석** 모두 읽기
3. [`docs/PRODUCT_ARCHITECTURE_v1.4.md`](docs/PRODUCT_ARCHITECTURE_v1.4.md), [`docs/ACCESS_CONTROL_v1.4.md`](docs/ACCESS_CONTROL_v1.4.md)
4. `docs/STORAGE_ARCHITECTURE_v1.3.md`, `docs/CLIENT_SYNC_STATES.md`, `docs/GOOGLE_DRIVE_MODE_v1.3.md`
5. 과거 v1.2/v1.3/아카이브 (결정 변경의 맥락 확인용, 최신 설계 대체 금지)

## 작업 시작 절차

1. `git status`, `find`/tree, `go test ./...`, `go vet ./...`로 상태를 점검하고 관련 문서를 읽는다.
2. '현재 실제로 동작하는 기능'과 '목표 규격만 존재하는 기능'을 분리해 설명한다.
3. 구현 계획/영향 범위/테스트 계획을 작성하고 **작고 검증 가능한 변경 단위**로 작업한다.
4. 안전한 DB 및 파일 저장, 로그인·ACL을 먼저 완성한다. 모바일·Drive는 기본 아키텍처를 깨지 않도록 경계만 설계하고 우선 구현하지 않는다.
5. 변경마다 테스트를 추가하고 `go test -race ./...`, `go vet ./...`, `gofmt`를 수행. Mac/Pi 빌드 통과만으로 실기기 동작했다고 말하지 않는다.
6. README/Feature Status/관련 API·스키마 문서를 코드 변경과 동시에 갱신한다. 체크박스는 통합 검증을 거쳐서만 완료로 바꾼다.

## 현재 코드의 진짜 상태 (절대 혼동 금지)

- `server/main.go`(기동) + `internal/httpapi`(핸들러) + `internal/meta/sqlitestore` + `internal/blobstore`: Go 서버. 기본 localhost 평문 HTTP, LAN은 `-allow-lan` + `-tls`(자체 서명, `.system/tls/server.pem` 0600, TLS 1.3, SPKI SHA-256 핀) + 로그인 가능한 SERVER_ADMIN이 모두 있을 때만(`server/listen.go` `checkListen`, 사설 IP·와일드카드만, 공인 IP·호스트명 거부; 테스트·LAN 바이너리 스모크 검증, 앱 핀닝 실기기 미검증). 메타데이터는 SQLite `.system/withalbum.db`(`meta.MetadataStore` 인터페이스 뒤). 512MiB 단일 요청 원본 업로드/초보적 프리뷰. **API 자산 ID는 폴더별 랜덤 asset_id**(SHA-256은 내부 중복 제거·`sha256` 필드). 새 폴더 소유자는 생성자, 이전·이관 폴더는 bootstrap 관리자 계정(`account_bootstrap.user_id`, 처음엔 로그인 불가한 잠금 `admin`, `admin init`으로 인수해도 user_id 유지).
- `internal/httpapi/middleware_authz.go`: 모든 라우트의 접근 정책 표(public 4개 = 대시보드 페이지·health·login·refresh / session / server+액션 / album+액션). 미선언 라우트는 핸들러 구성 시 panic. album 라우트는 매 요청 DB 소유자·멤버십 → `authz.Allowed`, 비멤버(관리자 포함) 404 · 역할 부족 403. 업로드는 커밋 직전 트랜잭션 안에서 재확인(`meta.CommitRequest.Authorize`). UPLOADER 응답은 항상 202 영수증. HTTP E2E 테스트(`authz_test.go`)·로컬 바이너리 스모크로 검증.
- `internal/meta/sqlitestore/memberships.go`(`meta.Memberships`) + `internal/httpapi/members.go`: 초대(활성 등록 계정, 7일, 1회, 취소, 토큰 없음)·수락(대상 본인, 1 트랜잭션)·역할 변경·추방·소유권 이전(현재 멤버에게, 이전 오너 CONTRIBUTOR, 대기 초대 취소)·보관. OWNER만, 쓰기마다 트랜잭션 안 재판정 + 같은 트랜잭션 감사 로그. HTTP E2E 테스트(`members_test.go`)·실패 주입·동시성 테스트·로컬 바이너리 스모크로 검증. 초대 거절·자진 탈퇴·보관 복원은 없음.
- `internal/meta/jsonstore`: 서버 기동에 쓰이지 않음, `MetadataStore` 미구현(테스트에서 이전 버전 데이터 생성용으로 고정). state.json만 있는 저장소는 기동 거부.
- `internal/legacyimport` + `withalbum-server migrate-legacy [-apply]`: state.json → SQLite 무손실 이관(dry-run 기본, 백업·임시 DB 단일 트랜잭션·전 행 검증·원자적 rename·state.json 보존). 테스트와 로컬 스모크(이전 버전 바이너리로 만든 데이터)로 검증, 대용량·외장 디스크 실기기 이관은 미검증.
- 업로드 커밋 순서·기동 재조정(`blobstore.Reconcile`, `server/main.go` `openApp`)·여유 공간 507·단일 인스턴스 잠금: 장애 주입·SIGKILL 테스트로 검증, 전원 차단·실기기 미검증.
- `internal/auth/password`(Argon2id) + `internal/users`(사용자 저장소, 마지막 관리자 보호, 감사 로그 `internal/audit`) + `server/admin.go`(`admin init`=bootstrap 인수·`create-user`·`reset-password`, 콘솔 전용, 저장소 잠금 필요): 테스트·로컬 바이너리 스모크로 검증. 비밀번호 재설정·비활성화는 세션을 같은 트랜잭션에서 회수.
- `internal/auth/session`(기기 세션, access 15분/refresh 30일 회전·재사용 시 세션 폐기, 토큰 SHA-256만 저장) + `internal/auth/ratelimit`(메모리 속도 제한) + `password.Limiter`(Argon2id 동시 2개, 더미 해시로 열거 방지) + `internal/httpapi/auth.go`·`middleware_auth.go`(`/api/v1/auth/*`, `/api/v1/me*`, `/api/v1/admin/users*`, `RequireSession`, 웹 쿠키+CSRF): 테스트·로컬 바이너리 스모크로 검증.
- `internal/authz`: 권한 함수와 테스트. 모든 관리자·앨범 라우트에서 사용.
- `internal/db`: SQLite 열기·PRAGMA·마이그레이션(0001 schema v1.4, 0002 이관 보조, 0003 `assets.preview_ready`, 0004 `account_bootstrap`, 0005 세션 토큰 컬럼·`auth_refresh_used`, 0006 대기 초대 부분 UNIQUE 인덱스, 0007 `sync_state`·`pruned_seq`·`upload_receipts` 재생성·refresh 유예 컬럼). `docs/schema_v1.4.sql`은 마이그레이션 0001과 동일(정본은 `internal/db/migrations/`). users·audit_log·devices·auth_sessions·folder_members·folder_invitations 사용(`token_sha256`은 미사용, NULL).
- `docs/API_v2.yaml`(2.0.0-draft, M16): **유일한 공식 API 규격**, 모든 경로 구현됨. `internal/httpapi/openapi_test.go`가 라우트 정책 표와 경로·메서드·공개 여부·401/403/404 문서화를 대조. `docs/API_AUTH_v1.4.yaml`·`docs/API_v1.2.yaml`은 폐기(역사).
- sync API v2(M16): `internal/httpapi/sync.go` + `internal/meta/sqlitestore/sync.go`. cursor는 `<폴더 epoch 16hex>.<seq>` 문자열(epoch = DB `sync_state.epoch`+folder_id 해시, `restore`가 교체) — 다른 폴더·복원 전·앞선 cursor 409 `cursor_invalid`, 보존 범위 밖 409 `cursor_expired`(`folder_sequences.pruned_seq`, 현재 이벤트 삭제 없음). tombstone `asset_removed`(`DELETE /folders/{fid}/assets/{aid}`, OWNER만, `folder_assets.removed_at`, 원본 파일 유지), 스냅샷 `GET /folders/{fid}/assets`, 폴더 범위 `POST .../assets/check`(UPLOADER 403), 선택적 ack `PUT .../sync/cursor`. 업로드는 `Idempotency-Key` 필수, `upload_receipts`(user·device·folder·key PK, 마이그레이션 0007에서 재생성)를 매 커밋(already_present 포함) 같은 트랜잭션에 기록, 재시도는 저장된 결과 재응답(`Idempotency-Replayed: true`), 다른 바이트 422. HTTP 테스트(`sync_v2_test.go`: CLIENT_SYNC_STATES 시나리오 2·3·6·7·8)·로컬 바이너리 스모크로 검증.
- 인증 후속(M16): refresh 30초 유예(앱만, 직전 교환 1회, `auth_refresh_used.used_at/grace`), 절대 만료 90일, 로그인 성공 시 `NeedsRehash` 재해시, `session.Prune`+영수증 30일 정리(`server/maintenance.go`, 6시간마다), `GET|DELETE /admin/users/{uid}/devices*`, 본인 비밀번호 변경 실패 감사, 권한 거부(403/404) 요약 감사(`internal/httpapi/denials.go`, 계정·라우트·상태당 10분에 1행).
- 관리 웹 UI(M17): `internal/httpapi/web/index.html`(정적 HTML/JS 1개, 기존 API만 사용, DOM은 textContent만) + `internal/httpapi/admin.go`(`GET /admin/server/storage` 저장소·여유 공간·기동 재조정 개수, `GET /admin/users/{uid}/folders` 사용 중 개수+보관 앨범 ID, `POST /admin/folders/{fid}/owner` **보관 앨범만** 관리자 재배정 — 1 트랜잭션·재판정·감사 `folder.reassign_archived_owner`, 사용 중 앨범은 404, 보관 유지라 아무도 열람 불가) + `internal/meta/sqlitestore/admin_folders.go`. 대시보드 CSP는 인라인 script/style SHA-256 해시(`security.go` `dashboardCSP`, 페이지에서 계산, `'unsafe-inline'` 없음)+Trusted Types. httptest(`admin_test.go`)·로컬 바이너리 스모크로 검증, 브라우저 수동 점검은 `docs/DASHBOARD_CHECKLIST.md`(사람이 수행).
- `internal/integrity`(`verify [-hash]`, 읽기 전용) + `internal/backup`(`backup -to`: `VACUUM INTO` 스냅샷·원본 증분·SHA-256 매니페스트, 서버 실행 중 가능 / `restore -from`: 전수 검증·스테이징·세션 전부 폐기·저널·원자적 교체·기존 데이터 보존, 서버 중지 필요) + `server/maint.go`: 테스트(업로드 중 백업, 복원 후 권한 동일성, 단계별 크래시 재실행, 손상 백업 거부)·로컬 바이너리 스모크로 검증. 실기기·대용량 미검증. 절차 `docs/BACKUP_RESTORE.md`.
- Flutter/iOS/Android, OAuth Drive, 외부 통신, 실시간 푸시, GUI DMG, 원격/클라우드 백업·백업 스케줄러는 없음.
- README v1.5는 **기획·인수인계 정리판**이고 새 기능 릴리스가 아님.

## 보안 불변식 (위반하는 변경 금지)

- non-loopback listen은 `-allow-lan` + TLS + 로그인 가능한 SERVER_ADMIN이 모두 있을 때만(M15). 평문 HTTP는 loopback만. 이 게이트 완화·인터넷 공개 금지.
- 기본 서버 USER는 폴더 생성 금지(`can_create_albums=false`). 서버 관리자라도 타인 앨범 데이터 API 자동 열람 금지. 관리자의 소유자 재배정은 보관된 앨범에만(보관 유지, 열람권 없음, M17 사용자 승인) — 사용 중 앨범으로 넓히지 말 것.
- 각 앨범 단일 OWNER 보장, 소유권 이전 트랜잭션 및 로그. 초대는 기존 등록 계정에 대상 지정/수락 필요.
- UPLOADER: 업로드만, 목록·이벤트·미리보기·원본·전역 해시 존재 정보 금지. VIEWER는 읽기만. CONTRIBUTOR는 읽기+업로드, 타인 삭제·초대 불가.
- API의 자산 다운로드는 소속 앨범과 실제 권한을 재검증. 권한 변경은 즉시 후속 요청에 반영. 업로드 commit 직전 재확인.
- 비밀번호는 Argon2id, 토큰은 고엔트로피·기기별 해지; 웹 쿠키 HttpOnly/Secure/SameSite + CSRF, 검증된 TLS 필요.
- 외장 디스크가 빠지면 기본 로컬 디스크에 같은 경로를 생성해서 저장하면 안 된다. 경로·볼륨 식별 실패 시 차단(fail closed).
- 업로드 후 앱의 원본 임시 사본 삭제는 원격 원본 무결성 + 미리보기 준비를 확인한 뒤만. 기본 Photos/Gallery 원본을 자동 삭제하지 않는다.
- Google Drive `writer`를 '업로드 전용'이라고 표현하지 않는다. 서비스가 지원하지 않는 기능은 UI에서 안내한다.

## 구현을 우선 진행할 작업

**Step 0 — 보안·데이터 감사:** 현재 코드의 DB/파일 동시성, 에러 처리, 파일 ID/다운로드 범위, 실패 복구, 스토리지 unmount 리스크 목록화.

**Step 1 — 내구성:** SQLite 실제 적용 및 마이그레이션, 데이터 스키마 버전, 블롭/앨범 트랜잭션, 크래시 복구, 마운트 검증, 백업/복구 테스트.

**Step 2 — 로그인·권한:** 초기 관리자 등록, Argon2id, 계정/세션 관리, 서버/앨범 역할, invitation 및 transfer, 미리보기·원본·이벤트 접근 통제 + IDOR 테스트.

**Step 3 — Mac-first 검증:** Apple Silicon/외장 저장장치 실기기 E2E, 설치/자동 시작, 서명/공증 패키징 계획.

**Step 4 — 모바일/다중 저장:** Flutter 앱 코어 + Share Sheet/Android Intent, 자체 서버 adapter, 양방향 동기화, 그다음 Google Drive PoC.

## 반환할 작업물 형식

각 작업 완료 후 다음을 보고한다: (a) 변경 파일 (b) 설계 선택 근거 (c) 실제 통과한 명령·테스트 (d) 미검증 내용과 남은 위험 (e) 다음 작업. 성공하지 않은 테스트를 '완료'라고 하지 말 것.

## 사람이 결정해야 하는 사항

공개 GitHub 저장소 경로 및 모듈명, 최종 라이선스, 앱 서명/Apple 개발자 계정/Google OAuth 프로젝트, 최초 배포 대상과 보안 신고 주소는 근거 없이 만들어 넣지 않는다. 계정 비밀번호나 클라우드 자격증명, 사용자 실제 사진을 저장소에 커밋하지 않는다. 안전하고 되돌릴 수 있는 로컬 구현·테스트는 진행하되, 실제 데이터 삭제/공개 배포/원격 계정 생성은 사용자 확인 후 처리한다.
