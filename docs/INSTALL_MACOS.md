# macOS 설치 — 개발용 패키지 (Apple Silicon 우선)

> 아직 누구나 더블클릭하는 DMG가 아닙니다. **현재는 실험용**이며 로컬 컴퓨터에서만 접근할 수 있습니다.

## 실행 파일 준비

GitHub Releases에서 Mac CPU에 맞는 ZIP을 선택해 압축을 풉니다. 아직 릴리스가 없다면 소스 폴더에서 `go build -o withalbum-server ./server`를 실행합니다.

- Apple Silicon M1/M2/M3/M4 이상: `darwin_arm64`
- Intel Mac: `darwin_amd64`

## 한 번 실행해 보기

중요하지 않은 사진을 보관할 **기존 테스트 전용 디렉터리**를 준비하세요. 외장 스토리지 이름이 아래와 다를 수 있습니다.

```bash
mkdir -p "$HOME/WithAlbumTest"
WITHALBUM_ROOT="$HOME/WithAlbumTest" ./withalbum-server
```

브라우저에서 `http://127.0.0.1:8787`을 엽니다.

## 로그인할 때 자동 시작 (실험용 LaunchAgent)

```bash
# 외장하드가 마운트되어 있고 지정 폴더가 이미 있을 때만 실행
bash scripts/install-macos.sh ./withalbum-server "/Volumes/ExternalSSD/WithAlbum"
```

스크립트는 `$HOME/.local/share/withalbum-server/`에 실행 파일을 설치하고 `~/Library/LaunchAgents/`에 사용자 LaunchAgent를 만듭니다. 이 방식은 **사용자가 로그인한 세션**에서만 실행되며, 로그인 전이나 사용자 로그아웃 후 상시 실행은 보장하지 않습니다. 최종 Mac 버전에서는 서명된 앱, Service Management와 시스템 수준의 백그라운드 실행을 검토합니다.

> 스토리지 분리 또는 마운트 실패 시 안전한 동작, 볼륨 고유 ID 검사, 본격적인 원본 복구는 아직 미구현입니다. 단순한 디렉터리 확인은 물리적인 외장 디스크 식별이 아닙니다.

## 향후 정식 Mac 버전

원클릭 DMG/PKG + 첫 실행 마법사 + 볼륨 선택 + 관리자 초기 계정 + 메뉴바 상태 + 자동 시작/중지 + 업데이트 지원. Apple 서명 및 공증을 완료해야 사용자에게 일반적인 Mac 설치 경험을 제공할 수 있습니다.
