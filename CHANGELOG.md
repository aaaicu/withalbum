# Changelog / 변경 기록

## [Unreleased]

### Added
- 저장소 볼륨 가드 `internal/storage`: 루트 직하 볼륨 마커 `.withalbum-volume.json`(format 1, 랜덤 volume_id)을 기동 시 검증하고, 업로드·폴더 생성 직전에 마커와 루트 장치 번호(st_dev)를 재확인한다. **부분 구현, 실기기 미검증.**
- `-init-storage` 플래그: 이미 존재하는 디렉터리에 마커만 원자적으로 기록(기존 `.system/` 데이터 불변).
- 선택 환경변수 `WITHALBUM_VOLUME_ID`: 지정한 볼륨이 아니면 기동 거부.
- `/api/v1/health` 응답에 `storage: ok|unavailable`.
- SQLite 기반 `internal/db` (**아직 HTTP 미연결**): cgo 없는 드라이버 `modernc.org/sqlite` v1.39.0(SQLite 3.50.3), 모든 연결에 `foreign_keys=1`·`journal_mode=WAL`·`synchronous=FULL`·`busy_timeout=5000` 강제, 명시적 트랜잭션은 `BEGIN IMMEDIATE`. `PRAGMA user_version` 기반 내장 마이그레이션(0001 = schema v1.4, 0002 = 레거시 이관 보조 테이블 `legacy_imports`·`legacy_asset_map`·`uploader_device_label` 컬럼), 상위 버전 DB 거부, 적용 전 `integrity_check`.

### Changed
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
