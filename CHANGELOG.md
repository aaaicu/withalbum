# Changelog / 변경 기록

## [Unreleased]

아래 [0.1.0-alpha.1] 초안 이후의 변경을 여기에 적습니다.

### Added (end-to-end encrypted relay for access from outside the home, M35)
- 새 바이너리 `withalbum-relay`(`./relay`, 표준 라이브러리만): 집 서버 터널(키 증명으로 인증)과 앱 연결(랑데부 ID)을 받아 바이트만 이어 줌. TLS 1.3 전용 바깥 TLS(`-cert`/`-key` PEM, 파일이 바뀌면 다시 읽음), 연결·IP·터널·스트림·속도·대역폭·유휴 제한, 내용·IP 없는 JSON 로그(`-log-ips`로만 IP), 사설 주소의 `GET /healthz`(개수만), SIGTERM 시 드레인. 운영: `docs/RELAY_OPERATIONS.md`.
- `internal/relay`: 프로토콜(`WITHALBUM-RELAY/1 CONNECT|SERVE`, CHALLENGE/PROVE), 랑데부 ID = SPKI 핀에서 유도(base32 32자), 바깥 TLS exporter에 묶인 ECDSA P-256 키 증명, 9바이트 헤더 프레임 다중화(스트림당 256 KiB 신용 기반 흐름 제어, FIN/RST, PING), 재연결 지수 백오프+지터 클라이언트, 퍼즈 `FuzzReadFrame`·`FuzzHandshake`.
- 서버 `-relay host:port` / `WITHALBUM_RELAY`(기본 꺼짐, `-tls`·로그인 가능한 관리자 필요, 수신 포트 없음), `WITHALBUM_RELAY_CA_FILE`(사설 CA 중계). 중계로 온 연결은 같은 핀 TLS·같은 ACL, 단 앱 경로만 허용 — `GET /`와 `/api/v1/admin/*`는 403 `not_available_via_relay`. 로그인 속도 제한은 중계가 알려 준 앱 IP 기준(루프백·잘못된 값은 `relay-unknown`).
- 페어링 페이로드에 선택 `relay=<host:port>&rid=<id>`(버전은 `v=1` 유지 — 기존 앱이 v≠1을 거부하므로), `withalbum-server tls -relay`, `GET /api/v1/me`의 `relay`, `GET /api/v1/admin/server/tls`의 `relay`(`connected` 포함).
- 문서: `docs/RELAY_DESIGN.md`(위협 모델, 메타데이터 고지, 프로토콜, 결정, 앱 연동 계약), `SECURITY.md` 중계 절, `docs/API_v2.yaml`.

### Added / Changed (trust hardening, M37)
- 디스크 식별: 볼륨 마커에 `disk_uuid`(macOS `getattrlist` ATTR_VOL_UUID, Linux `/dev/disk/by-uuid` 최선, 그 외 없음)를 `-init-storage`·설치 화면 등록 때 기록. 기동(`storage.Open`)에서 기록된 UUID와 다르면 `ErrDiskMismatch`, 읽을 수 없으면 `ErrDiskUnknown`으로 `SECURITY:` 안내와 함께 거부(복사된 마커·복제 디스크). 이전 버전 마커는 서버가 첫 기동 때 잠금을 잡은 뒤 그 디스크의 UUID를 채택하고 로그를 남김(`verify` 등 읽기 명령은 마커를 바꾸지 않음). 새 플래그 `-adopt-disk`(서버 중지 필요, 잠금): 의도적으로 옮긴 디스크를 라이브러리 디스크로 다시 기록. 이전 버전 바이너리는 새 필드를 무시(형식 1 유지).
- **동작 변경:** 실행 중 저장소 확인(`Volume.Check`)이 한 번 실패하면 그 프로세스에서는 계속 실패(503 `storage_unavailable`, health `unavailable`) — 디스크를 다시 연결해도 재시작 전까지 쓰지 않음. 혼돈 시험에서 같은 장치 번호로 재연결되면 health가 `ok`로 돌아오지만 DB 핸들·인스턴스 잠금은 분리된 마운트의 것이던 문제를 수정. 저장소가 사라져 세션 확인이 실패하면 500 대신 503 `storage_unavailable`.
- `backup -keep N`: 새 스냅샷이 완성된 뒤 가장 오래된 완성 스냅샷부터 지워 N개 유지(먼저 `.partial-pruned-<id>`로 rename, 공유 원본은 유지, 실패해도 새 스냅샷은 유효·JSON `pruned`/`prune_error`, 종료 코드 1).
- `scripts/install-backup-macos.sh`(매일 `backup -keep N` + 선택 주간 `verify -hash` LaunchAgent, `--print`·`--uninstall`), `scripts/restore-drill.sh`(백업 → `restore -check` → 임시 루트 복원 → `verify -hash` → 임시 포트 기동 확인, `--demo`).
- `scripts/chaos-macos.sh` + `tools/chaos`: hdiutil 스파스 디스크 이미지 위 혼돈 시험(SIGKILL 본문 중·장애 지점, 강제 분리, 재연결·장치 번호 변경, 가득 참, SIGTERM, 복사 마커, 디스크 없는 기동) — 100회 결과 `docs/CHAOS_RESULTS.md`. 장애 지점 정지는 `-tags chaos` 시험 빌드에서만(`server/chaos_on.go`; 기본 빌드는 `chaos_off.go`로 nil).
- `scripts/perf-bench.sh` + `tools/perf`: 합성 10만·50만 자산 측정 `docs/PERFORMANCE.md`. 질의 계획 회귀 테스트(`queryplan_test.go`). 빠진 인덱스는 없었음; 50만 원본에서 기동 재조정이 약 45초(파일 시스템 한계)로 기록.
- README Known limitations·INSTALL_MACOS·MACMINI_E2E_CHECKLIST 5장·DASHBOARD_CHECKLIST·BACKUP_RESTORE·SECURITY·API_v2(health `unavailable` 유지, 인증 경로 503) 갱신.

