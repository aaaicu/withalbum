# Changelog / 변경 기록

## [Unreleased]

- 최소 Go 버전 1.23 → 1.26 (지원 종료 버전 탈피), modernc.org/sqlite v1.39.0 → v1.60.1 (SQLite 3.53.4).

### Added (M11, album sharing)
- 초대·멤버·소유권 이전·보관 API(OWNER만, 멤버 403·비멤버/관리자 404): `GET /api/v1/folders/{id}/members`, `PATCH|DELETE /api/v1/folders/{id}/members/{uid}`, `POST /api/v1/folders/{id}/owner`, `GET|POST /api/v1/folders/{id}/invitations`, `DELETE /api/v1/folders/{id}/invitations/{iid}`, `DELETE /api/v1/folders/{id}`(보관). 초대받은 계정: `GET /api/v1/invitations`, `POST /api/v1/invitations/{iid}/accept`. 규격: `docs/API_AUTH_v1.4.yaml`(1.4.3-draft).
- 쓰기마다 BEGIN IMMEDIATE 트랜잭션 안에서 계정·세션·소유자·멤버십을 다시 읽어 재판정하고 감사 로그(`invitation.create|accept|revoke`, `member.role_change|remove`, `folder.transfer_owner|archive`)를 함께 커밋. 초대는 활성 등록 계정만(없음·비활성·잠금은 같은 404), 7일 만료(조회 시 판정), 대상·앨범당 대기 1개, 토큰 없음(수락에 대상 본인 세션 필요). 소유권 이전은 현재 멤버에게만, 이전 오너는 CONTRIBUTOR, 대기 초대 취소.
- 마이그레이션 0006: 대기 초대 부분 UNIQUE 인덱스(`folder_id, invited_user_id WHERE status='pending'`)와 폴더별 인덱스. `authz.ListMembers` 액션(OWNER만).

### Changed (breaking, API)
- 앨범 ACL(M10): 대시보드 페이지·`/api/v1/health`·`/api/v1/auth/login`·`/api/v1/auth/refresh` 외 모든 경로가 세션 필요(없거나 무효하면 401, 알 수 없는 `/api/v1/*` 경로도). 폴더·자산 라우트는 요청마다 DB의 소유자·멤버십으로 판정: 소유·멤버가 아닌 폴더는 SERVER_ADMIN이라도 404 `folder_not_found`, 역할 부족은 403 `forbidden`. 라우트별 접근 정책은 `internal/httpapi/middleware_authz.go` 표 하나에만 선언하며 미선언 라우트는 핸들러 구성 시 panic.
- `POST /api/v1/folders`는 SERVER_ADMIN 또는 `can_create_albums`만(아니면 403), 만든 사람이 유일한 OWNER. `GET /api/v1/folders`는 내가 소유하거나 멤버인 폴더만 `role`·`can_read`·`can_upload`와 함께 반환. `GET /api/v1/folders/{id}` 추가(UPLOADER 포함 멤버에게 최소 메타데이터).
- 자산 ID가 SHA-256에서 폴더별 랜덤 ID로 바뀜(같은 바이트도 폴더마다 다른 자산·파일명). 다운로드는 `GET /api/v1/folders/{fid}/assets/{aid}/original|preview`만, 그 폴더에 연결된 자산이 아니면 404. **무인증 `/api/v1/assets/{sha}/original|preview` 제거.** sync의 `assets`는 자산 ID 키, 각 자산에 `sha256`.
- 업로드 응답: 읽기 권한자는 201 `created`/200 `already_present`(다른 폴더에 같은 바이트가 있어도 `linked`가 아닌 `created`), UPLOADER는 항상 202 `{"result":"accepted","folder_id","sha256","bytes"}`. 커밋 직전 DB 트랜잭션 안에서 계정·세션·멤버십을 다시 읽어 권한 재확인(업로드 중 회수 시 403/404, 기록 없음). 이미 있는 바이트도 pool 파일 재검증·프리뷰 재생성을 똑같이 수행.
- 로컬 대시보드가 로그인(웹 쿠키 세션 + CSRF)을 요구하고 내 앨범만 표시.
- `internal/meta/jsonstore`는 더 이상 `meta.MetadataStore`를 구현하지 않음(이전 버전 데이터 생성용으로 고정).

