# 위드앨범 v1.5 구현 현황 / 품질 게이트

> v1.5는 **문서·통합 인수인계 버전**입니다. Go 애플리케이션의 기능 버전이 아닙니다. 목록에서 `구현`은 현재 저장소에 코드가 존재한다는 뜻이며, 제품 사용 안전성을 보증하지 않습니다.

| 기능 | 현 상태 | 소스 / 근거 | 완료 판정시 요구 증거 |
|---|---|---|---|
| Go 로컬 HTTP 서버 | 개발 PoC 구현 | `server/main.go`(기동), `internal/httpapi` | 개발용 로컬 실행·테스트 |
| 로컬 관리 UI | 개발 PoC 구현 | `internal/httpapi/web/index.html` | 테스트 화면 조회, 계정 기반 UI는 별개 |
| 전체 파일 업로드 (512MiB 이하) | 개발 PoC 구현 | `internal/httpapi`, `internal/blobstore` | 영상 대용량 재개/정합성은 추가 구현 |
| SHA-256 content-addressed 원본 | 개발 PoC 구현 | `internal/blobstore` | 동시성/복구/블롭 접근 권한 시험 |
| 폴더별 해시 중복 인식 | 개발 PoC 구현 | `internal/meta/sqlitestore` (`CommitUpload`) | 업로드·복제·재시도·동시성 통합 시험 |
| 기본 JPEG/PNG/GIF 프리뷰 | 개발 PoC 구현 | `internal/blobstore/preview.go` | HEIC/RAW/Live Photo/영상 미지원 검증 |
| JSON 상태 파일 (이전 버전) | **서버 미사용** — state.json만 있는 저장소는 기동 거부(파일 불변) | `internal/meta/jsonstore`, `server/main.go` (`openMetadata`) | — |
| state.json → SQLite 이관 | **부분 구현(테스트 검증)** — `migrate-legacy` dry-run/`-apply`, 백업·단일 트랜잭션·전 행 검증·원자적 rename, 단계별 크래시 재실행 복구 | `internal/legacyimport`, `server/main.go`, `docs/MIGRATION_legacy_state.md` | 대용량·실기기(외장 디스크) 이관 검증 |
| 폴더별 event/cursor | 개발 PoC 구현 | `internal/meta/sqlitestore` (`folder_sequences`, `folder_changes`) | 장기간 offline 클라이언트/영속 cursor 검증 |
| 정식 SQLite schema v1.4 | **부분 구현(서버 연결)** — 폴더·자산·blob·링크·시퀀스·이벤트 사용, 사용자·감사 로그·기기·세션 사용(로그인 API), 멤버/초대 테이블 미사용 | `internal/db/` (마이그레이션 0001~0005), `internal/meta/sqlitestore`, `docs/schema_v1.4.sql` | 장애 시험 |
| 파일·DB atomic commit | **부분 구현(테스트 검증, 실기기 미검증)** — staging fsync → pool rename → 디렉터리 fsync → DB tx(COMMIT 직전 볼륨·파일 재확인), 기동 재조정(incoming 정리·고아 orphans/ 격리·누락 보고), 여유 공간 507, 단일 인스턴스 잠금 | `internal/httpapi` (`upload`), `internal/blobstore` (`Promote`, `Reconcile`), `server/main.go` (`openApp`), `server/crash_test.go` | 단계별 장애 주입·SIGKILL 테스트 통과. 남은 증거: 실제 전원 차단·외장 디스크 분리 E2E |
| 외장하드 분리·마운트 식별 | **부분 구현, 실기기 미검증** (볼륨 마커 + st_dev, 쓰기 직전 재확인, 503 `storage_unavailable`) | `internal/storage`, `internal/httpapi` (`storageReady`) | 디스크 unplug 시 fail-closed (Mac mini 실기기 E2E), 디스크 UUID 대조 |
| 사용자 계정/Argon2id | **부분 구현(테스트 검증)** — Argon2id PHC 해시·검증(동시 계산 수 제한), 사용자 저장소, 최초 관리자 `admin init`(콘솔 전용, bootstrap 인수)·`create-user`·`reset-password`, HTTP 관리자 계정 API(`/api/v1/admin/users*`), 마지막 관리자 보호, 감사 로그 | `internal/auth/password`, `internal/users`, `internal/audit`, `server/admin.go`, `internal/httpapi/auth.go` | Pi armv7 해시 지연 측정 |
| 로그인/세션/기기 토큰 | **부분 구현(테스트·로컬 바이너리 스모크 검증, loopback 전용)** — 로그인·refresh 회전(재사용 시 세션 폐기)·로그아웃·기기 목록/해지·비밀번호 변경, Bearer + 웹 쿠키/CSRF, 속도 제한, 실패 감사, 재설정·비활성 시 세션 회수. **폴더/자산 API는 아직 무인증(M10)** | `internal/auth/session`, `internal/auth/ratelimit`, `internal/httpapi/middleware_auth.go`, `docs/API_AUTH_v1.4.yaml` | 앱 연동, TLS(M15), 실기기 |
| 서버의 계정별 앨범 생성 제한 | **정책 함수만** | `internal/authz` | HTTP E2E 403 및 생성/소유자 test |
| OWNER·읽기/쓰기/초대 | **정책 함수·설계만** | `internal/authz`, `docs/API_AUTH_v1.4.yaml` | E2E 초대/수락/철회/오너이전 |
| 보호된 원본·프리뷰 다운로드 | **미구현** | 현재 공개 PoC 다운로드 | 인증된 album-asset 범위 검사·IDOR |
| 원격 HTTPS/LAN 페어링 | **미구현** | `SECURITY.md` | 실제 Mac/mobile 인증서 검증 |
| iOS/Android 앱 · Share Sheet | **미구현** | `docs/PRODUCT_ARCHITECTURE_v1.4.md` | 두 OS 실기기 흐름 |
| 송신 큐/복구/스마트 프리뷰 | **설계만** | `docs/CLIENT_SYNC_STATES.md` | 실패·전송 영수증·정리 안전 시험 |
| 역방향 동기화 모바일 | **설계만** | `docs/CLIENT_SYNC_STATES.md` | 다중 유저 event cursor 검증 |
| Google Drive 모드 | **설계만** | `docs/GOOGLE_DRIVE_MODE_v1.3.md` | A/B 개인 Google 계정 PoC 검증 |
| macOS DMG GUI · 공증 | **미구현** | `docs/INSTALL_MACOS.md` | Apple Silicon 실기기 설치·서명 검증 |
| Linux/Pi용 소스 크로스빌드 | **CI 설정 존재** | `.github/workflows` | 실제 CI 성공·실기기 실행은 별개 |
| GitHub Releases 자동화 | **설정 존재** | `.goreleaser.yaml` | 실제 tag dry-run·asset·checksums test |
| 백업/복원 및 안전한 업데이트 | **미구현** | `docs/ROADMAP.md` | 복원 훈련/rollback 실험 |

## 필수 권한 인수 기준

- USER 생성권 기본 차단, 오너 1명, 초대 수락 전 조회 거부.
- VIEWER 업로드 거부, UPLOADER 목록/프리뷰/원본/해시 existence API 거부, CONTRIBUTOR 타인 삭제/초대 거부.
- SERVER_ADMIN이 멤버 아닌 앨범 API에서 원본을 읽지 못한다.
- 모든 서버 다운로드에서 folder-asset 연결 확인; role 해제 즉시 새 요청 거부.
- Google Drive는 같은 `UPLOADER` 보장을 하지 못하므로 해당 선택지 제거.

## 데이터 안정성 인수 기준

- 디스크 분리·꽉 참·응답 유실·서버 SIGKILL·중복 동시 업로드 후 일관성.
- 미완료 스테이징 파일의 재시도 또는 안전 정리, 업로드 영수증의 멱등성.
- SQLite/Blobs 분리 커밋 오류에도 안전한 재조정 수단 제공.
- macOS 외장 디스크 마운트 지점 우발적 재생성으로 내부 디스크에 쓰지 않음.
- 2차 백업 복원 절차, 유실 시 오류 표시; 원본 삭제는 검증 후에만.