### Added (macOS menu bar app + DMG, M32)
- `macos/`: 메뉴 막대 앱 "WithAlbum Server"(Swift Package, SwiftUI `MenuBarExtra`, macOS 13+, arm64+x86_64, `LSUIElement`). 번들 `Contents/Helpers/withalbum-server`에 유니버설 서버. 서버는 `scripts/install-macos.sh`와 같은 사용자 LaunchAgent(`org.withalbum.server`, `RunAtLoad`·`KeepAlive`·`ThrottleInterval` 30, `~/Library/Logs/WithAlbum` 0700, `WITHALBUM_CONFIG`·`WITHALBUM_ROOT` 그대로)로 실행하고 `launchctl bootstrap|bootout|kickstart`로 관리. 로그인 시 시작을 끄면 plist를 `~/Library/Application Support/WithAlbum/ServerApp/`로 옮김.
- 상태: 기존 `GET /api/v1/health`(설치 모드는 `/setup` 200)와 `launchctl print`만 사용(새 API 없음). 정상 실행 중·무결성 경고·저장 중단·설치 필요·시작하는 중·시작하지 못함(error.log 마지막 오류)·멈춤·관리하지 않는 서버. 프록시를 쓰지 않음.
- 설치 코드: 설치 모드에서 이 Mac의 error.log 마지막 `SETUP: open` 줄을 읽어 표시·복사(어디에도 보내지 않음), 루프백 URL만.
- LAN 접속 허용: 저장소 등록 + 정상 실행 중일 때만, 이 Mac 인터페이스의 사설 주소(RFC 1918, fc00::/7)만 골라 `-tls -allow-lan`. 주소 규칙은 서버 `scopeOf`·스크립트 `--lan` 패턴과 같고, 공유 픽스처 `macos/Tests/Fixtures/listen_scope_cases.json`을 Go(`server/listen_shared_test.go`)와 Swift 테스트가 함께 검사. LAN 동안 상태 확인·관리 화면 창은 `withalbum-server tls -json`의 SPKI 핀이 같은 인증서만 믿음. 페어링 QR(CoreImage).
- 기존 스크립트 설치 감지: 다른 실행 파일을 가리키는 plist는 덮어쓰지 않고 "이 앱으로 관리 전환"(저장소·설정·환경 변수·LAN 주소 유지, 규칙에 맞지 않는 LAN 주소는 루프백으로). DMG·앱 이동(translocation) 경로에서는 등록 거부.
- `scripts/build-macos-app.sh`: 서버 arm64/amd64(`CGO_ENABLED=0 -trimpath -ldflags "-s -w -X main.version"`) + `lipo`, Swift release 빌드(디버그 맵 심볼 strip), Info.plist·LICENSE·NOTICE·`third_party/` 포함, ad-hoc 서명, `--self-check`, 빌드 경로 누출 검사, `hdiutil` UDZO DMG(Applications 링크) + `.sha256`. 서명 없음.
- `scripts/sign-notarize-macos.sh`: `WITHALBUM_SIGN_IDENTITY`(Developer ID Application)·`WITHALBUM_NOTARY_PROFILE`(사람이 `xcrun notarytool store-credentials`로 저장한 키체인 프로필)만 받음. hardened runtime, 예외 없는 entitlements, 앱 공증·staple → DMG 서명·공증·staple → `spctl`. 암호·키 인자 없음. DMG는 GoReleaser가 아닌 관리자 수동 단계(`docs/RELEASING.md` "macOS DMG").
- 테스트: `cd macos && swift test`(29: plist 생성·XML 이스케이프·plutil, 스크립트 plist 감지·전환, 주소 규칙·공유 픽스처, health·launchctl·설치 코드·tls JSON 파싱, P-256 SPKI 핀, 컨트롤러 dry-run 파일 동작, 문구 규칙). `.gitignore`에 `macos/.build/`·`*.dmg`.

### Added (idempotent album creation, video duration)
- `POST /api/v1/folders`에 선택 헤더 `Idempotency-Key`(업로드와 같은 형식 `^[A-Za-z0-9._:-]{1,128}$`, 계정·세션 기기 범위). 같은 키+같은 이름(trim 후) 재시도는 새 폴더를 만들지 않고 처음 만든 폴더를 201 + `Idempotency-Replayed: true`로 돌려줌(역할·`member_count`는 지금 기준, 보관됐으면 404 `folder_not_found`). 같은 키+다른 이름 422 `idempotency_key_reused`, 형식 오류 400 `invalid_idempotency_key`. 재시도도 새 요청처럼 권한 판정(생성 권한이 없어졌으면 403). 같은 키 동시 요청은 폴더 하나(BEGIN IMMEDIATE 트랜잭션 안에서 조회·기록). 키는 30일(업로드 영수증과 같은 정리). 헤더가 없으면 이전과 같음.
- 동영상 재생 시간: 자산 JSON `duration_ms`(정수 ms, 버림, 모르면·영상 아님·아직 안 읽음 null)와 라이브 포토 `components[].duration_ms`. 라이브 포토 자산 자체의 `duration_ms`는 항상 null(원본이 사진; 영상 길이는 구성에). `internal/capturetime.Duration`: QuickTime/MP4 `moov/mvhd`(version 0/1, timescale), mvhd duration이 0인 조각 MP4는 `mvex/mehd`, 무한(all ones)·timescale 0·1주 초과는 모름. 촬영 시각과 같은 읽기 제한·퍼즈 `FuzzDuration`.
- 촬영 시각 워커가 재생 시간도 읽음(커밋 후, 업로드 경로 불변). 새로 알게 되면 `asset_updated`(촬영 시각과 같이 찾으면 한 번) — 영상 원본을 가진 자산 + 그 영상을 `paired_video` 구성으로 가진 자산. 마이그레이션 0012가 기존 원본을 모두 `duration_state = pending`으로 두어 백필(이미 정해진 `captured_at`은 그대로, 사진은 이벤트 없음).
- `GET /me` `server_features`에 `folder_create_idempotency`, `asset_duration` 추가.
- 마이그레이션 0012: `folder_create_receipts`(user·device·key PK, 폴더 삭제 시 함께 삭제), `blobs.duration_state`·`blobs.duration_ms`, 부분 인덱스 `idx_blobs_duration_pending`.

