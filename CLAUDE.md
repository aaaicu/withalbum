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

- `server/main.go`(기동) + `internal/httpapi`(핸들러) + `internal/meta/sqlitestore` + `internal/blobstore`: 인증 없는 localhost 전용 Go PoC. 메타데이터는 SQLite `.system/withalbum.db`(`meta.MetadataStore` 인터페이스 뒤). 512MiB 단일 요청 원본 업로드/초보적 프리뷰. DB 내부 asset_id는 랜덤이지만 **API의 자산 식별자는 아직 SHA-256**이고 실제 ACL은 없음. 폴더 소유자는 로그인 불가한 bootstrap `admin` 계정.
- `internal/meta/jsonstore`: 서버 기동에 쓰이지 않음(이관 소스 읽기용). state.json만 있는 저장소는 기동 거부, **state.json → SQLite 이관 미구현**.
- `internal/authz`: 권한 함수와 테스트만 있으며 HTTP handler 미연결.
- `internal/db`: SQLite 열기·PRAGMA·마이그레이션(0001 schema v1.4, 0002 이관 보조, 0003 `assets.preview_ready`). `docs/schema_v1.4.sql`은 마이그레이션 0001과 동일(정본은 `internal/db/migrations/`). 계정·세션·멤버·초대·감사 로그 테이블은 아직 미사용.
- `docs/API_AUTH_v1.4.yaml`: **미구현 목표 설계**.
- Flutter/iOS/Android, OAuth Drive, 외부 통신, 실시간 푸시, GUI DMG, 신뢰 가능한 백업 복원은 없음.
- README v1.5는 **기획·인수인계 정리판**이고 새 기능 릴리스가 아님.

## 보안 불변식 (위반하는 변경 금지)

- 인증/인가/HTTPS 검증 전에 `127.0.0.1` listen 가드 제거 금지, 외부 공개 금지.
- 기본 서버 USER는 폴더 생성 금지(`can_create_albums=false`). 서버 관리자라도 타인 앨범 데이터 API 자동 열람 금지.
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
