# macOS 설치 — 개발용 패키지 (Apple Silicon 우선)

> 아직 누구나 더블클릭하는 DMG가 아닙니다. **현재는 실험용**이며 로컬 컴퓨터에서만 접근할 수 있습니다.

## 실행 파일 준비

GitHub Releases에서 Mac CPU에 맞는 ZIP을 선택해 압축을 풉니다. 아직 릴리스가 없다면 소스 폴더에서 `go build -o withalbum-server ./server`를 실행합니다.

- Apple Silicon M1/M2/M3/M4 이상: `darwin_arm64`
- Intel Mac: `darwin_amd64`

## 한 번 실행해 보기

중요하지 않은 사진을 보관할 **기존 테스트 전용 디렉터리**를 준비하세요. 외장 스토리지 이름이 아래와 다를 수 있습니다.

```bash
mkdir -p "$HOME/WithAlbumTest" && WITHALBUM_ROOT="$HOME/WithAlbumTest" ./withalbum-server -init-storage
WITHALBUM_ROOT="$HOME/WithAlbumTest" ./withalbum-server
```

서버는 저장소 루트를 만들지 않습니다. `-init-storage`는 **이미 있는** 디렉터리에 볼륨 마커(`.withalbum-volume.json`)를 한 번 기록하고 종료합니다. 외장 디스크를 쓸 때는 디스크가 마운트된 상태에서 그 디스크 안의 폴더로 초기화하세요. 출력된 `volume_id`를 `WITHALBUM_VOLUME_ID`로 지정하면 다른 볼륨으로는 기동하지 않습니다(선택).

브라우저에서 `http://127.0.0.1:8787`을 엽니다.

## 로그인할 때 자동 시작 (실험용 LaunchAgent)

```bash
# 외장하드가 마운트되어 있고 지정 폴더가 이미 있을 때만 실행. 먼저 한 번 초기화:
WITHALBUM_ROOT="/Volumes/ExternalSSD/WithAlbum" ./withalbum-server -init-storage
bash scripts/install-macos.sh ./withalbum-server "/Volumes/ExternalSSD/WithAlbum"
```

스크립트는 `$HOME/.local/share/withalbum-server/`에 실행 파일을 설치하고 `~/Library/LaunchAgents/`에 사용자 LaunchAgent를 만듭니다. 이 방식은 **사용자가 로그인한 세션**에서만 실행되며, 로그인 전이나 사용자 로그아웃 후 상시 실행은 보장하지 않습니다. 최종 Mac 버전에서는 서명된 앱, Service Management와 시스템 수준의 백그라운드 실행을 검토합니다.

> **부분 구현(실기기 미검증):** 루트가 없거나 마커가 없으면 기동하지 않고, 실행 중 마커가 사라지거나 루트의 장치 번호(st_dev)가 바뀌면 업로드를 `503 storage_unavailable`로 거부합니다. 디스크를 다시 연결한 뒤에는 서버를 재시작해야 합니다(LaunchAgent의 KeepAlive가 재시작하지만, 디스크가 없으면 계속 종료됩니다). 디스크 UUID 대조와 본격적인 원본 복구는 아직 미구현이며, 마커 파일을 복사한 다른 디스크는 구분하지 못할 수 있습니다.

## 향후 정식 Mac 버전

원클릭 DMG/PKG + 첫 실행 마법사 + 볼륨 선택 + 관리자 초기 계정 + 메뉴바 상태 + 자동 시작/중지 + 업데이트 지원. Apple 서명 및 공증을 완료해야 사용자에게 일반적인 Mac 설치 경험을 제공할 수 있습니다.