### Added
- 로그인·기기 세션·계정 관리 API (loopback 전용): `POST /api/v1/auth/login|refresh|logout`, `GET /api/v1/me`, `POST /api/v1/me/password`, `GET /api/v1/me/devices`, `DELETE /api/v1/me/devices/{id}`, `GET|POST /api/v1/admin/users`, `GET|PATCH /api/v1/admin/users/{id}`, `PATCH /api/v1/admin/users/{id}/permissions`. 규격: `docs/API_AUTH_v1.4.yaml`(1.4.1-draft).
- `internal/auth/session`: 256비트 랜덤 토큰(SHA-256만 저장), access 15분·refresh 30일 회전, refresh 재사용 시 세션 전체 폐기, 기기별 해지, 매 요청 계정 재조회. 웹은 HttpOnly·SameSite=Strict 쿠키 + CSRF 토큰(`X-CSRF-Token`).
- `internal/auth/ratelimit`: 로그인 속도 제한(IP 토큰 버킷, 계정+IP·계정 실패 횟수) → 429 + Retry-After. `password.Limiter`: Argon2id 동시 계산 2개 제한(초과 대기 시 503 busy), 없는 계정도 더미 해시로 같은 비용.
- 감사 로그: 로그인 성공/실패(사유), 제한 시작, 로그아웃, 기기 해지, refresh 재사용, 비밀번호 변경.
- 비밀번호 재설정(콘솔·관리자)과 계정 비활성화가 그 계정의 모든 세션을, 본인 비밀번호 변경이 다른 세션을 같은 트랜잭션에서 폐기.
- 마이그레이션 0005: `auth_sessions`에 access 토큰 해시·만료·CSRF 해시·회전 시각·폐기 사유, `auth_refresh_used`(교환된 refresh 해시).
- 계정 코어(**콘솔 전용, HTTP 로그인 없음**): `internal/auth/password` Argon2id(PHC, 기본 m=64MiB·t=3·p=2, 상수시간 검증, 잠금 값 `!` 거부, 재해시 판정, 비밀번호 최소 12자·최대 1024바이트), `internal/users` 사용자 저장소(마지막 활성 관리자 비활성·강등 거부, 소유 앨범 있는 계정 비활성 거부), `internal/audit` 감사 로그 기록(같은 트랜잭션, 비밀 키 거부).
- `withalbum-server admin init | create-user | reset-password`: 최초 관리자는 콘솔에서 잠금 bootstrap 계정을 인수(user_id 유지 → 기존·이관 폴더 소유 유지, 1회만). 비밀번호는 에코 없는 TTY 2회 입력 또는 stdin 한 줄, 명령행 인자 불가. 저장소 잠금이 필요해 서버 실행 중에는 `LOCKED`로 거부.
- 마이그레이션 0004 `account_bootstrap`: bootstrap 관리자를 로그인 이름 대신 user_id로 식별(기존 DB의 잠금 `admin`을 자동 기록).
- 의존성 `golang.org/x/crypto` v0.58.0, `golang.org/x/term` v0.47.0 (BSD-3-Clause), `golang.org/x/sys` v0.48.0 → v0.49.0.
- 저장소 볼륨 가드 `internal/storage`: 루트 직하 볼륨 마커 `.withalbum-volume.json`(format 1, 랜덤 volume_id)을 기동 시 검증하고, 업로드·폴더 생성 직전에 마커와 루트 장치 번호(st_dev)를 재확인한다. **부분 구현, 실기기 미검증.**
- `-init-storage` 플래그: 이미 존재하는 디렉터리에 마커만 원자적으로 기록(기존 `.system/` 데이터 불변).
- 선택 환경변수 `WITHALBUM_VOLUME_ID`: 지정한 볼륨이 아니면 기동 거부.
- `/api/v1/health` 응답에 `storage: ok|unavailable`.
- SQLite 기반 `internal/db` (**아직 HTTP 미연결**): cgo 없는 드라이버 `modernc.org/sqlite` v1.39.0(SQLite 3.50.3), 모든 연결에 `foreign_keys=1`·`journal_mode=WAL`·`synchronous=FULL`·`busy_timeout=5000` 강제, 명시적 트랜잭션은 `BEGIN IMMEDIATE`. `PRAGMA user_version` 기반 내장 마이그레이션(0001 = schema v1.4, 0002 = 레거시 이관 보조 테이블 `legacy_imports`·`legacy_asset_map`·`uploader_device_label` 컬럼), 상위 버전 DB 거부, 적용 전 `integrity_check`.

