# 위드앨범 v1.5 구현 현황 / 품질 게이트

> v1.5는 **문서·통합 인수인계 버전**입니다. Go 애플리케이션의 기능 버전이 아닙니다. 목록에서 `구현`은 현재 저장소에 코드가 존재한다는 뜻이며, 제품 사용 안전성을 보증하지 않습니다.

| 기능 | 현 상태 | 소스 / 근거 | 완료 판정시 요구 증거 |
|---|---|---|---|
| Go 로컬 HTTP 서버 | 개발 PoC 구현 | `server/main.go`(기동), `internal/httpapi` | 개발용 로컬 실행·테스트 |
| 로컬 관리 UI (M17, 디자인 M23) | **부분 구현(httptest·로컬 바이너리 DOM 스모크 검증, 브라우저 수동 점검 전)** — 로그인(웹 쿠키+CSRF), 받은 초대 수락, 내 앨범·생성·파일 보내기 테스트(브라우저 SHA-256을 `X-Content-SHA256`으로 보내 서버가 일치를 확인한 경우만 "원본 보관됨"), OWNER 멤버·초대·소유권 이전·보관, 비밀번호 변경·기기 해지, 관리자 사용자 관리(생성·권한·비활성·기기 해지·보관 앨범 재배정 후 비활성화)·저장장치(저장 중단 경고 D-03)·백업 안내·TLS 지문. SEED "보관 그린" 토큰, 라이트/다크, 해시 기반 CSP(`'unsafe-inline'` 없음). 사용자(D-07)·앨범 상세·초대(D-05/06) 시안 반영(M23 후속, API에 있는 값만) | `internal/httpapi/web/index.html`, `internal/httpapi/admin.go`, `internal/httpapi/admin_test.go`, `docs/DASHBOARD_CHECKLIST.md` | Mac 브라우저(Safari·Chrome) 관리자/일반 사용자 시나리오 수동 점검, 시안과 나란히 비교 |
| 설치 화면 (M24, 이 Mac에서만) | **부분 구현(httptest·jsdom·로컬 바이너리 스모크 검증, Mac 브라우저 점검 전)** — 미등록 저장소·미인수 관리자일 때 평문 루프백 리스너에서만 설치 모드, 콘솔 1회용 코드(60비트·30분·5회 잠김·상수시간), 설치 세션+CSRF+동일 출처, 저장장치 후보(/Volumes·홈·직접 경로·WITHALBUM_ROOT 고정)·쓰기 테스트·마운트 재확인 후 `WithAlbum` 한 단계만 생성, `openApp`+설정 파일 고정, `ClaimBootstrap`(감사 `setup.complete`), 같은 프로세스에서 정상 모드로 전환(설치 라우트 404). TLS·LAN 리스너엔 설치 라우트 없음 | `server/setup.go`, `internal/httpapi/setup.go`, `internal/httpapi/web/setup.html`, `internal/storage/inspect*.go`, `SECURITY.md` | Mac 브라우저(Safari·Chrome)로 실제 외장 디스크 설치 시나리오, 시안 D-01과 나란히 비교 |
| 전체 파일 업로드 (512MiB 이하) | 개발 PoC 구현 | `internal/httpapi`, `internal/blobstore` | — |
| 대용량 재개 업로드 (M22) | **부분 구현(HTTP 테스트·로컬 바이너리 스모크 검증)** — 세션(계정·기기·폴더 소유, 타인 404, `Idempotency-Key` 같은 범위), 전체 크기 공간 예약(507), 오프셋 조회, 청크 순차 추가(fsync 후 기록, 선택 청크 해시), 끊긴 청크의 도착분 보존, SIGKILL 후 기동 시 부분 파일을 기록 오프셋으로 절단, 완료 시 전체 SHA-256 재계산·불일치 시 0부터, 단일 업로드와 같은 커밋 경로(권한·볼륨 재확인, UPLOADER 202), 24시간 만료·주기 정리 | `internal/httpapi/uploads.go`, `internal/blobstore/uploads.go`, `internal/meta/sqlitestore/uploads.go`, 마이그레이션 0008 | 앱 큐 연동, 실기기·외장 디스크·Wi-Fi 끊김 장시간 시험 |
| SHA-256 content-addressed 원본 | 개발 PoC 구현 | `internal/blobstore` | 동시성/복구/블롭 접근 권한 시험 |
| 폴더별 해시 중복 인식 | 개발 PoC 구현 | `internal/meta/sqlitestore` (`CommitUpload`) | 업로드·복제·재시도·동시성 통합 시험 |
| 기본 JPEG/PNG/GIF 프리뷰 | 개발 PoC 구현 | `internal/blobstore/preview.go` | — |
| HEIC·영상 프리뷰 (M22) | **부분 구현(테스트·로컬 바이너리 스모크 검증, macOS)** — 선택적 외부 도구(sips: HEIC/HEIF/AVIF/WebP/TIFF/BMP, ffmpeg: 영상 대표 프레임), 커밋 후 DB 기반 워커(재시작 안전), `preview_status` ready/pending/failed/unsupported + `asset_updated` 이벤트, 도구 없으면 unsupported. 셸 없음·최소 환경·시간 제한·프로세스 그룹 종료·`.system/work` 격리·원본 0400 및 실행 전후 크기·모드·mtime 확인·출력 JPEG 디코드 후 재인코드, ffmpeg는 file 프로토콜·컨테이너 화이트리스트 | `internal/blobstore/convert.go`, `internal/httpapi/previews.go` | Linux ffmpeg 실기기, RAW·Live Photo·HDR, 자식 프로세스 메모리 상한(Go 단독으로 불가) |
| JSON 상태 파일 (이전 버전) | **서버 미사용** — state.json만 있는 저장소는 기동 거부(파일 불변) | `internal/meta/jsonstore`, `server/main.go` (`openMetadata`) | — |
| state.json → SQLite 이관 | **부분 구현(테스트 검증)** — `migrate-legacy` dry-run/`-apply`, 백업·단일 트랜잭션·전 행 검증·원자적 rename, 단계별 크래시 재실행 복구 | `internal/legacyimport`, `server/main.go`, `docs/MIGRATION_legacy_state.md` | 대용량·실기기(외장 디스크) 이관 검증 |
| 폴더별 event/cursor (sync API v2) | **부분 구현(HTTP 테스트·로컬 바이너리 스모크 검증)** — 문자열 cursor(폴더·DB epoch 포함, 복원 시 epoch 교체), `limit`, tombstone `asset_removed`, 409 `cursor_invalid`/`cursor_expired` + 스냅샷(`GET /assets`), 선택적 ack(`device_sync_cursors`), OWNER 사진 제거, 폴더 범위 `/assets/check` | `internal/httpapi/sync.go`, `internal/meta/sqlitestore/sync.go`, `docs/API_v2.yaml` | 앱 연동, 이벤트 보존(삭제) 정책은 미구현 |
| 멱등 업로드 | **부분 구현(HTTP 테스트 검증)** — `Idempotency-Key` 필수, `upload_receipts`(계정·기기·폴더·키), 응답 유실 재시도 시 같은 결과, 다른 바이트 422, 30일 보존 후 정리 | `internal/httpapi/httpapi.go` (`upload`), 마이그레이션 0007 | 앱 큐 연동 |
| 정식 SQLite schema v1.4 | **부분 구현(서버 연결)** — 폴더·자산·blob·링크·시퀀스·이벤트 사용, 사용자·감사 로그·기기·세션 사용(로그인 API), 멤버·초대 테이블 사용(초대·멤버 API) | `internal/db/` (마이그레이션 0001~0006), `internal/meta/sqlitestore`, `docs/schema_v1.4.sql` | 장애 시험 |
| 파일·DB atomic commit | **부분 구현(테스트 검증, 실기기 미검증)** — staging fsync → pool rename → 디렉터리 fsync → DB tx(COMMIT 직전 볼륨·파일 재확인), 기동 재조정(incoming 정리·고아 orphans/ 격리·누락 보고), 여유 공간 507, 단일 인스턴스 잠금 | `internal/httpapi` (`upload`), `internal/blobstore` (`Promote`, `Reconcile`), `server/main.go` (`openApp`), `server/crash_test.go` | 단계별 장애 주입·SIGKILL 테스트 통과. 남은 증거: 실제 전원 차단·외장 디스크 분리 E2E |
| 외장하드 분리·마운트 식별 | **부분 구현, 실기기 미검증** (볼륨 마커 + st_dev, 쓰기 직전 재확인, 503 `storage_unavailable`) | `internal/storage`, `internal/httpapi` (`storageReady`) | 디스크 unplug 시 fail-closed (Mac mini 실기기 E2E), 디스크 UUID 대조 |
| 사용자 계정/Argon2id | **부분 구현(테스트 검증)** — Argon2id PHC 해시·검증(동시 계산 수 제한), 사용자 저장소, 최초 관리자 `admin init`(콘솔 전용, bootstrap 인수)·`create-user`·`reset-password`, HTTP 관리자 계정 API(`/api/v1/admin/users*`), 마지막 관리자 보호, 감사 로그 | `internal/auth/password`, `internal/users`, `internal/audit`, `server/admin.go`, `internal/httpapi/auth.go` | Pi armv7 해시 지연 측정 |
| 로그인/세션/기기 토큰 | **부분 구현(테스트·로컬 바이너리 스모크 검증)** — 로그인·refresh 회전(재사용 시 세션 폐기)·로그아웃·기기 목록/해지·비밀번호 변경, Bearer + 웹 쿠키/CSRF, 속도 제한, 실패 감사, 재설정·비활성 시 세션 회수. 폴더/자산 API도 세션 필요(M10) | `internal/auth/session`, `internal/auth/ratelimit`, `internal/httpapi/middleware_auth.go`, `docs/API_v2.yaml` | 앱 연동, 실기기. M16: refresh 30초 유예(앱, 1회), 절대 만료 90일(응답 `session_expires_at`, M18), 로그인 시 재해시, 세션 행 정리(6시간마다), 관리자 타인 기기 해지, 비밀번호 변경 실패·권한 거부 요약 감사 |
| 서버의 계정별 앨범 생성 제한 | **부분 구현(HTTP E2E 테스트·로컬 바이너리 스모크 검증)** — 기본 USER 403, 허가 USER·관리자 생성 시 생성자가 OWNER, 트랜잭션 안 재확인 | `internal/httpapi` (`createFolder`), `internal/httpapi/authz_test.go` | 실기기 |
| OWNER·읽기/쓰기 권한 | **부분 구현(HTTP E2E 테스트 검증)** — 소유자·멤버십으로 매 요청 판정, 비멤버·관리자 404, 역할 부족 403, UPLOADER 202 일반 영수증, 업로드 커밋 직전 재확인 | `internal/httpapi/middleware_authz.go`, `internal/meta/sqlitestore`, `internal/authz` | 실기기·TLS(M15) |
| 초대·역할 변경·추방·오너 이전·보관 | **부분 구현(HTTP E2E 테스트·로컬 바이너리 스모크 검증)** — OWNER만, 쓰기마다 트랜잭션 안 재판정 + 같은 트랜잭션 감사 로그. 등록·활성 계정 대상 초대(7일, 1회, 취소 가능, 토큰 없음), 대상 본인만 수락(403/410), 역할 변경·추방 즉시 반영, 이전 시 이전 오너 CONTRIBUTOR·대기 초대 취소·실패 롤백, 앨범 보관(복원 없음) | `internal/meta/sqlitestore/memberships.go`, `internal/httpapi/members.go`, `docs/API_v2.yaml` | 앱 연동, 초대 거절·자진 탈퇴·보관 복원(후속) |
| 보호된 원본·프리뷰 다운로드 | **부분 구현(HTTP E2E 테스트 검증)** — `/folders/{fid}/assets/{aid}/…`만, folder_assets 연결(removed_at NULL) 확인, 다른 폴더 자산 ID 404(IDOR), 무인증 SHA 경로 제거 | `internal/httpapi` (`download`), `internal/httpapi/authz_test.go` | LAN·TLS(M15) 후 실기기 |
| TLS·LAN 바인딩 게이트 (M15) | **부분 구현(단위·HTTP TLS 테스트·LAN 바이너리 스모크 검증)** — non-loopback은 `-allow-lan` + `-tls` + 로그인 가능한 SERVER_ADMIN 모두 필요(사설 IP·와일드카드만), 평문 HTTP는 loopback만, 자체 서명 인증서(0600, TLS 1.3, 같은 키로 갱신) + SPKI SHA-256 핀, `withalbum-server tls`·`GET /api/v1/admin/server/tls` 페어링 페이로드, 보안 헤더·원본 sandbox | `server/listen.go`, `internal/tlsid`, `internal/httpapi/security.go`, `SECURITY.md` | 앱의 핀닝 연결(iOS/Android) 실기기 E2E, 실제 공유기 LAN에서 장시간 운용 |
| 원격 HTTPS/LAN 페어링 | **미구현** | `SECURITY.md` | 실제 Mac/mobile 인증서 검증 |
| iOS/Android 앱 · Share Sheet | **미구현** | `docs/PRODUCT_ARCHITECTURE_v1.4.md` | 두 OS 실기기 흐름 |
| 송신 큐/복구/스마트 프리뷰 | **설계만** | `docs/CLIENT_SYNC_STATES.md` | 실패·전송 영수증·정리 안전 시험 |
| 역방향 동기화 모바일 | **설계만** | `docs/CLIENT_SYNC_STATES.md` | 다중 유저 event cursor 검증 |
| Google Drive 모드 | **설계만** | `docs/GOOGLE_DRIVE_MODE_v1.3.md` | A/B 개인 Google 계정 PoC 검증 |
| macOS DMG GUI · 공증 | **미구현** | `docs/INSTALL_MACOS.md` | Apple Silicon 실기기 설치·서명 검증 |
| Linux/Pi용 소스 크로스빌드 | **CI 설정 존재** | `.github/workflows` | 실제 CI 성공·실기기 실행은 별개 |
| GitHub Releases 자동화 | **설정 존재 + GoReleaser 없는 dry-run 검증(M18)** — prerelease 태그(`vX.Y.Z-접미사`)만·초안 발행, `-trimpath`·`main.version`, 압축 파일에 LICENSE·NOTICE·`third_party/`(Go 모듈·SEED Apache-2.0 고지)·설치 스크립트·문서. `scripts/release-dryrun.sh`가 같은 5개 압축 파일·`checksums.txt`를 저장소 밖에 만들고 검사 | `.goreleaser.yaml`, `.github/workflows/release.yml`, `scripts/release-dryrun.sh`, `scripts/third-party-licenses.sh`, `docs/RELEASING.md` | 실제 GoReleaser 실행(`goreleaser check`·`--snapshot`), Actions에서의 태그 실행, 실제 Release 화면 |
| Mac mini 실기기 E2E | **절차만(M18)** | `docs/MACMINI_E2E_CHECKLIST.md` | 사람이 실기기에서 수행·결과 기록 |
| 백업/복원·무결성 검사 | **부분 구현(테스트·로컬 바이너리 스모크 검증, 실기기 미검증)** — `verify [-hash]`(읽기 전용, 같은 크기 손상 감지), `backup`(서버 실행 중, `VACUUM INTO` + 원본 증분 + SHA-256 매니페스트, 대상 마커·잠금, 같은 디스크 경고), `restore`(서버 중지, 전수 검증 → 스테이징 → 세션 전부 폐기 → 저널 → 원자적 교체, 기존 데이터 보존). 업로드 중 백업 일관성·복원 후 권한 동일성·단계별 크래시 재실행 테스트 | `internal/integrity`, `internal/backup`, `server/maint.go`, `docs/BACKUP_RESTORE.md` | 외장 디스크 실기기 복원 훈련, 대용량 소요 시간 |
| 안전한 업데이트(rollback) | **미구현** | `docs/ROADMAP.md` | 업그레이드/다운그레이드 실험 |

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
