# Docker로 실행하기 — 공유기 설정 없이 서버를 집 안 기기와 떼어 놓기

> **상태: 시험판.** 이 저장소의 코드·시험은 통과했지만, 실제 Docker(Docker Desktop on Mac, Linux)에서
> 빌드·기동·격리를 사람이 확인하기 전입니다. 아래 "4. 격리 확인"을 꼭 직접 해 보세요.

## 왜 Docker인가

집 밖 접속(Tailscale Funnel)을 켜면 인터넷에서 서버의 로그인 화면에 닿을 수 있습니다. 서버는 인증·권한·속도
제한으로 지키지만, **서버가 뚫렸다고 가정**하면 다음 걱정은 그 컴퓨터에서 같은 집 와이파이의 **홈캠·공유기
관리 화면·다른 컴퓨터로 넘어가는 것**입니다([`HOME_NETWORK_ISOLATION.md`](HOME_NETWORK_ISOLATION.md)).

가장 좋은 방법은 공유기에서 망을 나누는 것이지만, 공유기가 지원하지 않으면 이 구성으로 대신할 수 있습니다:

```
휴대폰 ──(집 와이파이)──▶ 이 컴퓨터:8787 ─┐
Tailscale Funnel ──▶ 이 컴퓨터 127.0.0.1:8788 ─┤
                                            ▼
                           [edge 컨테이너: 바이트만 전달]
                                            │  internal 네트워크 (바깥으로 나가는 길 없음)
                                            ▼
                           [withalbum 컨테이너: 서버]  ──✕──▶ 홈캠·공유기·다른 기기·인터넷
                                            │
                                    /data = 사진 보관 폴더
```

- **withalbum 컨테이너는 Docker의 `internal` 네트워크에만** 붙습니다. Docker는 이 네트워크에 바깥으로 나가는
  길을 만들지 않으므로, 서버 프로그램이 뚫려도 그 안에서 홈캠·공유기·다른 컴퓨터·인터넷으로 연결을 열 수 없습니다.
- 휴대폰 연결은 **edge 컨테이너**(`withalbum-server forward`)가 받아 서버로 넘깁니다. 내용을 읽거나 바꾸지 않고
  바이트만 옮기므로, 앱과 서버 사이의 TLS·지문 고정은 그대로입니다. edge는 정해진 두 주소(서버의 8787·8788)로만
  연결합니다.
- 서버는 일반 사용자(uid 10001)로, 읽기 전용 파일 시스템·모든 권한(capability) 제거·`no-new-privileges`로 돕니다.
  쓸 수 있는 곳은 사진 보관 폴더(`/data`)와 작은 `/tmp`뿐입니다.

### 이 구성이 막는 것과 못 막는 것

| | |
|---|---|
| ✅ 막음 | 뚫린 **위드앨범 서버**에서 홈캠·공유기 관리 화면·다른 컴퓨터·인터넷으로 가는 연결 |
| ✅ 줄임 | 서버가 뚫렸을 때 이 컴퓨터의 다른 파일·프로그램까지 닿는 범위(컨테이너 안에 갇힘) |
| ❌ 못 막음 | 같은 와이파이의 **다른 기기**(휴대폰·노트북·스마트 가전)가 뚫렸을 때 그 기기 → 홈캠 |
| ❌ 못 막음 | 사진 보관 폴더 자체: 서버가 뚫리면 `/data`의 사진·DB는 읽히거나 지워질 수 있음 → **백업은 따로** |
| ❌ 못 막음 | Docker나 그 아래 가상 머신·커널의 취약점(드물지만 있음) — Docker·OS 업데이트 유지 |

그래서 Docker와 함께 아래 "5. 공유기를 못 바꿀 때 같이 할 일"도 해 주세요.

### 이 구성에서 꺼지는 기능

서버가 바깥으로 나갈 수 없으므로, **서버가 먼저 바깥에 연결해야 하는 기능**은 쓸 수 없습니다.

- 중계(`WITHALBUM_RELAY`, 종단 간 암호화 중계) — 대신 Tailscale Funnel을 쓰세요(들어오는 연결이라 됨).
- (나중 기능) Google·Apple 로그인의 키 받기 등 서버가 외부 서버에 묻는 기능.

## 1. 준비