- SQLite 메타데이터 저장소 `internal/meta/sqlitestore`: 서버가 `.system/withalbum.db`(모드 0600)를 사용한다. 업로드 1건의 blob upsert·자산(랜덤 내부 `asset_id`)·폴더 연결·폴더 시퀀스 증가·변경 이벤트를 `BEGIN IMMEDIATE` 트랜잭션 하나로 커밋하고, 전역 mutex 대신 SQLite 쓰기 락으로 직렬화한다. 읽기는 트랜잭션을 열지 않는다.
- 로컬 bootstrap 관리자 계정(`admin`, 로그인 불가한 잠금 비밀번호 값, SERVER_ADMIN): 계정 기능 전까지 모든 폴더의 소유자.
- 마이그레이션 0003: `assets.preview_ready`.
- `withalbum-server migrate-legacy [-apply] [-json]` (`internal/legacyimport`): 이전 버전 `.system/state.json`을 SQLite로 무손실 이관. 기본은 점검만(state.json 엄격 파싱·일관성 검사, 모든 원본 크기·SHA-256 재계산, 프리뷰 존재 확인, 보고서). `-apply`는 `state.json.bak-<UTC>` 백업 → `withalbum.db.importing`에 스키마+전체 행+`legacy_imports`를 트랜잭션 하나로 기록 → 재오픈 후 전 행 대조 검증 → `withalbum.db`로 rename → state.json을 `state.json.migrated-<UTC>`로 rename(삭제 안 함). 결함 입력은 아무것도 쓰지 않고 거부, 중단 시 재실행으로 이어가거나 처음부터 다시 함. 절차: `docs/MIGRATION_legacy_state.md`.
- `db.MigrateNewTx`: 새 DB의 스키마 생성을 호출자 트랜잭션 안에서 수행.

- 업로드 원자적 커밋·크래시 복구 (**부분 구현, 실기기 미검증**): 성공 응답은 임시 파일 fsync → pool rename → pool 디렉터리 fsync → DB 트랜잭션이 모두 끝난 뒤에만 보낸다. DB 트랜잭션 안에서 COMMIT 직전에 볼륨과 pool 파일을 재확인한다(`meta.CommitRequest.BeforeCommit`).
- 기동 재조정 `blobstore.Reconcile`: `.system/incoming`의 미완료 업로드·프리뷰 임시 파일 삭제, DB에 없는 pool 파일은 `.system/orphans/`로 격리(삭제 안 함), DB에 있으나 pool에 없거나 크기가 다른 원본은 로그 `INTEGRITY ERROR`와 health `integrity: warning`. 결과는 로그 `recovery: ...` 한 줄 요약.
- 여유 공간 사전검사: 업로드 후 여유 공간이 `WITHALBUM_MIN_FREE_BYTES`(기본 1GiB) 미만이면 기록 전에 `507 insufficient_storage`. 길이를 모르는 본문은 예산까지만 받는다. 디스크가 가득 찬 경우(ENOSPC/EDQUOT/SQLITE_FULL)도 507.
- 단일 인스턴스 잠금 `.system/withalbum.lock`(flock): 같은 저장소에서 두 번째 서버나 서버 실행 중 `migrate-legacy -apply`를 `LOCKED`로 거부.
- 같은 SHA-256의 pool 파일이 이미 있으면 해시를 재검증해 재사용하고, 내용이 다르면(손상) `orphans/`로 옮긴 뒤 교체한다. DB에 있으나 pool 파일이 사라진 원본은 같은 파일을 다시 업로드하면 복원된다.
- 테스트 전용 장애 주입 훅 `httpapi.FaultInjector`(프로덕션에서는 nil), 단계별 크래시 이미지 테스트와 실제 SIGKILL 테스트(`server/crash_test.go`).

