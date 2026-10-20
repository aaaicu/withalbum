# macOS 설치 (Apple Silicon 우선)

> **현재는 실험용**입니다. 서명·공증된 DMG는 아직 발행되지 않았습니다. 아래 메뉴 막대 앱은 소스에서 직접 빌드할 수 있고, 다른 방법으로는 서버 실행 파일과 개발용 LaunchAgent 스크립트를 씁니다.

## 메뉴 막대 앱 "WithAlbum Server" (M32, macOS 13 이상)

앱 하나에 서버(유니버설: Apple Silicon + Intel)가 들어 있고, 메뉴 막대 아이콘에서 서버를 관리합니다. 서버는 앱이 아니라 사용자 LaunchAgent(`org.withalbum.server`, `scripts/install-macos.sh`와 같은 이름·설정)로 실행되므로, 메뉴 막대 앱을 종료해도 서버는 계속 돕니다.

### 설치

1. `WithAlbumServer_<버전>.dmg`를 열고 **WithAlbum Server**를 **Applications(응용 프로그램)** 폴더로 끌어 놓습니다. 디스크 이미지 안에서 바로 실행하면 서버를 등록하지 않습니다(이미지를 꺼내면 경로가 사라지기 때문).
2. 응용 프로그램 폴더에서 앱을 엽니다. 메뉴 막대에 사진 묶음 아이콘이 생깁니다(Dock에는 없음).
3. 메뉴에서 **서버 등록하고 시작** → 저장소가 아직 없으면 서버가 설치 모드로 뜨고 **설치 코드** 창이 열립니다. **설치 화면 열기**로 `http://127.0.0.1:8787/setup`을 열어 코드를 입력하고 저장장치·관리자를 정하세요(아래 "설치 화면으로 시작하기"와 같은 흐름). 설치 화면이 이미 등록한 저장소(`~/Library/Application Support/WithAlbum/server.json`)가 있으면 그것을 그대로 씁니다.

