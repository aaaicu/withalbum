# WithAlbum Server 개발 로드맵 (Mac-first)

## 원칙

최우선 운영 환경은 **상시 실행되는 Mac mini + 외장 SSD/HDD**. 단일 Go 코어를 Linux AMD64, Linux ARM64 및 Raspberry Pi ARMv7에도 빌드합니다. 초반 macOS는 개발자용 CLI 패키지, 안정적인 릴리스 단계에서 서명·공증한 GUI DMG를 목표로 합니다.

## M0 — GitHub 공개 준비/개발용 배포 [현재 이 패키지에 일부 구현]

- [x] 기존 v1.2 Go PoC 보존
- [x] 서버 로컬 웹 대시보드 초안 (상태, 폴더, 업로드)
- [x] 비-로컬 주소 수신 거부, 버전 출력, 기본 종료 처리
- [x] CI·GoReleaser·Release Notes·GitHub README 골격
- [x] 개발자용 macOS/Linux 설치 스크립트
- [ ] 실제 Mac mini/외장 디스크와 Linux/Pi의 수동 E2E 시험
- [ ] 최초 GitHub 저장소 생성·연결 및 `v0.1.0-alpha.1` 시험 릴리스

## M1 — 로컬 서버의 데이터 안정성 (최우선)

- [ ] SQLite WAL, 스키마 마이그레이션, 트랜잭션/업로드 복구, 안전한 외장 디스크 탐지
- [ ] 스토리지 분리/부족 시 fail-closed, 종료·재시작·정전 복구 시험
- [ ] 대용량 분할 업로드·재개·해시 검증·원본 원자적 commit
- [ ] 일관된 파일 ID, CAS, 폴더 링크, 중복 제거, 안전한 변경 이벤트·커서
- [ ] 폴더 직접 파일 가져오기(Import) watcher, 변경/삭제 정책
- [ ] 전체/증분 백업·복원 테스트·업그레이드/다운그레이드 정책

## M2 — 보안과 로컬 공유

- [ ] 최초 관리자 계정 생성 및 복구 방법
- [ ] 페어링 및 기기별 토큰·폐기, 폴더 단위 ACL
- [ ] LAN 보안 연결 정책 (HTTPS 및 안전한 설정 방식)
- [ ] 웹 UI 관리: 사용자, 폴더 권한, 용량, 실패 알림, 작업 로그 (M17: 사용자·앨범 멤버·저장소 상태·TLS 구현, 실패 알림·작업 로그 화면 없음)
- [ ] 위험한 API 테스트, 서비스 계정 권한 최소화

## M3 — 미디어 처리/클라이언트

- [ ] HEIC, Live Photo, RAW, JPG, PNG, MP4, MOV 등 원본 보존 시험
- [ ] 원본 무손실 보관과 CPU/OS별 선택적 프리뷰·썸네일 워커
- [ ] iOS Share Extension / Android Share Intent, 앱 대기열과 재시도
- [ ] 공유 갤러리·역방향 변경 동기화·원본 다운로드
- [ ] 압축 캐시 정책, 원본 정리 안전 조건

## M4 — 누구나 설치하는 배포 (정식 공개 목표)

- [ ] Mac 전용 GUI 설치·초기 설정·메뉴바/관리자 실행·자동 시작
- [ ] macOS 서명·공증된 DMG/PKG, Apple Silicon/Intel 실기기 시험
- [ ] Linux systemd 패키징, Raspberry Pi RAM/성능 실기기 시험
- [ ] Docker/Compose (Linux NAS), Docker 내부 권한/볼륨 점검
- [ ] SBOM, checksum, 공급망 증명, 안전한 업데이트 알림·롤백
- [ ] CI/릴리스 승격 정책, beta 피드백, 장애·복원 실험

**M0 완료와 M4 정식 공개는 전혀 다른 단계입니다.** 현재 코드는 프로토타입이며, 신뢰 가능한 사진 서버로 분류하지 않습니다.


## v1.3 확장 범위 — Google Drive (구현 전)

- Mac mini 개인 서버를 우선 완성하되 모바일 도메인은 `AlbumStorageAdapter` 방식으로 분리
- Drive `drive.file` OAuth·개인 폴더·다중 사용자 공유·변경 목록·업로드 재개 PoC
- Google Drive 모드에서는 제한 기능을 UI에서 명시하고 백엔드 없는 데이터 처리만 제공
- Google Drive 연동을 서버 바이너리에 들어있는 구현이라고 주장하지 않을 것
- 상세 설계: [GOOGLE_DRIVE_MODE_v1.3.md](GOOGLE_DRIVE_MODE_v1.3.md)

## v1.4 P0 권한·계정·폴더 소유권 (신규 확정)

- [x] 서버 계정과 앨범 권한 2중 정책·읽기 전용/쓰기 전용 역할을 문서화
- [x] `internal/authz` 순수 권한 판정 함수 및 역할별 단위 테스트
- [ ] SQLite v1.4 스키마의 실제 적용·마이그레이션 테스트
- [ ] 최초 관리자 bootstrap, 사용자 등록/비활성, Argon2id 로그인, TLS (서버 측 구현: 계정 M8~M11, TLS·LAN 게이트 M15 — 앱 연동·실기기 미검증)
- [ ] 세션 및 기기 등록·토큰 회전/해지
- [ ] 모든 HTTP 핸들러의 권한 미들웨어·데이터 범위 검증
- [ ] 앨범 owner 단일 보장, 사용자 초대·수락·역할·소유권 이전
- [ ] 쓰기 전용의 전역 중복 존재·다른 사용자 자산 정보 누출 방지
- [ ] 관리자/오너 전용 웹 관리 화면 및 보안/복구 테스트
- [ ] Google Drive 모드는 uploader-only를 사용자에게 제공하지 않도록 UI 기능 게이팅

설계: [ACCESS_CONTROL_v1.4.md](ACCESS_CONTROL_v1.4.md). **현 로컬 PoC는 여전히 인증 없는 테스트 서버**다.