### Changed
- `/api/v1/health` 응답에 `integrity: ok|warning` 키 추가.
- 업로드 오류 응답 추가: `507 insufficient_storage`, `503 storage_io_error`(디스크 I/O 오류), `503 busy` + `Retry-After: 1`(DB 쓰기 경합이 busy_timeout 5초 초과; 이전에는 500), `500 temp_write_failed`(임시 파일 쓰기 실패; 이전에는 400 `upload_failed`). 폴더 생성의 DB 오류도 같은 규칙(507/503)을 따른다. `X-Device-ID` 길이 검사가 본문 저장 전으로 이동(응답 동일).
- SQLite 연결 PRAGMA에 `fullfsync=1` 추가(macOS에서 커밋을 디스크 캐시 너머까지 기록; 다른 OS는 영향 없음). 측정(내장 SSD, macOS, 1회): 100KiB 업로드 100건 13.4ms → 24.6ms/건, 200MiB 1건 0.31s → 0.34s.
- 프리뷰 파일도 rename 후 디렉터리를 fsync하고, 디코더 패닉은 오류로 바꿔 원본 커밋에 영향을 주지 않는다.
- **업그레이드 시 주의:** 이전 버전의 `.system/state.json`만 있는 저장소에서는 서버가 기동을 거부한다(state.json과 다른 파일을 변경하지 않음, 종료 코드 1). `migrate-legacy`로 이관한 뒤 기동할 것. DB와 state.json이 함께 있으면 state.json이 이관 기록(`legacy_imports`)에 있을 때만 기동한다.
- HTTP 응답 형식은 동일하다. 응답의 `asset_id`는 호환을 위해 계속 SHA-256이며, 내부적으로 sha → 자산을 조회한다.
- **업그레이드 시 필요한 조치:** 서버가 더 이상 저장소 루트를 만들지 않는다. 기존 데이터 폴더도 `withalbum-server -init-storage`를 한 번 실행해야 기동한다(없으면 원인 안내 후 종료).
- 볼륨 확인 실패 시 업로드/폴더 생성은 `503 storage_unavailable`, pool·state에 아무것도 쓰지 않는다.
- `.system/*` 하위 디렉터리는 `MkdirAll` 대신 단계별 `Mkdir`로 생성해, 루트가 사라졌을 때 경로가 재생성되지 않는다.
- CI/Makefile gofmt 검사 대상을 전체 패키지(`gofmt -l .`)로 확대.
- CI 크로스빌드가 `./server`뿐 아니라 전체 패키지(`go build ./...`)를 대상 OS/아키텍처별로 컴파일한다.
- `go.mod`의 go 지시자 표기 `1.23` → `1.23.0` (`go get`이 정규화, 최소 버전 동일).
- **내부 구조 리팩터(HTTP 동작 변경 없음):** `server/main.go`를 설정·기동만 남기고 `internal/httpapi`(핸들러·대시보드), `internal/meta`(`MetadataStore` 인터페이스·도메인 타입), `internal/meta/jsonstore`(state.json 구현), `internal/blobstore`(임시 저장·pool 승격·프리뷰)로 분리. HTTP 계약 테스트(`internal/httpapi/contract_test.go`)와 저장소 적합성 테스트(`internal/meta/metatest`)를 추가해 이후 SQLite 구현을 같은 테스트로 검증한다.

## [v1.5-handoff] — 2026-10-17 (문서·통합 패키지)

- WithAlbum v1.4의 모든 소스·테스트·설치/릴리스 설정·버전별 기획을 하나의 프로젝트로 통합.
- README를 사용 설명 + 파일별 안내 + 상세 구현 상태 + 각 절의 HTML 개발 주석으로 확장.
- `CLAUDE.md`, 기능 상태표, 요구 결정 기록을 추가.
- **Go 서버 기능 변경 없음.** 기존 localhost PoC의 인증/영속 DB/Drive/모바일은 구현되지 않았다.

## [v1.4 design/policy] (기존)

- 계정/폴더 오너/초대/권한 정책 설계, `internal/authz` 및 테스트 추가. HTTP 연결은 미구현.

## [v1.3 design] (기존)

- 위드앨범 이름 확정, Google Drive 저장 방식 설계 및 저장 어댑터 아키텍처.

## [v1.2 PoC / GitHub starter] (기존)

- 기본 Go 로컬 서버, 업로드/해싱/폴더/프리뷰/동기화 데모, 릴리스 자동화 초안.