**서명 없는 빌드(직접 빌드한 DMG)의 Gatekeeper:** 서명·공증 전 앱은 다른 Mac에서 내려받으면 "확인되지 않은 개발자" 또는 "손상됨"으로 열리지 않습니다. 직접 빌드한 Mac에서는 그냥 열립니다. 다른 Mac에서 시험하려면 내용을 확인한 뒤 Finder에서 앱을 Control-클릭 → 열기(macOS 15 이상은 시스템 설정 > 개인정보 보호 및 보안 > "그래도 열기")를 쓰세요. `xattr -dr com.apple.quarantine`로 격리 속성을 지우는 방법은 권하지 않습니다. 배포본은 관리자가 서명·공증합니다([`RELEASING.md`](RELEASING.md#macos-dmg)).

### 메뉴

| 항목 | 동작 |
|---|---|
| 상태 줄 | 정상 실행 중 / 실행 중 · 무결성 경고 / 실행 중 · 저장 중단(디스크 없음) / 설치 필요 / 시작하는 중 / 시작하지 못함(30초마다 다시 시도, error.log 마지막 오류 표시) / 멈춤 / 이 앱이 관리하지 않는 서버. 기존 `GET /api/v1/health`(설치 모드에서는 `/setup`)만 물어봄 — 새 API 없음 |
| 저장 위치·접속·서버 버전 | LaunchAgent의 `WITHALBUM_ROOT` 또는 설정 파일의 저장소, "이 Mac에서만 접속" 또는 LAN 주소 |
| 관리 화면 열기 | 이 Mac에서만 접속이면 기본 브라우저로 `http://127.0.0.1:8787`(설치 모드는 `/setup`). LAN 접속을 켠 동안에는 루프백이 열려 있지 않으므로 앱 안 창에서 `https://<사설 주소>`를 열고, 서버 인증서는 저장소의 공개키 지문과 같을 때만 믿습니다 |
| 설치 코드 보기 | 설치 모드일 때만. 이 Mac의 `~/Library/Logs/WithAlbum/error.log`에서 마지막 코드를 읽어 보여 주고 복사합니다(어디에도 보내지 않음). 만료·잠김이면 "새 코드 받기(서버 다시 시작)" |
| 시작 / 멈춤 / 다시 시작 | `launchctl bootstrap` / `bootout` / `kickstart -k`. 멈춤은 지금만 멈추며, "로그인 시 서버 시작"이 켜져 있으면 다음 로그인 때 다시 시작합니다 |
| LAN에서 접속 허용 | 저장소가 등록되고 서버가 정상 실행 중일 때만(=설치 화면에서 관리자를 만들었음). 이 Mac 네트워크의 **사설 주소**(192.168.x.x, 10.x.x.x, 172.16–31.x.x, fd00::/8) 중 하나와 포트를 골라 `-tls -allow-lan`, `WITHALBUM_LISTEN=<주소>:<포트>`로 다시 시작합니다(스크립트의 `--lan`과 같음). 로그인 가능한 관리자가 없으면 서버가 기동을 거부하고, 앱은 그 이유를 보여 주며 "이 Mac에서만 접속으로 되돌리기"를 제공합니다 |
| 기기 연결 정보(페어링 QR) | LAN일 때. `withalbum-server tls -json` 결과(공개키 지문, 주소별 `withalbum://pair…`)를 QR·문자열로 보여 줍니다. 비밀 값은 없습니다 |
| 로그인 시 서버 시작 | 켜면 plist를 `~/Library/LaunchAgents/`에, 끄면 `~/Library/Application Support/WithAlbum/ServerApp/`에 둡니다(끈 상태에서는 "시작"을 누를 때만 로그아웃 전까지 실행) |
| 로그인 시 이 앱 열기 | 메뉴 막대 앱 자체의 로그인 항목(Service Management). 서명 없는 빌드에서는 실패할 수 있습니다 |
| 로그 폴더 열기 | `~/Library/Logs/WithAlbum`(0700: 설치 코드가 적힐 수 있음) |
| 서버 등록 해제 | 서버를 멈추고 이 앱이 쓴 plist만 지웁니다. 사진 원본·저장소·설정 파일·로그는 남습니다 |
| 메뉴 막대 앱 종료 / 서버를 멈추고 종료 | 기본은 앱만 종료(서버 계속 실행) |

### 스크립트로 설치한 서버가 이미 있을 때

`scripts/install-macos.sh`로 만든 `~/Library/LaunchAgents/org.withalbum.server.plist`가 있으면 앱은 그 파일을 덮어쓰지 않고 "스크립트로 설치한 서버가 있어요"를 보여 줍니다. **이 앱으로 관리 전환**을 누르면 저장 위치(`WITHALBUM_ROOT`)·설정 파일(`WITHALBUM_CONFIG`)·기타 환경 변수·접속 방식(`--lan` 주소)은 그대로 두고 실행 파일만 앱 안의 서버로 바꿔 다시 시작합니다. 예전 실행 파일(`~/.local/share/withalbum-server/`)은 지우지 않습니다. 기존 LAN 주소가 규칙(사설 주소)에 맞지 않으면 "이 Mac에서만 접속"으로 바꿉니다. 앱을 다른 위치로 옮겼을 때도 같은 전환으로 경로를 고칩니다. 읽을 수 없는 plist는 건드리지 않습니다.

### 소스에서 빌드

macOS 13 이상 + Xcode(명령행 도구 포함) + Go가 필요합니다.

```bash
bash scripts/build-macos-app.sh --version 0.1.0-alpha.1   # dist/macos/ 에 .app 과 _unsigned.dmg
(cd macos && swift test)                                   # 순수 로직 단위 테스트
```

서버는 `./server`에서 `CGO_ENABLED=0 -trimpath -ldflags "-s -w -X main.version=<버전>"`으로 arm64·amd64를 빌드해 `lipo`로 합치고 `Contents/Helpers/withalbum-server`에 넣습니다. 앱 실행 파일은 Swift Package(`macos/`, SwiftUI `MenuBarExtra`)로 arm64+x86_64를 빌드합니다. 둘 다 ad-hoc 서명만 합니다. 앱의 `Contents/Resources`에 LICENSE·NOTICE·`third_party/`가 들어갑니다.

진단: `"/Applications/WithAlbum Server.app/Contents/MacOS/WithAlbumServer" --status`는 창 없이 상태를 한 번 확인해 출력합니다(읽기만, 설치 코드 값은 출력하지 않음). `WITHALBUM_APP_DRY_RUN=1`로 앱을 실행하면 plist를 임시 폴더에만 쓰고 `launchctl`을 부르지 않습니다(개발·시험용).

**왜 SMAppService.agent가 아닌가:** 번들에 넣는 고정 plist는 서명 후 바꿀 수 없어 `--lan` 주소·`WITHALBUM_ROOT`·로그 경로(`~` 확장 불가)를 사용자마다 다르게 둘 수 없고, 스크립트와 같은 이름의 에이전트와 충돌을 피하기 어렵습니다. 그래서 스크립트처럼 plist를 써서 `launchctl`로 관리합니다.

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

메뉴 막대 앱(위)에 더해: 서명·공증된 DMG 발행, 자동 업데이트, 로그인 전 상시 실행(시스템 서비스). Apple 서명 및 공증을 완료해야 사용자에게 일반적인 Mac 설치 경험을 제공할 수 있습니다.