### Added (Live Photos: still + paired video as one asset)
- 라이브 포토 묶음 업로드: `POST /folders/{id}/bundles`(`kind: live_photo`, 구성 `still` 1 + `paired_video` 1, `Idempotency-Key`), `GET|DELETE /folders/{id}/bundles/{bid}`, `POST .../bundles/{bid}/complete`. 각 구성은 기존 재개 업로드 세션(`PATCH /uploads/{uid}`)으로 보냄. 완료 시 두 파일 SHA-256·크기 재계산(틀린 구성만 offset 0, 422 `hash_mismatch` + `role`), 형식(사진 HEIC/HEIF/JPEG, 영상 QuickTime/MP4 아니면 415 `unsupported_component_type`), Apple content identifier 비교(둘 다 있고 다르면 422 `pairing_mismatch`, 하나라도 없으면 `pairing: unverified`로 보관). 두 원본이 pool에 들어간 뒤 한 트랜잭션(권한·볼륨 재확인)에서 커밋 — 그 전에는 자산·이벤트·영수증 없음.
- 중복 규칙(폴더 기준): 새 사진 → 201 `created`; 같은 사진만 있는 자산 → 영상을 붙여 200 `upgraded`(같은 asset_id, `asset_updated`, 감사 `asset.attach_component`); 같은 사진+같은 영상 → 200 `already_present`; 같은 사진+다른 영상 → 409 `component_conflict`(기존 원본 불변). UPLOADER는 언제나 같은 202 영수증(충돌이면 별도 자산으로 보관).
- 자산 JSON에 `media_kind`(항상), 라이브 포토만 `pairing`·`components`(`[{"role":"paired_video","sha256","bytes","mime_type","filename"}]`). `GET /folders/{id}/assets/{aid}/components/paired_video`(원본과 같은 헤더·Range). `POST .../assets/check`의 present 항목에 `paired_sha256`(라이브 포토만). `GET /me`에 `server_features: ["live_photo_bundles"]`. 업로드 세션 응답에 `bundle_id`·`role`(구성 세션만, 그 세션의 complete/DELETE는 409 `upload_in_bundle`).
- 마이그레이션 0011: `upload_bundles`, `upload_sessions.bundle_id`·`role`(+ 묶음·구성 만료를 함께 늘리는 트리거), `asset_components`, `assets.pairing_state`, `upload_receipts` 재생성(`result`에 `upgraded`, `paired_sha256`).
- `internal/livephoto`(+ 테스트용 `livephototest`): HEIC/JPEG EXIF MakerNote Apple 태그 0x0011, MOV `com.apple.quicktime.content.identifier`. 컨테이너 읽기는 `internal/capturetime`의 경계 있는 리더 재사용(`ExifTIFF`, `QuickTimeString` 추가, 기존 추출 동작 불변). 퍼즈 `FuzzStillIdentifier`·`FuzzVideoIdentifier`.
- 만료·복구: 버려진 묶음은 구성 세션·부분 파일과 함께 정리, 기동 시 구성 하나의 바이트가 사라지면 그 구성만 offset 0. 복원은 열린 묶음도 폐기. `verify`는 영상 원본 손상 시 자산과 `role: paired_video`를 보고하고, 영상 행이 없는 라이브 포토를 경고(`live_photos_without_video`).
- 대시보드: 앨범 사진 타일에 작은 "라이브" 표시와 접근성 이름("라이브 포토", 짝 확인 안 됨).

### Added (asset metadata: capture time, uploader name, member count)
- 자산 응답(`GET /folders/{id}/assets` 스냅샷, `GET /folders/{id}/sync`의 `assets`)에 `captured_at`(촬영 시각, nullable)과 `uploaded_by_name`(올린 사람 표시 이름, nullable) 추가. 폴더 응답(`GET /folders`, `GET /folders/{id}`, 생성·초대 수락 응답)에 `member_count`(오너 + 로그인 가능한 활성 멤버, 대기 초대 제외) 추가 — `can_read`일 때만 있음(UPLOADER에게는 키 자체가 없음). 새 라우트 없음, 비멤버·관리자 404와 UPLOADER 403/202 영수증 불변.
- `captured_at`: 서버가 원본에서 읽은 값만. 형식 세 가지 — `2026-10-19T14:03:22+09:00`(현지 시각+오프셋), `2026-10-19T05:03:22Z`(오프셋 없는 순간, 동영상 mvhd), `2026-10-19T14:03:22`(시간대 없는 현지 시각, EXIF OffsetTimeOriginal 없음). 모르면 null, 업로드 시각으로 대체하지 않음.
- `internal/capturetime`: 순수 Go 추출기 — JPEG/TIFF EXIF(DateTimeOriginal + OffsetTimeOriginal), HEIF/HEIC/AVIF(meta/iinf/iloc의 Exif 항목, 파일 오프셋·idat), QuickTime/MP4(`com.apple.quicktime.creationdate`, 없으면 mvhd). io.ReaderAt로 최대 1MiB·4096회 읽기, 상자 2048개·IFD 항목 1024개·세그먼트 128개 제한, 64비트 mdat는 건너뜀(읽지 않음). 손으로 만든 테스트 픽스처(실제 사진 없음)와 퍼즈 테스트 `FuzzExtract`.
- 촬영 시각 워커(`internal/httpapi/captures.go`): 커밋 후 백그라운드에서 읽음(업로드 경로·응답 시간은 새 바이트와 기존 바이트가 같게 유지). 찾으면 그 원본의 모든 폴더 링크에 `asset_updated` 이벤트, 못 찾으면 이벤트 없음. 마이그레이션 0010이 기존 원본을 모두 `pending`으로 두어 같은 워커가 백필(DB 상태라 재시작 안전, 저장장치가 없으면 대기, 읽을 수 없는 원본은 "모름"으로 정리).
- 마이그레이션 0010: `blobs.capture_state`(pending/done), `blobs.captured_at`, `blobs.captured_utc`(정렬용 UTC 순간), 부분 인덱스 `idx_blobs_capture_pending`. 촬영 시각은 바이트의 속성이라 blob에 저장(같은 원본의 모든 자산이 공유).
- 대시보드 앨범 상세: 사진 타일의 제목·접근성 이름에 촬영 시각(없으면 "추가" 시각)과 올린 사람, 오너가 아닌 읽기 권한자의 머리 줄에 "멤버 N명".

