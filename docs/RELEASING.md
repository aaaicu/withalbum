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
| `.github/workflows/ci.yml` | PR·main: gofmt, vet, `go test -race`, 서드파티 라이선스 최신 여부, 5개 플랫폼 크로스빌드, macOS 러너에서 셸 문법·`swift test`(메뉴 막대 앱) |
| `.github/workflows/release.yml` | **pre-release 태그(`vX.Y.Z-접미사`)만** 받음(`v*.*.*-*` 필터 + 형식 검사 단계). `docs/releases/<태그>.md`가 없으면 실패. 검사 후 GoReleaser가 초안 prerelease 생성 |
| `.goreleaser.yaml` | 진입점 `./server`, 5개 대상, `CGO_ENABLED=0`, `-trimpath`, `-X main.version`, 압축 파일·포함 파일, `checksums.txt`, `release.prerelease: "true"`·`draft: true` |
| `scripts/third-party-licenses.sh` | 바이너리에 링크되는 Go 모듈(5개 대상 합집합)과 Go 표준 라이브러리의 라이선스 파일을 `third_party/go-modules/`에 모음. `--check`는 최신이 아니면 실패(GoReleaser `before` 훅·CI) |
| `scripts/release-dryrun.sh` | GoReleaser 없이 같은 압축 파일을 만들고 검사(아래) |
| `.github/release.yml` | GitHub UI에서 "Generate release notes"를 쓸 때의 분류 |
| `scripts/build-macos-app.sh` | macOS에서 메뉴 막대 앱(.app, 서버 유니버설 바이너리 포함)과 서명 없는 DMG 생성(아래 "macOS DMG") |
| `scripts/sign-notarize-macos.sh` | 관리자 키체인의 Developer ID·notarytool 프로필로 서명·공증·staple한 DMG 생성 |

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

## macOS DMG

메뉴 막대 앱 DMG는 **GoReleaser·GitHub Actions가 만들지 않습니다.** 서명 인증서와 공증 자격 증명은 관리자 Mac의 키체인에만 있어야 하므로, 관리자가 태그를 받은 macOS에서 직접 만들어 초안 Release에 올립니다(Actions의 릴리스 러너는 Linux).

### 처음 한 번 (사람)

1. Apple Developer Program 계정으로 **Developer ID Application** 인증서를 만들어 이 Mac 키체인에 설치합니다. 확인: `security find-identity -v -p codesigning`.
2. 공증 자격 증명을 키체인 프로필로 저장합니다(앱 암호 또는 App Store Connect API 키는 이 명령이 대화형으로 묻습니다 — 스크립트·환경 변수·저장소에 넣지 않음):
   ```bash
   xcrun notarytool store-credentials withalbum-notary
   ```
3. 인증서 이름·팀 ID·프로필 이름은 저장소에 적지 않습니다. 실행할 때 환경 변수로만 줍니다.

### 릴리스마다

```bash
git checkout v0.1.0-alpha.1
bash scripts/build-macos-app.sh --version 0.1.0-alpha.1          # dist/macos/WithAlbum Server.app (+ _unsigned.dmg)
WITHALBUM_SIGN_IDENTITY="Developer ID Application: <이름> (<팀>)" \
WITHALBUM_NOTARY_PROFILE=withalbum-notary \
  bash scripts/sign-notarize-macos.sh "dist/macos/WithAlbum Server.app"
```

`sign-notarize-macos.sh`: 서버(`Contents/Helpers/withalbum-server`) → 앱 순서로 hardened runtime·보안 타임스탬프·예외 없는 entitlements(`macos/Support/WithAlbumServer.entitlements`)로 서명 → 앱 zip을 `xcrun notarytool submit --keychain-profile "$PROFILE" --wait`로 공증·staple → 공증된 앱으로 DMG(Applications 링크 포함) → DMG 서명·공증·staple → `spctl` 확인 → `WithAlbumServer_<버전>.dmg`와 `.sha256`. 암호·키를 인자로 받지 않습니다.

올리기: 초안 Release에 `WithAlbumServer_<버전>.dmg`를 첨부하고, 그 SHA-256을 릴리스 노트에 적습니다(`checksums.txt`는 GoReleaser 산출물만 다룸). `_unsigned.dmg`는 올리지 않습니다.

### 다른 Mac에서 Gatekeeper 확인 (사람, 발행 전)

인터넷으로 받은 파일(격리 속성 있음)로 확인해야 의미가 있습니다. 초안 Release나 다른 경로로 **다른 Mac**에 내려받아:

1. `spctl -a -vv -t open --context context:primary-signature WithAlbumServer_<버전>.dmg` → `accepted`, `source=Notarized Developer ID`.
2. DMG를 열어 앱을 응용 프로그램 폴더로 옮기고 더블클릭 → 경고 없이(또는 "인터넷에서 다운로드한 앱" 확인 한 번) 열림, 메뉴 막대 아이콘.
3. `spctl -a -vv "/Applications/WithAlbum Server.app"` → `accepted`. `xcrun stapler validate`도 통과.
4. 서버 등록 → 설치 코드 → 설치 화면 → 관리 화면, 로그아웃/로그인 후 자동 시작. 결과를 릴리스 노트의 실기기 검증 표에 적습니다.

## 향후 정식판 가이드

- Mac 메뉴 막대 앱 DMG의 서명·공증 발행(스크립트 준비됨, 위 "macOS DMG"), CLI ZIP 바이너리 서명
- 배포 파일 SHA-256 외 공급망 증명(provenance)·SBOM·취약점 스캔
- 서버 DB/설정 백업 확인 후 안전한 업데이트 및 자동 롤백
- 역호환 API, DB 마이그레이션 가이드 및 Release Notes의 Breaking changes 섹션
