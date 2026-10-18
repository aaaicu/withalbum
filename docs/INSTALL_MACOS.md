# macOS 설치 — 개발용 패키지 (Apple Silicon 우선)

> 아직 누구나 더블클릭하는 DMG가 아닙니다. **현재는 실험용**이며 로컬 컴퓨터에서만 접근할 수 있습니다.

## 실행 파일 준비

GitHub Releases에서 Mac CPU에 맞는 ZIP을 선택해 압축을 풉니다. 아직 릴리스가 없다면 소스 폴더에서 `go build -o withalbum-server ./server`를 실행합니다.

- Apple Silicon M1/M2/M3/M4 이상: `darwin_arm64`
- Intel Mac: `darwin_amd64`

## 설치 화면으로 시작하기 (이 Mac에서만, M24)

```bash
./withalbum-server
```

등록된 저장소가 없으면 서버는 설치 모드로 뜨고 터미널에 다음과 같이 출력합니다.

```
SETUP: open http://127.0.0.1:8787/setup on this Mac and enter the setup code  XXXX-XXXX-XXXX
```

1. 같은 Mac의 브라우저에서 그 주소를 열고 코드를 입력합니다. 코드는 한 번만, 30분 동안 쓸 수 있고 5번 틀리면 잠깁니다(서버를 멈추고 다시 시작하면 새 코드).
2. 원본을 보관할 저장장치를 고릅니다. 외장 디스크를 고르면 그 디스크 바로 아래 `WithAlbum` 폴더를 만들지 묻습니다(직접 체크해야 만들고, 만들기 직전에 디스크가 연결되어 있는지 다시 확인). 이미 만든 폴더는 경로로 지정할 수 있습니다.
3. 관리자 아이디와 비밀번호(12자 이상)를 정하면 같은 서버가 바로 대시보드로 바뀝니다. 방금 만든 계정으로 로그인하세요.

고른 저장 폴더와 볼륨 ID는 `~/Library/Application Support/WithAlbum/server.json`(0600)에 기록되어 다음 실행과 콘솔 명령(`admin`, `verify`, `backup` 등)이 같은 저장소를 씁니다. 그 폴더(디스크)가 없으면 서버는 설치 모드로 가지 않고 종료합니다. 다른 저장소를 쓰려면 `WITHALBUM_ROOT`를 지정하세요(항상 우선). 설치 화면은 `-tls`·`-allow-lan`으로 시작했을 때나 다른 기기에서는 열리지 않습니다.

## 콘솔 명령으로 시작하기

중요하지 않은 사진을 보관할 **기존 테스트 전용 디렉터리**를 준비하세요. 외장 스토리지 이름이 아래와 다를 수 있습니다.

```bash
mkdir -p "$HOME/WithAlbumTest" && WITHALBUM_ROOT="$HOME/WithAlbumTest" ./withalbum-server -init-storage
WITHALBUM_ROOT="$HOME/WithAlbumTest" ./withalbum-server
```

서버는 저장소 루트를 만들지 않습니다. `-init-storage`는 **이미 있는** 디렉터리에 볼륨 마커(`.withalbum-volume.json`)를 한 번 기록하고 종료합니다. 외장 디스크를 쓸 때는 디스크가 마운트된 상태에서 그 디스크 안의 폴더로 초기화하세요. 출력된 `volume_id`를 `WITHALBUM_VOLUME_ID`로 지정하면 다른 볼륨으로는 기동하지 않습니다(선택).

브라우저에서 `http://127.0.0.1:8787`을 엽니다.

## 같은 집 LAN의 기기에서 접속하기 (TLS 필수)

평문 HTTP는 이 컴퓨터(loopback)에서만 열립니다. 휴대폰 등 LAN 기기를 연결하려면 먼저 콘솔에서 관리자를 만들고(`withalbum-server admin init -login <이름>`), Mac의 사설 IP(예: 시스템 설정 > 네트워크)로 TLS를 켜서 시작합니다.

```bash
WITHALBUM_ROOT="$HOME/WithAlbumTest" WITHALBUM_LISTEN="192.168.0.10:8787" ./withalbum-server -tls -allow-lan
```

- 조건(`-allow-lan`, `-tls`, 로그인 가능한 관리자) 중 하나라도 없거나 주소가 공인 IP·호스트 이름이면 기동을 거부합니다.
- 첫 기동 때 `.system/tls/server.pem`(0600)에 자체 서명 인증서가 만들어지고, 로그에 **공개키 지문(SPKI SHA-256)**과 페어링 페이로드(`withalbum://pair?...`)가 나옵니다. 실행 중에도 `withalbum-server tls`로 다시 볼 수 있습니다. 앱은 이 지문으로만 서버를 신뢰합니다.
- 브라우저는 자체 서명 인증서라 경고를 표시합니다. 관리 화면은 가능하면 이 Mac에서 여세요.
- 공유기에서 이 포트를 포트포워딩하지 마세요(인터넷 공개 미지원). 자세한 내용: [`SECURITY.md`](../SECURITY.md).