### Added (invitation decline)
- `POST /api/v1/invitations/{invitation_id}/decline`: 초대받은 계정이 본인 세션으로 초대를 거절(본문 없음, 204). 수락과 같은 판정 — 없는 ID 404 `invitation_not_found`, 다른 계정(서버 관리자·오너 포함) 403 `forbidden`, 이미 수락·취소·만료·거절·앨범 보관·보낸 사람이 더 이상 오너가 아님 410 `invitation_gone`(아무것도 쓰지 않음). BEGIN IMMEDIATE 1 트랜잭션에서 상태 `declined` + 감사 로그 `invitation.decline`(앨범·역할·보낸 사람, 토큰·비밀 없음). 정책 표 `AccessSession`.
- 마이그레이션 0009: `folder_invitations.status` CHECK에 `declined` 추가(테이블 재생성, 행·열·인덱스·외래 키 유지). 대기 초대 부분 UNIQUE 인덱스는 `pending`만 다루므로 거절 후 오너가 같은 계정을 다시 초대할 수 있음.
- 대시보드 "받은 초대"에 "거절" 버튼(확인 대화상자). 오너의 대기 초대 목록(`GET /folders/{id}/invitations`)과 앨범 상세의 "초대 대기" 행에서는 거절된 초대가 빠짐(따로 "거절함" 표시 없음).

### Changed (dashboard design v2, D-05/06/07)
- 사용자(D-07): 표 열을 이름·역할·상태·기기·마지막 접속·앨범 생성 허용으로. 기기·마지막 접속은 기존 `GET /admin/users/{uid}/devices`(로그인된 기기, 마지막 로그인·연결 갱신 시각)를 계정마다 불러 계산(동시 4개, API 변경 없음). 필터 칩·검색·"…" 열 대신 이름을 눌러 관리 패널. 상태 태그(활성·정지·로그인 불가), 52×32 스위치, 안내 카드 2개. 사용자 추가 대화상자는 이름·아이디·임시 비밀번호만(일반 사용자·앨범 생성 꺼짐으로 생성, 권한은 표에서 변경). 가입 요청·승인은 서버에 없어 표시하지 않음.
- 앨범(D-05/06): 왼쪽 메뉴 "내 앨범" 아래 앨범 목록, 이동 경로 제거, 제목 아래 "사진 N장 · 멤버 N명"(오너가 아니면 역할부터, 영상·기타는 있을 때만), "최근 추가순", 넓은 사진 카드 + "멤버" 카드(작은 역할 태그, 초대는 "초대 대기 · N일 남음", 관리 도구는 "⋯" 아래). 머리의 방패 "원본 N장 보관됨"은 넣지 않음(자산 목록은 서버 재검증 결과가 아님).
- 초대 대화상자: 닫기(X), "누구를 초대할까요"(관리자 오너: 이미 멤버·초대 대기 계정은 고를 수 없게 표시), 처음 역할 "보기", 7일 만료 안내 상자, 보낸 뒤 버튼이 "OO 님에게 초대를 보냈어요 · 닫기"로 바뀜. 역할 설명 갱신(업로드 전용은 자기가 보낸 사진도 볼 수 없음).
- 역할 표시 "소유자" → "오너". 입력·선택 상자는 안쪽 테두리, 포커스 시 안쪽 2px 포커스 색(강제 색상 모드에서는 외곽선).

## [0.1.0-alpha.1] — 미발행 (초안: 태그·GitHub Release 없음)

> 첫 prerelease 후보. 발행 전까지 이 섹션은 바뀔 수 있습니다. 발행 시 날짜를 적고 "미발행"을 지웁니다. 실기기 검증 결과는 `docs/releases/v0.1.0-alpha.1.md`.

### 요약
- Mac mini 우선 셀프호스팅 서버의 첫 실험판: SQLite 메타데이터, SHA-256 원본 보관(원자적 커밋·기동 재조정), 계정·기기 세션(Argon2id, 회전 refresh), 앨범 ACL(OWNER/CONTRIBUTOR/VIEWER/UPLOADER)·초대·소유권 이전, sync API v2(cursor·tombstone·멱등 업로드), 재개 업로드, HEIC/영상 프리뷰(선택 도구), 백업·복원·무결성 검사, TLS·LAN 게이트, 내장 관리 화면과 설치 화면.
- 공식 API 규격: `docs/API_v2.yaml` (2.0.0-draft).
- 실험판입니다: 인터넷 공개 금지, 대체 불가능한 원본의 유일한 보관소로 쓰지 말 것, 서명·공증되지 않은 macOS 바이너리. 자세한 제약은 README "Known limitations".

