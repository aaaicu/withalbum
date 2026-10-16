# 위드앨범(WithAlbum) 제품·아키텍처 정의 v1.4

- 기준: 2026-10-17
- v1.3(저장 백엔드, 공유 시트 중심 UX, Mac-first) **유지** + 신규 P0 계정·권한 모델을 병합.
- 문서 상태: **설계 확정 및 인증 정책 테스트 코드 작성**. 실제 로그인/보안 서버 완성 아님.

## 현재 확정된 제품 방향

위드앨범은 iOS/Android 모바일 앱에서 특정 공유 앨범으로 사진·영상을 보내고, 자신이 선택한 저장소(Self-hosted: Mac mini 우선, Linux, Pi, NAS / 향후 Google Drive)에 원본을 보관하는 앱이다. 휴대폰 앱 내부의 원본 임시 복사본은 저장 성공 확인 후 정리하고 가벼운 미리보기로 대체한다. 기본 Photos/Gallery의 원본은 사용자가 직접 관리한다.

## v1.4 추가 결정: 계정과 폴더 소유권

1. 사용자가 서버 주소·계정명·비밀번호로 로그인하며, 계정은 서버 인스턴스마다 독립.
2. 서버 관리자가 계정을 만들거나 승인한다. 기본 계정은 새 앨범 생성 불가.
3. `SERVER_ADMIN` 또는 `can_create_albums=true`인 사용자만 앨범을 생성한다.
4. 앨범 생성자가 단일 OWNER가 되며 OWNER만 다른 서버 계정을 VIEWER(읽기), UPLOADER(쓰기), CONTRIBUTOR(읽기+쓰기)로 초대한다.
5. 권한은 **API 서버에서 검증**한다. 서버 관리자도 앨범 미가입 상태에서는 원본/미리보기 열람 권한이 없다(서버 물리 저장소 접근은 별도 문제).
6. 앨범 소유권을 이전하지 않고 오너 계정을 비활성화할 수 없다.
7. Google Drive 모드에는 이 ACL을 억지로 매핑하지 않는다. **쓰기 전용은 Drive 초기 모드 미지원**.

세부 프로토콜, 보안 제약, 초대/권한 모델은 [ACCESS_CONTROL_v1.4.md](ACCESS_CONTROL_v1.4.md)를 기준으로 한다.

## 개발 우선순위

**P0 Mac-first**: SQLite + 트랜잭션 저장 → 관리자 등록/계정·세션 → 앨범 생성 제한/단일 OWNER → 초대·권한·접근 차단 → 원본 다운로드 권한 검사 → 백업 및 Mac 실기기 검증.

**P1**: 모바일 Share Sheet, 안전한 업로드 큐, 증분 동기화/원본 요청, 용량 최적화, 관리자/오너 권한 UI.

**P2**: Linux/Pi 동작 확인 및 설치 경험 완성, Google Drive PoC, Google Drive 가능한 역할만 UI에 표시.

## 소스

- 기존 제품 설계: [PRODUCT_ARCHITECTURE_v1.3.md](PRODUCT_ARCHITECTURE_v1.3.md)
- 계정·권한: [ACCESS_CONTROL_v1.4.md](ACCESS_CONTROL_v1.4.md)
- DB: [schema_v1.4.sql](schema_v1.4.sql)
- API: [API_AUTH_v1.4.yaml](API_AUTH_v1.4.yaml)
- Google Drive: [GOOGLE_DRIVE_MODE_v1.3.md](GOOGLE_DRIVE_MODE_v1.3.md)

**실행 코드의 현재 상태:** `internal/authz` 권한 판정 함수를 구현해 단위 테스트했다. `server/`의 PoC HTTP 핸들러에는 인증/ACL이 아직 연결되지 않았다. 현 PoC는 루프백 전용으로만 사용할 것.