## 로그인할 때 자동 시작 (실험용 LaunchAgent)

`scripts/install-macos.sh`는 `$HOME/.local/share/withalbum-server/`에 실행 파일을 설치하고 `~/Library/LaunchAgents/org.withalbum.server.plist`(사용자 LaunchAgent)를 만듭니다. 로그는 `~/Library/Logs/WithAlbum/`(0700: 설치 코드가 적힐 수 있음)에 남습니다. 저장소를 정하는 방법은 둘 중 하나입니다.

```bash
# A) 설치 화면으로 등록한 저장소(설정 파일)를 그대로 사용 — 저장소 인자 없음
bash scripts/install-macos.sh ./withalbum-server
#    아직 등록 전이면 설치 모드로 뜹니다. 코드는 로그에만 있습니다:
grep "SETUP: open" ~/Library/Logs/WithAlbum/error.log | tail -1

# B) 콘솔로 초기화한 저장소를 고정(WITHALBUM_ROOT) — 먼저 한 번 초기화
WITHALBUM_ROOT="/Volumes/ExternalSSD/WithAlbum" ./withalbum-server -init-storage
bash scripts/install-macos.sh ./withalbum-server "/Volumes/ExternalSSD/WithAlbum"

# LAN(TLS)으로 제공: 관리자가 있어야 함(설치 완료 또는 admin init 후)
bash scripts/install-macos.sh --lan 192.168.0.10:8787 ./withalbum-server
```

- A는 LaunchAgent에 `WITHALBUM_ROOT`를 넣지 않으므로, 서버가 설정 파일(`~/Library/Application Support/WithAlbum/server.json`, `WITHALBUM_CONFIG`를 지정해 설치했다면 그 경로)의 루트와 볼륨 ID를 씁니다. 설정 파일의 루트가 없으면(디스크 미연결) 서버는 설치 모드가 아니라 종료합니다.
- B는 `WITHALBUM_ROOT`가 설정 파일보다 우선합니다. 설정 파일이 다른 루트를 가리키면 스크립트가 경고합니다.
- `--lan`은 `-tls -allow-lan`과 `WITHALBUM_LISTEN=<주소>`로 실행합니다. 지문은 `WITHALBUM_LISTEN=<주소> ~/.local/share/withalbum-server/withalbum-server tls`.
- `KeepAlive`로 종료되면 다시 실행하고(`ThrottleInterval` 30초), 디스크가 없으면 30초마다 시도하며 종료합니다. 설치 코드가 잠기거나 만료되면 `launchctl kickstart -k gui/$(id -u)/org.withalbum.server`로 새 코드를 받습니다.
- 제거: `launchctl bootout gui/$(id -u) ~/Library/LaunchAgents/org.withalbum.server.plist` 후 plist와 설치된 실행 파일 삭제.

이 방식은 **사용자가 로그인한 세션**에서만 실행되며, 로그인 전이나 사용자 로그아웃 후 상시 실행은 보장하지 않습니다. 최종 Mac 버전에서는 서명된 앱, Service Management와 시스템 수준의 백그라운드 실행을 검토합니다. 실기기 점검 절차: [`MACMINI_E2E_CHECKLIST.md`](MACMINI_E2E_CHECKLIST.md).

> **부분 구현(실기기 미검증):** 루트가 없거나 마커가 없으면 기동하지 않고, 실행 중 마커가 사라지거나 루트의 장치 번호(st_dev)가 바뀌면 업로드를 `503 storage_unavailable`로 거부합니다. 다시 연결한 디스크의 장치 번호가 바뀌었으면 서버를 재시작해야 합니다. 실행 중인 서버는 503으로 버티며 종료하지 않으므로 KeepAlive가 재시작하지 않습니다: `launchctl kickstart -k gui/$(id -u)/org.withalbum.server`(재연결 후 재시작이 실제로 필요한지는 `MACMINI_E2E_CHECKLIST.md` 5장에서 측정). 디스크 UUID 대조와 본격적인 원본 복구는 아직 미구현이며, 마커 파일을 복사한 다른 디스크는 구분하지 못할 수 있습니다.

## 향후 정식 Mac 버전

원클릭 DMG/PKG + 첫 실행 마법사 + 볼륨 선택 + 관리자 초기 계정 + 메뉴바 상태 + 자동 시작/중지 + 업데이트 지원. Apple 서명 및 공증을 완료해야 사용자에게 일반적인 Mac 설치 경험을 제공할 수 있습니다.