### Added (release prep, M18)
- 토큰 응답 `session_expires_at`: 세션 절대 만료(로그인 + 90일). 로그인과 모든 refresh(유예 refresh 포함) 응답에서 같은 값이며 `refresh_expires_at`은 이 값을 넘지 않음. 앱은 더 이상 로그인 시각으로 계산할 필요 없음.
- 원본 다운로드 Range 규격 명시(`docs/API_v2.yaml`): 단일 범위 206 + `Content-Range`, 끝 초과 범위는 마지막 바이트로 제한, 접미 범위, 여러 범위는 `multipart/byteranges`, 만족 불가·형식 오류·bytes 외 단위는 416 + `Content-Range: bytes */<크기>`(본문은 짧은 text/plain), `If-Range`(Last-Modified), 304. 접근 검사가 먼저(UPLOADER 403·비멤버 404 불변). 테스트 `TestOriginalRange`로 고정. 동작 변경 없음.
- API 규격 Sync model 문단에 `asset_updated` 이벤트(적용 규칙 포함) 추가.
- 릴리스 패키징: `NOTICE`, `third_party/go-modules/`(링크되는 Go 모듈·표준 라이브러리 라이선스, `scripts/third-party-licenses.sh`로 생성·`--check`), `third_party/seed-design/`(Apache-2.0 전문). 압축 파일에 CHANGELOG·SECURITY·백업/실기기 점검 문서 포함.
- `scripts/release-dryrun.sh`: GoReleaser 없이 같은 5개 압축 파일과 `checksums.txt`를 저장소 밖에 만들고 대상·플래그·버전 문자열·포함 파일·경로 누출을 검사.
- `docs/MACMINI_E2E_CHECKLIST.md`: Mac mini 실기기 점검 절차와 결과표.

### Changed (release prep, M18)
- `.goreleaser.yaml`: `-trimpath`, `mod_timestamp`, 라이선스 확인 훅, `release.prerelease: "true"`·`draft: true`(이전 `auto`는 접미사 없는 태그를 정식 릴리스로 냈을 것).
- `release.yml`: `vX.Y.Z-접미사` 태그만, 형식 검사·`docs/releases/<태그>.md` 필수·gofmt·vet·`-race` 테스트·라이선스 확인 후 초안 prerelease. `ci.yml`에 라이선스 확인.
- `scripts/install-macos.sh`: 저장소 인자 생략 시 설치 화면이 등록한 설정 파일의 저장소를 사용(LaunchAgent에 `WITHALBUM_ROOT` 없음, `WITHALBUM_CONFIG` 전달), 미등록이면 설치 모드 안내(코드는 0700 로그에만), 설정 파일과 다른 루트를 주면 경고, `--lan ADDR:PORT`(`-tls -allow-lan`, 관리자 필요), `ThrottleInterval` 30초.

### Changed (upload contract fixes)
- 커밋된 `Idempotency-Key`로 업로드 세션을 다시 만들 때(세션이 24시간 만료·정리·완료 후 취소되었거나 단일 요청 업로드의 키) 409 `upload_already_committed` 대신 **저장된 커밋 결과**를 단일 업로드 재응답과 같은 형태로 돌려줌: 읽기 권한자 201 `created`/200 `already_present` + `asset_id`, UPLOADER 202 영수증(`asset_id` 없음), `Idempotency-Replayed: true`, 세션은 만들지 않음. 현재 역할 기준(권한을 잃었으면 403/404). 결과는 영수증을 찾은 같은 트랜잭션에서 읽음(`meta.UploadCommittedError`). 영수증 보존 기간(30일)이 지나면 새 요청 — 새 세션, 커밋은 `already_present`.
- `asset_updated` 이벤트의 `uploader_device_id`: 비어 있던 값 대신 그 자산을 그 폴더에 올린 업로드의 기기 라벨(같은 자산의 `asset_added`와 같은 값). `asset_removed`는 계속 "".
- 문서: 앱 임시 사본 정리 조건을 "원격 원본 SHA-256 검증 완료 + `preview_status`가 `pending`이 아님(`failed`/`unsupported`면 앱이 자체 썸네일 보관)"으로 `CLAUDE.md`·`docs/API_v2.yaml`·`docs/CLIENT_SYNC_STATES.md`에서 통일. `CLIENT_SYNC_STATES.md`에 NEEDS_ATTENTION(자동 재시도 금지 사유와 사용자 조치 → READY)·RETRY_WAIT·READY→HASHING 전이 반영.

### Added (M24, setup screen on this Mac)
- 설치 모드: 등록된 저장소가 없거나(기본 루트·`WITHALBUM_ROOT`가 미등록) 관리자가 없을 때, **평문 루프백 리스너에서만** 설치 화면(`/setup`)을 제공. `-tls`·`-allow-lan`·비루프백 주소에서는 설치 모드로 뜨지 않음. 별도 라우트 표(`httpapi.SetupRoutes`)로 정상 라우트 표와 분리.
- 1회용 설치 코드: 콘솔에만 출력, 12자(60비트), 메모리에 SHA-256만, 30분, 첫 정답에 소모, 5회 오입력 시 잠김(재시작 필요), 상수시간 비교. 코드 입력 전에는 어떤 정보도 응답하지 않음. 설치 세션 쿠키(HttpOnly, SameSite=Strict, Path=/setup, 1시간) + CSRF 헤더, JSON 전용, 다른 출처(Origin/Sec-Fetch-Site) 거부, 기존 Host 검사(421) 유지, 해시 기반 CSP.
- 저장장치 선택: macOS `/Volumes`(마운트 안 된 폴더는 선택 불가, 시스템 보조 볼륨 제외), 홈 폴더(시스템 디스크 경고), 직접 입력한 기존 폴더, `WITHALBUM_ROOT`가 있으면 그 폴더만. 파일 시스템·여유/전체 공간·기존 WithAlbum 데이터·쓰기 테스트. 사용자가 명시적으로 요청할 때만, 고른 디스크 바로 아래 `WithAlbum` 폴더 하나를 마운트 재확인 직후 생성.
- 등록 후 같은 프로세스에서 정상 기동 순서(`openApp`: 볼륨·잠금·메타데이터·재조정)를 실행하고 잠금을 유지, 관리자 생성(`ClaimBootstrap`, 감사 `account.bootstrap_claim` + `setup.complete`, `via=setup`) 후 요청 핸들러를 정상 API로 원자적 교체. 재시작 불필요.
- 사용자 설정 파일(`WITHALBUM_CONFIG`, 기본 `os.UserConfigDir()/WithAlbum/server.json`, 0600): 설치 화면이 고른 루트와 볼륨 ID. `WITHALBUM_ROOT`가 없을 때 서버·콘솔 명령이 사용하고 볼륨 ID를 고정. 설정 파일의 루트가 없으면 종료(fail closed).

