# Changelog / 변경 기록

## [Unreleased]

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