| 컴퓨터 | 필요한 것 |
|---|---|
| Mac mini (Apple Silicon) | [Docker Desktop](https://www.docker.com/products/docker-desktop/) — 설정 → Resources → File sharing에 사진 디스크(`/Volumes/...`)가 들어 있는지 확인 |
| Linux (NAS·라즈베리파이 64비트·미니 PC) | Docker Engine + Compose 플러그인(`docker compose version`) |

사진 보관 폴더를 만듭니다(외장 디스크라면 연결된 상태에서):

```sh
mkdir -p /Volumes/PhotoDisk/WithAlbum                 # Mac 예시
sudo mkdir -p /mnt/photos/WithAlbum && sudo chown -R 10001:10001 /mnt/photos/WithAlbum   # Linux 예시
```

이 컴퓨터의 집 주소(휴대폰이 접속할 주소)를 확인합니다. 공유기에서 이 주소를 **고정(DHCP 예약)**해 두면 좋습니다.

```sh
ipconfig getifaddr en0      # Mac (유선이면 en0, 와이파이면 en1일 수 있음)
hostname -I                 # Linux
```

## 2. 설정

이 저장소(또는 릴리스 소스 압축)를 받은 폴더에서:

```sh
cp docker.env.example .env
```

`.env`를 열어 고칩니다(비밀번호·키는 들어가지 않습니다):

```sh
WITHALBUM_DATA=/Volumes/PhotoDisk/WithAlbum     # 1에서 만든 폴더
WITHALBUM_LAN_URL=https://192.168.0.10:8787     # 이 컴퓨터의 집 주소:포트
```

## 3. 처음 한 번 — 저장소 등록 · 관리자 만들기 · 시작

```sh
docker compose build
docker compose run --rm withalbum -init-storage                  # 폴더를 사진 보관소로 등록 (볼륨 ID가 출력됨)
docker compose run --rm withalbum admin init -login <관리자이름>   # 관리자 비밀번호를 물어봄
docker compose up -d
docker compose logs -f withalbum                                 # "listening" 줄과 TLS 지문 확인, Ctrl+C로 빠져나옴
```

- 컨테이너에서는 Mac의 **설치 화면(브라우저)을 쓸 수 없습니다.** 위 두 명령이 설치 화면 대신입니다.
- 관리 화면: 같은 와이파이의 컴퓨터 브라우저에서 `https://192.168.0.10:8787/` (자체 서명 인증서라 경고가
  나옵니다 — 주소창의 지문이 아래 `tls` 명령의 지문과 같은지 확인한 뒤 진행).
- 휴대폰 연결 QR·지문:

```sh
docker compose exec withalbum withalbum-server tls
```

`WITHALBUM_LAN_URL`로 정한 주소가 QR에 들어갑니다(컨테이너 안 주소 `172.x.x.x`가 아니라).

### 이미 Mac에서 쓰던 사진 보관소를 옮길 때

서버(메뉴 막대 앱 또는 `launchctl`)를 **먼저 멈추고**, 같은 폴더를 `WITHALBUM_DATA`로 지정한 뒤 `-init-storage`
없이 `docker compose up -d` 하면 됩니다. 컨테이너 안에서는 디스크 UUID를 읽을 수 없어서 그 확인만 건너뛰고
(기동 로그에 남음), 보관소 표시 파일과 볼륨 ID는 그대로 확인합니다. **Mac의 서버와 컨테이너를 절대 동시에 켜지
마세요.** 서버는 같은 보관소를 한 번에 하나만 열도록 잠그지만, Docker Desktop은 Mac과 가상 머신 사이에서 이 잠금이
통한다는 보장이 없어, 둘이 동시에 열면 사진 DB가 망가질 수 있습니다.

## 4. 격리 확인 (꼭 하세요)

서버 컨테이너와 **같은 네트워크**에서, 막혀야 하는 주소에 연결을 시도해 봅니다. 아무것도 보내지 않고 연결만 한 번씩
시도합니다.

```sh
# 예: 공유기 관리 화면, 홈캠(RTSP 554·웹 80), 이 컴퓨터 자신의 집 주소(SSH 22), 인터넷
docker compose run --rm --no-deps withalbum reach 192.168.0.1:80 192.168.0.1:443 192.168.0.20:554 192.168.0.20:80 192.168.0.10:22 1.1.1.1:443
```

| 결과 | 뜻 |
|---|---|
| `blocked` | 길이 없음 — **정상** |
| `route` | 상대가 "닫힌 포트"라고 답함 — 길이 있음, **격리 실패** |
| `OPEN` | 연결됨 — **격리 실패** |

마지막 줄이 `all blocked`면 통과입니다. `NOT ISOLATED`가 나오면:

- `compose.yaml`의 `withalbum` 서비스가 `networks: [internal]`만 갖고 있는지, `internal: true`가 있는지 확인합니다.
- **Linux에서 이 컴퓨터 자신의 주소만 열려 있다면**(Docker가 내부망에도 호스트 쪽 주소를 줄 수 있음): 호스트
  방화벽에서 내부망 대역의 들어오는 연결을 막습니다. 대역은 `docker network inspect withalbum_internal`의
  `Subnet`입니다. 예(nftables 없이 iptables):
  `sudo iptables -I INPUT -s 172.30.0.0/16 -j DROP` (재부팅 후에도 유지하려면 배포판의 방화벽 설정에 넣기).
- 홈캠·공유기 주소는 공유기 관리 화면의 "연결된 기기" 목록에서 확인할 수 있습니다.

정상 동작도 함께 확인합니다: 휴대폰 앱으로 연결·사진 올리기·보기, 그리고 집 밖(LTE)에서 Funnel로 접속.

## 5. 공유기를 못 바꿀 때 같이 할 일

Docker는 "서버 → 홈캠"을 막습니다. "다른 기기 → 홈캠"까지 줄이려면, 공유기의 망 분리 없이도 할 수 있는 것들:

1. **홈캠 비밀번호**를 기본값에서 바꾸고, 홈캠 **펌웨어를 업데이트**합니다.
2. 홈캠·공유기의 **UPnP·원격 관리·포트포워딩을 끕니다**(위드앨범은 포트포워딩이 필요 없습니다).
3. 공유기에 **게스트 와이파이**가 있으면 홈캠을 게스트 와이파이로 옮기는 것을 검토합니다(게스트망은 보통 집 안
   기기와 서로 못 봅니다). 단, 홈캠 앱이 같은 망을 요구하면 연동이 끊길 수 있으니 확인 후에.
4. 위드앨범 서버 컴퓨터는 사진 보관 외의 일(Home Assistant, NAS 공유 등)을 같이 하지 않는 것이 좋습니다.
5. Docker Desktop·macOS(또는 Linux)·Tailscale을 자동 업데이트로 둡니다.

## 6. 운영

```sh
docker compose ps                    # 상태
docker compose logs --tail 100 withalbum
docker compose stop                  # 멈추기 (사진 보관소 잠금이 풀림)
docker compose up -d                 # 다시 시작
git pull && docker compose build && docker compose up -d     # 새 버전으로
```

- 백업: 서버가 켜진 채로 `docker compose exec withalbum withalbum-server backup -to /data/...`는 **권하지 않습니다**
  (같은 디스크). 다른 디스크를 컨테이너에 추가로 연결하거나, 서버를 멈추고 호스트에서 `withalbum-server backup`을
  실행하세요([`BACKUP_RESTORE.md`](BACKUP_RESTORE.md)).
- 점검: `docker compose exec withalbum withalbum-server verify`
- 외장 디스크가 빠지면 서버는 쓰기를 멈춥니다(fail closed). 디스크를 다시 연결한 뒤 `docker compose restart withalbum`.

### Tailscale Funnel과 함께 (집 밖 접속)

Tailscale은 컨테이너가 아니라 **이 컴퓨터에** 설치합니다([`FUNNEL.md`](FUNNEL.md)). `.env`에
`WITHALBUM_FUNNEL_URL=https://<기기>.<tailnet>.ts.net`을 넣고 `docker compose up -d` 한 뒤, 이 컴퓨터에서:

```sh
tailscale funnel --bg --tcp=443 --proxy-protocol=2 tcp://127.0.0.1:8788
```

edge 컨테이너가 이 컴퓨터의 `127.0.0.1:8788`에서만 받으므로, Funnel 경로는 이 컴퓨터 밖에서 직접 닿지 않습니다.
Funnel 경로에서는 지금처럼 앱 기능만 열리고 관리 화면·설치 화면은 막힙니다.

## 7. 알아 둘 점

- **로그인 시도 제한이 주소별로 나뉘지 않습니다**(집 안 연결): 서버에는 모든 집 안 연결이 edge 컨테이너 주소에서
  온 것으로 보입니다. 집 안에서 누가 비밀번호를 여러 번 틀리면 집 안 전체 로그인이 잠시 막힐 수 있습니다.
  계정별 제한과 Funnel 경로의 주소별 제한(PROXY 헤더)은 그대로입니다.
- HEIC 미리보기는 Linux용 `heif-dec`(libheif)로, 동영상은 `ffmpeg`로 만듭니다(Mac의 `sips`·`avconvert` 대신).
  Mac에서 바로 실행할 때와 미리보기 화질이 조금 다를 수 있습니다.
- Mac의 Docker Desktop은 Linux 가상 머신 위에서 돌아서, Mac에서 바로 실행할 때보다 사진 폴더 읽기·쓰기가
  느릴 수 있습니다. 메뉴 막대 앱은 컨테이너 서버를 관리하지 않습니다.
- 이미지는 비밀값을 갖지 않습니다. TLS 키는 사진 보관 폴더의 `.system/tls/`에 있습니다(백업 대상).

## 설정 값 (컨테이너용)

| 변수 | 뜻 |
|---|---|
| `WITHALBUM_CONTAINER=1` | 이미지가 설정. Funnel 리스너를 컨테이너 주소(`0.0.0.0:8788`)에 열 수 있게 하고, 디스크 UUID를 읽을 수 없을 때 그 확인만 건너뜀 |
| `WITHALBUM_LAN_URL` | 연결 QR에 넣을 집 주소. `https://<사설 IP>:<포트>`, 쉼표로 여러 개 |
| `WITHALBUM_ROOT=/data`, `WITHALBUM_LISTEN=0.0.0.0:8787`, `WITHALBUM_FUNNEL_LISTEN=0.0.0.0:8788` | 이미지 기본값 |
| `WITHALBUM_HEIFDEC` | `heif-dec` 경로 지정, `off`면 끔 |

명령:

| 명령 | 뜻 |
|---|---|
| `withalbum-server forward -map <받을 주소>=<보낼 주소>` | edge 컨테이너의 바이트 전달기 |
| `withalbum-server reach <주소:포트> ...` | 격리 확인: 연결을 한 번씩 시도하고 `blocked`/`route`/`OPEN` 표시 |