### Changed (M23 follow-up, D-05/06/07)
- 사용자(D-07): 전체·앨범 생성 허용·정지 필터 칩, 이름·아이디 검색, 표(사용자·역할·앨범 생성 허용 스위치·상태·관리), 사용자 추가 대화상자(브라우저에서 만든 초기 비밀번호, 역할, 앨범 생성 허용), 생성 후 "앨범에 초대하기" 안내. 목록 API에 없는 기기 수·참여 앨범·최근 접속·가입 요청은 표시하지 않음.
- 앨범(D-05/06): 목록 → 앨범 상세(`#album/<id>`): 사진·영상 개수(자산 목록에서 계산), 최근 사진 미리보기(세션으로 받은 이미지를 blob: URL로 표시, 대시보드 CSP `img-src blob:`), 멤버와 역할·수락 대기 초대(OWNER), 앨범 설정(소유권 이전·보관), 파일 보내기 테스트. UPLOADER는 사진·수량을 보지 않음.
- 초대 대화상자: SERVER_ADMIN 오너는 관리자 계정 목록에서 선택, 일반 오너는 상대의 사용자 ID 입력(사용자 검색 API 없음). 역할 이름·설명을 실제 권한대로: 참여(보기·원본 받기·올리기, 사진 제거와 멤버 관리는 소유자만), 보기(보기·원본 받기), 업로드 전용(자기 사진만 제출). "기기 · 내 계정"에 내 사용자 ID와 복사 버튼.

### Changed (M23, dashboard design)
- 내장 관리 화면을 SEED Design 토큰 기반 "보관 그린" 디자인으로 재구성: 상단 바(로고·서버 상태 칩·사용자), 왼쪽 관리 메뉴(개요·내 앨범·기기 / 관리자: 저장장치·사용자·백업·복구·서버 설정, 주소 `#해시`로 이동·뒤로 가기), 카드 본문, 라이트/다크(`prefers-color-scheme`), 시스템 서체, 인라인 SVG 아이콘, 터치 44px, 모든 입력에 보이는 라벨. 기존 기능은 모두 유지.
- 저장장치 오류 화면: `health`·`/admin/server/storage`의 `storage: unavailable`이면 상단 "저장 중단됨" 칩, 개요 상태 카드(모든 사용자), 저장장치 화면 경고·점검 결과·다시 시작 단계·재검사(관리자). API 변경 없음.
- 공통 상태 카드(무슨 일·데이터 안전·재시도·다음 행동): 디스크 분리, 서버 오프라인, 세션 만료, 권한 변경, 초대 만료, 빈 앨범, 원본 검증 실패.
- 문구 규칙: "업로드 완료" 제거. 파일 보내기 테스트는 브라우저에서 계산한 SHA-256을 `X-Content-SHA256`으로 보내고, 서버 결과가 일치할 때만 방패 아이콘과 "원본 보관됨"을 표시(업로드 전용 앨범은 "서버가 받았어요"). 테스트 `TestDashboardDesignRules`가 문구·아이콘 접근성·원격 리소스·이모지·다크 토큰을 고정.
- SEED Design(Apache-2.0) 토큰 출처를 `docs/OPEN_SOURCE_DECISIONS.md`에 표기.

### Added (M22, resumable uploads / HEIC and video previews)
- 재개 가능한 업로드 세션: `POST /api/v1/folders/{id}/uploads`(전체 `bytes`·`sha256`·`filename`, `Idempotency-Key` 필수), `GET|PATCH|DELETE .../uploads/{uid}`, `POST .../uploads/{uid}/complete`. 세션은 만든 계정·세션 기기·폴더에만 보이고 그 밖에는 404 `upload_not_found`. 같은 키·같은 내용 재생성은 같은 세션(200), 다른 내용 422, 이미 커밋된 키 409 `upload_already_committed`. 생성 시 전체 크기만큼 여유 공간을 예약(다른 열린 세션의 남은 바이트 포함, 부족하면 507), 단일 요청 업로드도 예약분을 침범하지 않음. 청크는 `Upload-Offset`이 현재 오프셋과 같아야 하고(아니면 409 `offset_mismatch`+현재 오프셋), 선택 `X-Chunk-SHA256` 불일치는 422 `chunk_hash_mismatch`(버림), fsync 후 오프셋 기록. 해시 없는 청크가 끊기면 도착분 보존. 같은 세션 동시 요청 409 `upload_busy`. 완료 시 저장 파일 전체를 다시 해시해 선언과 비교(불일치 422 `hash_mismatch`, 세션 0부터), 단일 업로드와 같은 커밋 경로(트랜잭션 안 권한·볼륨 재확인, 영수증, UPLOADER 202). 완료 재시도는 저장된 결과(`Idempotency-Replayed: true`). 계정당 열린 세션 16개(429 `too_many_uploads`), 세션당 최대 64GiB, 청크 최대 512MiB, 24시간(마지막 변경 기준) 만료 후 6시간 주기 정리에 합류. 기동 시 부분 파일을 기록 오프셋으로 절단·바이트 잃은 세션 삭제·고아 부분 파일 삭제.
- HEIC/영상 프리뷰: 선택적 외부 변환기(macOS `sips`, `ffmpeg`; `WITHALBUM_PREVIEW_TOOLS`/`WITHALBUM_SIPS`/`WITHALBUM_FFMPEG`). 해당 형식은 `preview_status: "pending"`으로 커밋되고 백그라운드 워커가 변환 후 `ready`/`failed`를 기록하며 그 바이트를 가진 모든 폴더에 `asset_updated` 이벤트를 남김. 도구가 없으면 `unsupported`(원본만 보존). 자산·업로드 결과에 `preview_status` 추가.
- 업로드 MIME 판별에 ISO BMFF 브랜드 추가(`image/heic`, `image/heif`, `image/avif`, `video/quicktime`, `video/mp4`, `video/3gpp`).
- 마이그레이션 0008: `upload_sessions`, `assets.preview_state`.

