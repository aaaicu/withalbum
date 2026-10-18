# GitHub Releases / 버전 배포

## 목표

README의 다운로드 링크 → GitHub Releases → macOS ZIP / Linux tar.gz / Raspberry Pi tar.gz + `checksums.txt` + 릴리스 노트.

`Releases`는 GitHub 저장소 안의 `release/` 폴더가 아니라 GitHub 자체 릴리스 화면입니다.

## 원칙

- **prerelease만 발행합니다.** 1.0·정식판 선언, "서명된 설치 프로그램" 표기를 하지 않습니다. macOS ZIP의 바이너리는 Apple 서명·공증이 되지 않았습니다(DMG 아님).
- 워크플로는 **초안(draft) prerelease**를 만들고, 저장소 owner가 내용을 검토한 뒤 GitHub 화면에서 직접 "Publish release"를 누릅니다.
- 실기기 검증 결과(통과/실패/미검증)를 릴리스 노트에 그대로 적습니다. CI가 빌드됐다는 사실은 실기기 검증이 아닙니다.
- 태그 push와 Release 발행은 **사용자의 명시적 승인 후** 형상관리 담당이 수행합니다.

## 자동화 파일

| 파일 | 역할 |
|---|---|
| `.github/workflows/ci.yml` | PR·main: gofmt, vet, `go test -race`, 서드파티 라이선스 최신 여부, 5개 플랫폼 크로스빌드 |
| `.github/workflows/release.yml` | **pre-release 태그(`vX.Y.Z-접미사`)만** 받음(`v*.*.*-*` 필터 + 형식 검사 단계). `docs/releases/<태그>.md`가 없으면 실패. 검사 후 GoReleaser가 초안 prerelease 생성 |
| `.goreleaser.yaml` | 진입점 `./server`, 5개 대상, `CGO_ENABLED=0`, `-trimpath`, `-X main.version`, 압축 파일·포함 파일, `checksums.txt`, `release.prerelease: "true"`·`draft: true` |
| `scripts/third-party-licenses.sh` | 바이너리에 링크되는 Go 모듈(5개 대상 합집합)과 Go 표준 라이브러리의 라이선스 파일을 `third_party/go-modules/`에 모음. `--check`는 최신이 아니면 실패(GoReleaser `before` 훅·CI) |
| `scripts/release-dryrun.sh` | GoReleaser 없이 같은 압축 파일을 만들고 검사(아래) |
| `.github/release.yml` | GitHub UI에서 "Generate release notes"를 쓸 때의 분류 |

## 압축 파일 구성

이름: `withalbum-server_<버전>_<os>_<arch>` (`darwin_arm64`, `darwin_amd64`은 `.zip`, `linux_amd64`, `linux_arm64`, `linux_armv7`은 `.tar.gz`). `<버전>`은 태그에서 `v`를 뺀 값입니다.

| 파일 | 내용 |
|---|---|
| `withalbum-server` | 실행 파일. `withalbum-server -version` → `withalbum-server <버전>` (예: 태그 `v0.1.0-alpha.1` → `withalbum-server 0.1.0-alpha.1`, `/api/v1/health`의 `version`도 같음) |
| `LICENSE`, `NOTICE` | AGPL-3.0 전문, 제3자 고지 요약(SEED Design Apache-2.0, Go 모듈) |
| `README.md`, `CHANGELOG.md`, `SECURITY.md` | |
| `docs/INSTALL_MACOS.md`, `docs/INSTALL_LINUX.md`, `docs/BACKUP_RESTORE.md`, `docs/MACMINI_E2E_CHECKLIST.md`, `docs/OPEN_SOURCE_DECISIONS.md` | 설치·운영·점검 문서 |
| `scripts/install-macos.sh`, `scripts/install-linux.sh` | 개발용 자동 시작 설치 스크립트 |
| `third_party/go-modules/` | `MODULES.txt`(모듈·버전 목록)와 모듈별 LICENSE/NOTICE/PATENTS/AUTHORS, `go/LICENSE` |
| `third_party/seed-design/` | SEED Design 출처와 Apache License 2.0 전문 |

소스 코드(AGPL-3.0 대응 소스)는 GitHub가 릴리스마다 붙이는 태그의 소스 압축 파일로 제공됩니다.

## 발행 전 로컬 점검 (사람 또는 에이전트, 발행 없음)

```bash
test -z "$(gofmt -l .)" && go vet ./... && go test -race -count=1 ./...
bash scripts/third-party-licenses.sh --check
bash scripts/release-dryrun.sh v0.1.0-alpha.1      # 산출물은 저장소 밖 임시 디렉터리
# GoReleaser가 있으면 같은 설정으로도 확인(dist/ 생성, 발행 없음):
goreleaser check && goreleaser release --snapshot --clean
```

`release-dryrun.sh`가 확인하는 것: `.goreleaser.yaml`의 대상·`-trimpath`·`main.version` 설정과 일치, 압축 파일 이름과 포함 파일(`.goreleaser.yaml`의 `files:` 목록을 읽음), 각 바이너리가 `./server`에서 `CGO_ENABLED=0`·`-trimpath`로 빌드되었고 버전 문자열이 들어 있음, 빌드 기기의 홈·소스 경로가 바이너리에 없음, 이 기기에서 실행 가능한 바이너리의 `-version` 출력, zip 안 실행 권한, `checksums.txt` 검증. 마지막에 파일 목록·크기·체크섬을 출력합니다. 산출물 디렉터리는 확인 후 지우세요.

## 태그 릴리스 절차 (저장소 owner 승인 후)

1. `release/<버전>` 브랜치에서 `CHANGELOG.md`의 해당 섹션 확정(“미발행” 표기 제거, 날짜), `docs/releases/v<버전>.md`에 릴리스 노트와 **실기기 검증 결과표**(`docs/MACMINI_E2E_CHECKLIST.md` 11장) 기입.
2. 위 "발행 전 로컬 점검" 통과.
3. gitflow대로 `main`에 병합 후 태그:
   ```bash
   git tag -a v0.1.0-alpha.1 -m "WithAlbum Server v0.1.0-alpha.1 (prerelease)"
   git push origin v0.1.0-alpha.1
   ```
4. Actions의 "GitHub Release"가 테스트 → 초안 prerelease(압축 파일 5개 + `checksums.txt` + 릴리스 노트)를 만듭니다.
5. GitHub Releases 화면에서 초안을 검토: "Set as a pre-release" 체크 상태, 파일 5개+체크섬, 노트의 알려진 제약·미검증 항목. 받은 zip 하나를 `shasum -a 256 -c checksums.txt --ignore-missing`로 확인한 뒤 **Publish release**.
6. README 다운로드 표의 "미게시"를 갱신.

정식 태그(`v1.0.0`처럼 접미사 없는 태그)는 워크플로를 시작하지 않습니다. 정식판으로 가려면 아래 조건과 함께 워크플로·설정을 의도적으로 바꿔야 합니다.

## 향후 정식판 가이드

- Mac 바이너리 코드 서명 및 공증, 설치기(UI) 배포
- 배포 파일 SHA-256 외 공급망 증명(provenance)·SBOM·취약점 스캔
- 서버 DB/설정 백업 확인 후 안전한 업데이트 및 자동 롤백
- 역호환 API, DB 마이그레이션 가이드 및 Release Notes의 Breaking changes 섹션