### Fixed (M22)
- 같은 바이트의 동시 업로드에서 pool 승격(`Promote`)이 다른 업로드가 방금 넣은 정상 파일을 손상 파일로 오인해 orphans로 격리하고, 그 사이 pool 경로가 비어 동시 커밋이 500(`storage_failed`/`metadata_write_failed`)으로 실패할 수 있던 경쟁 조건(M17에서 1/16 관찰된 ConcurrentUploads 간헐 500의 원인). 승격을 pool 접두 디렉터리 단위로 직렬화.
- UPLOADER가 응답 시간으로 "이미 있던 바이트"를 추정할 수 있던 차이(바이너리 스모크 중앙값 약 7.7ms → 0.5ms 이내): pool·preview 접두 디렉터리 256개를 시작 시 미리 만들고, 이미 있는 바이트의 승격도 같은 디렉터리에 이름 추가·삭제(하드 링크) 후 동기화. 남은 링크는 기동 재조정이 정리.

### Added (M17, admin / owner web UI)
- 내장 웹 관리 화면(`internal/httpapi/web/index.html`, 의존성 없는 정적 HTML/JS): 개요(서버 상태·받은 초대 수락), 내 앨범(생성·업로드 테스트, OWNER는 멤버 역할 변경·내보내기·초대·초대 취소·소유권 이전·보관), 내 계정(비밀번호 변경·기기 해지), 사용자 관리(SERVER_ADMIN: 생성·역할·앨범 생성 권한·비활성/재활성·기기 해지·보관 앨범 재배정 후 비활성화), 서버·저장소(SERVER_ADMIN: 저장소 상태·여유 공간·시작 시 점검·TLS 지문). 모든 동작은 `docs/API_v2.yaml`의 기존 API만 사용하고, 관리자에게도 타인 앨범 이름·내용은 보이지 않음.
- `GET /api/v1/admin/server/storage`(SERVER_ADMIN): 저장소 ok/unavailable, 여유 공간·업로드 예약, integrity, 기동 재조정 결과 개수(경로·볼륨 ID·해시 없음). 전체 `verify` 결과는 저장하지 않음(콘솔 명령).
- `GET /api/v1/admin/users/{uid}/folders`(SERVER_ADMIN): 계정이 소유한 사용 중 앨범 개수, 보관 앨범 ID·날짜(이름·멤버·내용 없음).
- `POST /api/v1/admin/folders/{fid}/owner`(SERVER_ADMIN, 사용자 승인): **보관된 앨범만** 소유자를 다른 활성 계정으로 재배정. 1 트랜잭션(호출자 재판정, 새 오너의 멤버십 제거, 대기 초대 취소, 감사 `folder.reassign_archived_owner`). 보관 상태 유지라 새 오너·관리자 모두 접근 불가(404). 사용 중 앨범은 없는 앨범과 같은 404. 탈퇴 가족 계정 비활성화용.

### Changed (M17)
- 대시보드 CSP에서 `'unsafe-inline'` 제거: 인라인 `<script>`·`<style>` 각 1개를 임베드된 페이지에서 계산한 SHA-256 해시로만 허용, `img-src 'none'`, `form-action 'none'`, Trusted Types 요구(`require-trusted-types-for 'script'`).

### Added (M16, sync API v2 / idempotent uploads / unified OpenAPI)
- 공식 규격 `docs/API_v2.yaml`(2.0.0-draft) 하나로 통합. `API_AUTH_v1.4.yaml`·`API_v1.2.yaml`은 역사 기록. `internal/httpapi/openapi_test.go`가 라우트 정책 표와 경로·메서드·공개 여부·401/403/404 문서화를 대조.
- 업로드 `Idempotency-Key` 필수(없음 400 `idempotency_key_required`, 형식 오류 400 `invalid_idempotency_key`). 계정·세션 기기·폴더·키 범위의 `upload_receipts`를 커밋과 같은 트랜잭션에 기록(already_present 포함). 같은 키 재시도는 첫 결과 그대로(`Idempotency-Replayed: true`), 같은 키·다른 바이트 422 `idempotency_key_reused`. 영수증 30일 보존 후 정리.
- sync v2: cursor는 문자열 `<epoch>.<seq>`(폴더·DB별, `restore`가 epoch 교체), `limit`(1–500, 기본 100), tombstone `asset_removed`, 409 `cursor_invalid`/`cursor_expired`. 스냅샷 `GET /api/v1/folders/{id}/assets?after=&limit=`, 폴더 범위 `POST .../assets/check`(UPLOADER 403), OWNER 사진 제거 `DELETE .../assets/{aid}`(감사 `asset.remove`, 원본 파일 유지), 선택적 ack `PUT .../sync/cursor`(`device_sync_cursors`).
- 인증: refresh 응답 유실 30초 유예(앱만, 1회, `auth.refresh_grace`), 세션 절대 만료 90일, 로그인 시 Argon2id 파라미터 재해시, 6시간마다 오래된 세션 행·used refresh·영수증 정리, 관리자 `GET|DELETE /api/v1/admin/users/{uid}/devices*`(`device.admin_revoke`), 본인 비밀번호 변경 실패 감사, 권한 거부(403/404) 요약 감사 `access.denied`.
- 마이그레이션 0007: `sync_state`, `folder_sequences.pruned_seq`, `upload_receipts` 재생성(미사용 테이블), `auth_refresh_used.used_at/grace`.

### Changed (M16, breaking for clients)
- `GET .../sync`의 `cursor`·`next_cursor`가 정수에서 문자열로. 정수 cursor는 400, 끝을 넘은 cursor는 이제 409.
- `sync` 응답 `assets`에는 아직 폴더에 있는 자산만 들어감.

### Added (M15, TLS / LAN gate)
- `-tls`: 자체 서명 TLS(`internal/tlsid`). 첫 기동 시 ECDSA P-256 키와 인증서를 `.system/tls/server.pem`(키+인증서 한 파일, 0600, 디렉터리 0700, 임시 파일·fsync·rename)에 생성. TLS 1.3 전용. 인증서는 820일, 만료 60일 전·listen IP 누락 시 **같은 키로** 재발급(핀 불변). 0600이 아니거나 심볼릭 링크·손상 파일은 기동 거부(조용히 새 키를 만들지 않음).
- `-allow-lan`: non-loopback `WITHALBUM_LISTEN`은 `-allow-lan` + `-tls` + 로그인 가능한 활성 SERVER_ADMIN ≥1 모두 충족 시에만, 사설 IP(RFC 1918, fc00::/7) 또는 와일드카드만. 공인 IP·호스트 이름은 거부. 평문 HTTP는 loopback만.
- 지문·페어링: 기동 로그와 `withalbum-server tls [-json]`에 SPKI SHA-256 핀(콜론 hex, curl용 base64), 인증서 SHA-256, 만료일, 페어링 페이로드 `withalbum://pair?v=1&url=…&spki_sha256=…`. `GET /api/v1/admin/server/tls`(SERVER_ADMIN), 대시보드 관리자 섹션. 규격 `docs/API_AUTH_v1.4.yaml` 1.4.4-draft.
- 모든 응답에 `X-Content-Type-Options`, `X-Frame-Options: DENY`, `Referrer-Policy: no-referrer`, `Cross-Origin-Resource-Policy: same-origin`, 소스 없는 CSP. HSTS는 TLS + DNS 호스트(localhost·IP 제외)에만.
- 보안 점검 결과를 `SECURITY.md`에 기록.

### Fixed (security, M15 review)
- 원본 다운로드가 업로드 시 추정한 Content-Type(예: `text/html`) 그대로 서버 origin의 문서로 열릴 수 있던 문제(저장형 XSS): 원본 응답에 `Content-Security-Policy: …; sandbox`와 `Content-Disposition: attachment`.
- 로그인 속도 제한의 IP 키가 IPv6 전체 주소라 /64 안에서 주소를 바꿔 우회 가능 → IPv6는 /64 접두사로.
- 평문 loopback 리스너의 DNS 리바인딩: Host가 localhost·루프백 IP가 아니면 421 `invalid_host`.
- 프리뷰 디코딩을 동시에 1개로 제한(대형 PNG 동시 업로드로 메모리 고갈 방지). `MaxHeaderBytes` 1MiB → 64KiB.

### Added (M14, backup / restore / verify)
- `withalbum-server verify [-hash] [-json]` (`internal/integrity`): DB `integrity_check`·`foreign_key_check`, 모든 원본의 존재·크기, `-hash`면 SHA-256 전수 재검증(같은 크기 내용 손상 감지). 문제마다 원본과 그 자산·폴더를 보고, 아무것도 바꾸지 않음(서버 실행 중 가능). 종료 코드 0 정상 / 1 문제 / 3 검사 불가.
- `withalbum-server backup -init -to DIR` / `backup -to DIR [-rehash]` (`internal/backup`): 백업 대상 마커 `.withalbum-backup-target.json`(저장소 마커와 별개, 쓰기 직전 재확인)·대상 잠금. DB는 `VACUUM INTO` 스냅샷, 원본은 스냅샷에 있는 것만 복사하며 해시 검증(손상 원본이면 실패, 좋은 사본을 덮지 않음), 대상 `pool/`·`previews/` 공유로 증분, `snapshots/<id>/manifest.json`(DB·원본·프리뷰 SHA-256). 미완성 스냅샷은 `.partial-` 이름으로만 존재. 같은 디스크 경고, 0700/0600/0400.
- `withalbum-server restore -from SNAPSHOT [-check] [-replace]`: 전수 검증 → `.system-restore` 스테이징 → 마이그레이션·**모든 세션·기기 폐기**·감사 로그 `storage.restore` → 저널 `.withalbum-restore.json` → 기존 `.system`을 `.system.before-restore-<UTC>`로 보존 rename → 활성화. 기본은 빈 초기화 저장소에만, 서버 실행 중이면 `LOCKED`. 중단 시 재실행으로 완료, 저널이 있는 동안 다른 명령·서버는 기동 거부(`storage.ErrRestorePending`).
- `session.RevokeAllTx`, `session.ReasonRestore`, `storage.InitBackupTarget`/`OpenBackupTarget`/`SameDevice`/`LockFile`.
- 문서 `docs/BACKUP_RESTORE.md`: 백업 형식, 잠금 정책(실행 중 가능/중지 필요), 복원 훈련, 손상 원본 복구, orphans/ 보존 정책(자동 삭제 안 함), launchd 예시.

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
