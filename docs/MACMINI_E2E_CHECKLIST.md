# Mac mini 실기기 E2E 체크리스트 (v0.1.0-alpha.1)

자동 테스트와 크로스빌드는 "코드가 의도대로 동작한다"까지만 보여 줍니다. 이 문서는 **Apple Silicon Mac mini + 외장 디스크**에서 사람이 직접 따라 하며 결과를 기록하는 절차입니다. 각 항목은 명령 → 기대 결과 → 결과 칸 순서입니다. 결과 칸에는 `통과` / `실패` / `미검증` 중 하나와 **근거**(로그 한 줄, 명령 출력, 측정값)를 적습니다. 근거 없는 `통과`는 쓰지 않습니다.

- 예상 소요: 준비 30분, 1~4장 1시간, 5~7장 1.5시간(재부팅·전원 차단 포함), 8~10장 1시간 → **약 4시간**
- 실제 사진이 아니라 **테스트 전용 파일**을 쓰세요. 이 서버는 아직 대체 불가능한 원본의 유일한 보관소로 쓰면 안 됩니다.
- 전원 차단(7장)은 외장 디스크와 macOS 파일 시스템에 부담을 줍니다. 다른 중요한 데이터가 없는 디스크로만 하세요.
- 결과표는 마지막 장(11장)에 모아 `docs/releases/v0.1.0-alpha.1.md`의 "실기기 검증" 표에 옮깁니다.

## 0. 환경 기록

| 항목 | 값 |
|---|---|
| 날짜 / 시험자 | |
| Mac mini 모델·칩·메모리 | (예: Mac mini M4, 16GB) |
| macOS 버전 (`sw_vers`) | |
| 외장 디스크 모델·연결 방식 | (USB-C / Thunderbolt / USB-A) |
| 외장 디스크 파일 시스템 (`diskutil info /Volumes/<이름> \| grep -i "file system"`) | (APFS 권장, exFAT이면 기록) |
| 백업용 디스크 또는 폴더 | |
| 릴리스 산출물 (`withalbum-server -version`) | |
| 다른 기기 (LAN 점검용) | (다른 Mac, Linux, 휴대폰의 터미널 앱 등) |

명령은 **bash** 기준입니다(macOS 기본 셸 zsh와 `read` 문법이 다름). 터미널마다 먼저 `bash`를 실행하고, 아래 변수와 도우미(1장)를 그 셸에 붙여 넣으세요. 이 문서의 명령에서 쓰는 이름(본인 환경에 맞게 바꾸세요):

```bash
export DISK="/Volumes/WATest"                  # 테스트 전용 외장 디스크
export ROOT="$DISK/WithAlbum"                  # 저장소 루트
export E2E="$HOME/WithAlbumE2E"                # 시험용 작업 폴더(토큰이 들어가므로 끝나면 삭제)
export BIN="$E2E/withalbum-server"
mkdir -p "$E2E" && chmod 700 "$E2E"
```

## 1. 준비 — 산출물 확인

| # | 명령 | 기대 결과 | 결과 |
|---|---|---|---|
| 1.1 | GitHub Release(prerelease) 또는 `scripts/release-dryrun.sh` 산출물에서 `withalbum-server_<버전>_darwin_arm64.zip`과 `checksums.txt`를 `$E2E`에 받는다. `cd "$E2E" && shasum -a 256 -c checksums.txt --ignore-missing` | `..._darwin_arm64.zip: OK` | |
| 1.2 | `cd "$E2E" && unzip -q withalbum-server_*_darwin_arm64.zip && ls` | `withalbum-server`, `LICENSE`, `NOTICE`, `scripts/install-macos.sh`, `third_party/` 등 | |
| 1.3 | `"$BIN" -version` | `withalbum-server <태그에서 v를 뺀 값>` (예: `0.1.0-alpha.1`). 서명되지 않은 바이너리라 Gatekeeper가 막으면 그 문구와 해결 방법(`xattr -d com.apple.quarantine "$BIN"` 또는 시스템 설정 > 개인정보 보호 및 보안에서 허용)을 기록 | |
| 1.4 | `file "$BIN"` | `Mach-O 64-bit executable arm64` | |
| 1.5 | 기존 설정 확인: `ls -l "$HOME/Library/Application Support/WithAlbum/server.json" ~/Library/LaunchAgents/org.withalbum.server.plist 2>&1` | 처음이면 둘 다 없음. 있으면 다른 곳에 옮겨 두고 시작 | |
| 1.6 | 외장 디스크 연결 후 `ls "$DISK" && diskutil info "$DISK" \| grep -E "Volume UUID\|File System Personality"` | 디스크가 마운트되어 있음. UUID 기록 | |

시험 중 쓰는 도우미(로그인·업로드). 테스트 계정 비밀번호에는 따옴표·역슬래시를 쓰지 마세요.

```bash
export WA="http://127.0.0.1:8787"
wa_login() {  # 사용: wa_login <아이디>   → $E2E/token 에 access token 저장(15분 유효)
  read -r -s -p "password for $1: " PW; echo
  curl -s -X POST "$WA/api/v1/auth/login" -H 'Content-Type: application/json' \
    -d "{\"login_name\":\"$1\",\"password\":\"$PW\",\"device_name\":\"e2e\",\"platform\":\"ios\"}" > "$E2E/login.json"
  unset PW
  plutil -extract access_token raw -o - "$E2E/login.json" > "$E2E/token" && echo "logged in"
}
wa_auth() { echo "Authorization: Bearer $(cat "$E2E/token")"; }
wa_folder() { # 사용: wa_folder <이름>   → $E2E/fid
  curl -s -X POST "$WA/api/v1/folders" -H "$(wa_auth)" -H 'Content-Type: application/json' -d "{\"name\":\"$1\"}" > "$E2E/folder.json"
  plutil -extract id raw -o - "$E2E/folder.json" > "$E2E/fid" && cat "$E2E/fid"; echo
}
wa_upload() { # 사용: wa_upload <파일> [Idempotency-Key]   → HTTP 상태와 응답
  local key="${2:-$(uuidgen)}"
  curl -s -w '\nHTTP %{http_code}\n' -X POST "$WA/api/v1/folders/$(cat "$E2E/fid")/assets" -H "$(wa_auth)" \
    -H "Idempotency-Key: $key" -H "X-Filename: $(basename "$1")" -H "X-Content-SHA256: $(shasum -a 256 "$1" | cut -d' ' -f1)" \
    -H 'Content-Type: application/octet-stream' --data-binary @"$1"
}
wa_health() { curl -s "$WA/api/v1/health"; echo; }
# 테스트 파일
head -c 5000000 /dev/urandom > "$E2E/small.bin"
head -c 400000000 /dev/urandom > "$E2E/large.bin"
```

## 2. 설치 A — 설치 화면 경로 (이 Mac에서만)

| # | 명령 / 동작 | 기대 결과 | 결과 |
|---|---|---|---|
| 2.1 | `cd "$E2E" && "$BIN"` (WITHALBUM_ROOT 없이, 포그라운드) | 로그에 `SETUP MODE, LOOPBACK ONLY` 와 `SETUP: open http://127.0.0.1:8787/setup ... setup code XXXX-XXXX-XXXX` | |
| 2.2 | 같은 Mac의 Safari에서 `http://127.0.0.1:8787/setup` → 코드 입력 | 저장장치 목록에 `$DISK`가 "권장"으로, 홈 폴더는 시스템 디스크 경고와 함께 보임 | |
| 2.3 | `$DISK` 선택 → "WithAlbum 폴더 만들기" 체크 → 이 위치로 정하기 | `$ROOT` 폴더가 생기고 `ls -la "$ROOT"`에 `.withalbum-volume.json`, `.system/` | |
| 2.4 | 관리자 아이디(예: `e2eadmin`)·비밀번호(12자 이상) → 설치 완료 → 대시보드 로그인 | 같은 프로세스가 대시보드로 바뀜, 로그인 성공, `/setup` 재방문 시 404 | |
| 2.5 | `cat "$HOME/Library/Application Support/WithAlbum/server.json"`, `stat -f '%Sp' "$HOME/Library/Application Support/WithAlbum/server.json"` | `storage_root`가 `$ROOT`, `volume_id` 있음, 권한 `-rw-------` | |
| 2.6 | 다른 터미널에서 `wa_login e2eadmin && wa_folder e2e-a && wa_upload "$E2E/small.bin"` | `HTTP 201`, `"result":"created"`, 응답 `sha256`이 `shasum -a 256 small.bin`과 같음 | |
| 2.7 | 서버 Ctrl+C → 다시 `"$BIN"` | 설치 모드 없이 정상 기동, 로그 `root=$ROOT volume_id=...`가 2.5와 같음 | |
| 2.8 | 서버 Ctrl+C. 이후 장은 이 저장소(설정 파일 경로)를 씁니다 | | |

## 3. 설치 B — 콘솔(CLI) 경로

설치 화면 경로와 섞이지 않도록 **별도 폴더**를 씁니다(외장 디스크 안 `WithAlbumCLI`).

| # | 명령 | 기대 결과 | 결과 |
|---|---|---|---|
| 3.1 | `mkdir "$DISK/WithAlbumCLI" && WITHALBUM_ROOT="$DISK/WithAlbumCLI" "$BIN" -init-storage` | `Initialized storage root ...`, `volume_id=...` 출력 | |
| 3.2 | 같은 명령 다시 | `already initialized; nothing changed`로 종료 | |
| 3.3 | `WITHALBUM_ROOT="$DISK/WithAlbumCLI" "$BIN" admin init -login cliadmin` | 비밀번호 두 번 입력(에코 없음) 후 관리자 생성 | |
| 3.4 | `WITHALBUM_ROOT="$DISK/WithAlbumCLI" "$BIN"` | 설치 모드 없이 `LOOPBACK ONLY, PLAIN HTTP` 기동, 브라우저 `http://127.0.0.1:8787` 로그인 가능 | |
| 3.5 | `WITHALBUM_ROOT=/Volumes/NoSuchDisk/WithAlbum "$BIN"; echo $?` | `does not exist` 안내 후 종료(코드 1), `/Volumes/NoSuchDisk`가 생기지 않음(`ls /Volumes`) | |
| 3.6 | 서버 Ctrl+C | | |

## 4. launchd 자동 시작 (`scripts/install-macos.sh`)

LaunchAgent는 **사용자가 로그인한 세션**에서만 실행됩니다(로그인 전·로그아웃 후 상시 실행 아님). 무인 재부팅 복구를 원하면 시스템 설정 > 사용자 및 그룹 > 자동 로그인 여부를 기록하세요.

| # | 명령 | 기대 결과 | 결과 |
|---|---|---|---|
| 4.1 | 설정 파일 경로(설치 A) 등록: `cd "$E2E" && bash scripts/install-macos.sh "$BIN"` | `Using the storage registered by the setup screen: $ROOT`, `Installed user LaunchAgent` | |
| 4.2 | `plutil -p ~/Library/LaunchAgents/org.withalbum.server.plist` | `WITHALBUM_ROOT` **없음**(설정 파일 사용), `WITHALBUM_LISTEN=127.0.0.1:8787`, `KeepAlive`, `ThrottleInterval 30` | |
| 4.3 | `sleep 3; wa_health; launchctl print gui/$(id -u)/org.withalbum.server \| grep -E "state\|pid"` | `"storage":"ok"`, `state = running` | |
| 4.4 | `tail -5 ~/Library/Logs/WithAlbum/error.log` | 정상 기동 로그, 경로가 `$ROOT` | |
| 4.5 | 강제 종료 후 자동 재시작: `kill -9 $(pgrep -f '.local/share/withalbum-server/withalbum-server')`, 40초 뒤 `wa_health` | 다시 응답, 새 pid. 재시작까지 걸린 시간 기록 | |
| 4.6 | 로그아웃 → 다시 로그인 → `wa_health` | 로그인 후 자동 실행, 걸린 시간 기록 | |
| 4.7 | CLI 경로 등록으로 교체: `bash scripts/install-macos.sh "$BIN" "$DISK/WithAlbumCLI"` | 설정 파일과 다른 루트라는 `WARNING`, plist에 `WITHALBUM_ROOT` 있음, 로그의 root가 `WithAlbumCLI` | |
| 4.8 | 미등록 상태의 설치 모드(선택): 설정 파일을 잠시 옮기고(`mv ".../server.json" "$E2E/server.json.bak"`) `bash scripts/install-macos.sh "$BIN"` → `grep "SETUP: open" ~/Library/Logs/WithAlbum/error.log \| tail -1` | 코드가 로그에만 있고 로그 폴더 권한 `drwx------`(`stat -f '%Sp' ~/Library/Logs/WithAlbum`). 확인 후 `launchctl bootout gui/$(id -u) ~/Library/LaunchAgents/org.withalbum.server.plist`, 설정 파일 원위치, 4.1 다시 실행 | |
| 4.9 | 이후 장은 4.1 상태(설정 파일 경로)로 진행 | | |

제거 방법(마지막에): `launchctl bootout gui/$(id -u) ~/Library/LaunchAgents/org.withalbum.server.plist; rm ~/Library/LaunchAgents/org.withalbum.server.plist ~/.local/share/withalbum-server/withalbum-server`

## 5. 외장 디스크 분리·재연결 — fail closed와 복귀

핵심 질문: 디스크를 다시 연결했을 때 **서버 재시작 없이** 복귀하는가? 서버는 기동 때 기록한 루트의 장치 번호(st_dev)와 볼륨 마커를 쓰기 직전마다 다시 확인합니다. macOS가 다시 마운트하며 장치 번호를 바꾸면 재시작 전까지 `503 storage_unavailable`이 계속됩니다. 이 장에서 실제로 어떤지 측정합니다.

| # | 명령 / 동작 | 기대 결과 | 결과 |
|---|---|---|---|
| 5.1 | 기준값: `stat -f 'st_dev=%d' "$ROOT"; wa_health; wa_login e2eadmin; wa_folder e2e-disk; wa_upload "$E2E/small.bin"` | `storage ok`, `HTTP 201`. st_dev 값 기록: ____ | |
| 5.2 | Finder에서 꺼내기(또는 `diskutil eject "$DISK"`) — 서버는 실행 중. 꺼내기가 "사용 중"으로 거부되면 그 문구를 기록하고 `diskutil unmount force "$DISK"` 대신 5.3(물리 분리)으로 진행 | 꺼내기 성공 여부·문구 기록 | |
| 5.3 | (5.2가 거부된 경우 또는 추가 시험) 케이블을 **그냥 뽑기** | macOS의 "디스크를 올바르게 꺼내지 않음" 알림 여부 기록 | |
| 5.4 | `wa_health; head -c 1000 /dev/urandom > "$E2E/x.bin"; wa_upload "$E2E/x.bin"` | `"storage":"unavailable"`, 업로드 `HTTP 503` + `storage_unavailable` | |
| 5.5 | `ls -la /Volumes; ls -la "$ROOT" 2>&1` | `$DISK`가 없거나 비어 있음. **내장 디스크에 `$ROOT`나 `.system`이 새로 생기지 않음** | |
| 5.6 | 대시보드(브라우저) 새로고침 | 상단 "저장 중단됨", 저장장치 화면 경고 | |
| 5.7 | 디스크 다시 연결, 마운트 확인 후 `stat -f 'st_dev=%d' "$ROOT"` | st_dev 값 기록: ____ (5.1과 같은지/다른지) | |
| 5.8 | 재시작 **없이** `wa_health; wa_upload "$E2E/x.bin"` | 기록: st_dev가 같으면 `storage ok`·`HTTP 201`로 복귀 예상, 다르면 `503` 유지 예상. 실제 결과 기록 | |
| 5.9 | `launchctl kickstart -k gui/$(id -u)/org.withalbum.server; sleep 5; wa_health; wa_login e2eadmin; wa_upload "$E2E/x.bin"` | 재시작 후 `storage ok`. 업로드는 새 키라 `HTTP 201`, 5.8에서 같은 파일이 이미 올라갔다면 `HTTP 200 already_present` | |
| 5.10 | 디스크 없이 시작: 디스크 분리 → `launchctl kickstart -k gui/$(id -u)/org.withalbum.server; sleep 5; tail -3 ~/Library/Logs/WithAlbum/error.log; ls /Volumes` | `does not exist ... registered by the setup screen ... connect the disk` 로그 반복(30초 간격), `$ROOT`가 생기지 않음 | |
| 5.11 | 디스크 연결 → 60초 대기 → `wa_health` | KeepAlive 재시도로 자동 기동, `storage ok`. 걸린 시간 기록 | |
| 5.12 | 결론 기록: "디스크 재연결 시 재시작 필요: 예/아니오(조건: ____)" | 결과에 따라 문서·대시보드 문구 유지 또는 수정 필요 여부를 운영 기록에 남김 | |

## 6. 재부팅 복구

| # | 명령 / 동작 | 기대 결과 | 결과 |
|---|---|---|---|
| 6.1 | 재부팅 전: `wa_upload "$E2E/small.bin"` 등으로 사진 몇 개, `"$BIN" verify` 결과 `OK` 확인 | | |
| 6.2 | Apple 메뉴 > 재시동 → 로그인 | | |
| 6.3 | 로그인 후 `wa_health`가 응답할 때까지 시간 측정, `grep -E "recovery:\|does not exist" ~/Library/Logs/WithAlbum/error.log \| tail -5` | 외장 디스크가 늦게 마운트되면 처음 몇 번 `does not exist` 후 자동 기동. `recovery: ... missing=0 size_mismatch=0` | |
| 6.4 | `wa_login e2eadmin` 후 이전 자산 목록: `curl -s "$WA/api/v1/folders/$(cat "$E2E/fid")/assets" -H "$(wa_auth)" \| head -c 600` | 재부팅 전 자산이 모두 있음 | |
| 6.5 | 원본 다운로드 비교: `small.bin`을 올린 응답(5.1 등)의 `asset_id`로 `curl -s -o "$E2E/dl.bin" "$WA/api/v1/folders/$(cat "$E2E/fid")/assets/<asset_id>/original" -H "$(wa_auth)"; shasum -a 256 "$E2E/dl.bin" "$E2E/small.bin"` | 두 해시 같음 | |
| 6.6 | 이어받기(Range): `curl -s -D - -o /dev/null -H "Range: bytes=0-99" "$WA/api/v1/folders/$(cat "$E2E/fid")/assets/<asset_id>/original" -H "$(wa_auth)" \| grep -iE "^HTTP\|content-range"` | `HTTP/1.1 206`, `Content-Range: bytes 0-99/<크기>` | |

## 7. 전원 차단 후 무결성 (`verify -hash`)

| # | 명령 / 동작 | 기대 결과 | 결과 |
|---|---|---|---|
| 7.1 | 성공 응답을 받은 업로드의 sha256 목록을 `$E2E/committed.txt`에 기록(응답의 `sha256` 값) | | |
| 7.2 | 큰 업로드 중 차단: `wa_upload "$E2E/large.bin" key-power-1` 실행 중(진행 수 초 뒤) **Mac mini 전원 케이블을 뽑는다** | (응답 없음) | |
| 7.3 | 전원 연결 → 부팅 → 로그인 → `wa_health` | 기동 성공. 외장 디스크가 "올바르게 꺼내지 않음"으로 검사(fsck)되면 그 시간 기록 | |
| 7.4 | `grep "recovery:" ~/Library/Logs/WithAlbum/error.log \| tail -1` | `incoming_removed`가 0 또는 1 이상(미완료 임시 파일 정리), `missing=0 size_mismatch=0` | |
| 7.5 | 서버를 멈추지 않고 `"$BIN" verify -hash; echo "exit=$?"` (설정 파일 루트를 사용) | `OK`, `exit=0`. 7.1의 모든 sha256이 있음 | |
| 7.6 | 같은 키로 재시도: `wa_login e2eadmin; wa_upload "$E2E/large.bin" key-power-1` | `HTTP 201` 새로 커밋되거나, 차단 전에 커밋이 끝났다면 같은 결과 재응답(`Idempotency-Replayed: true`). 어느 쪽이든 자산은 하나 | |
| 7.7 | 대기 중 차단: 업로드가 없을 때 전원 차단 → 부팅 → `"$BIN" verify -hash` | `OK` | |

## 8. 백업·복원 훈련

절차의 자세한 설명: [`BACKUP_RESTORE.md`](BACKUP_RESTORE.md).

| # | 명령 | 기대 결과 | 결과 |
|---|---|---|---|
| 8.1 | 백업 대상 준비: `mkdir "/Volumes/<백업디스크>/WithAlbumBackup" && "$BIN" backup -init -to "/Volumes/<백업디스크>/WithAlbumBackup"` | 백업 대상 등록 | |
| 8.2 | 서버 실행 중 업로드하면서 백업: 한 터미널에서 `wa_upload "$E2E/large.bin"`, 다른 터미널에서 `"$BIN" backup -to "/Volumes/<백업디스크>/WithAlbumBackup"; echo "exit=$?"` | `backup: OK snapshot <id>`, `exit=0` | |
| 8.3 | `"$BIN" restore -from "/Volumes/<백업디스크>/WithAlbumBackup/snapshots/<id>" -check` | `OK`(모든 원본 해시 확인) | |
| 8.4 | 새 위치로 복원(실제 라이브러리 아님): `mkdir "$DISK/WithAlbumRestore" && WITHALBUM_ROOT="$DISK/WithAlbumRestore" "$BIN" -init-storage && WITHALBUM_ROOT="$DISK/WithAlbumRestore" "$BIN" restore -from ".../snapshots/<id>"` | 복원 완료 | |
| 8.5 | 다른 포트로 기동: `WITHALBUM_ROOT="$DISK/WithAlbumRestore" WITHALBUM_LISTEN=127.0.0.1:18787 "$BIN"` → `WA=http://127.0.0.1:18787 wa_login e2eadmin` → 원본 하나 내려받아 해시 비교 | 로그인 가능(세션은 모두 폐기되어 다시 로그인), 해시 같음, `recovery: ... missing=0` | |
| 8.6 | `WITHALBUM_ROOT="$DISK/WithAlbumRestore" "$BIN" verify -hash` | `OK` | |
| 8.7 | 8.5의 18787 서버가 **실행 중일 때** 그 저장소로 복원 시도: `WITHALBUM_ROOT="$DISK/WithAlbumRestore" "$BIN" restore -from ".../snapshots/<id>"` | `LOCKED`로 거부, 데이터 변화 없음(실제 라이브러리 `$ROOT`로는 시험하지 말 것) | |
| 8.8 | 정리: 18787 서버 Ctrl+C, `rm -rf "$DISK/WithAlbumRestore"` | | |

## 9. LAN TLS 접속 (`-allow-lan -tls`) + 지문 확인

LAN 리스너에는 설치 화면이 없습니다. 2장(또는 3장)에서 관리자를 만든 뒤 진행합니다. 공유기 포트포워딩은 하지 마세요.

| # | 명령 / 동작 | 기대 결과 | 결과 |
|---|---|---|---|
| 9.1 | Mac의 사설 IP: `ipconfig getifaddr en0` (유선이면 en0, Wi-Fi면 en1일 수 있음) → `export MAC_IP=<값>` | 사설 대역(10/8, 172.16/12, 192.168/16) | |
| 9.2 | 거부 확인(포그라운드, LaunchAgent는 잠시 `launchctl bootout gui/$(id -u) ~/Library/LaunchAgents/org.withalbum.server.plist`): `WITHALBUM_LISTEN="$MAC_IP:8787" "$BIN" -allow-lan` (TLS 없음) | `SECURITY:`로 기동 거부 | |
| 9.3 | `WITHALBUM_LISTEN=8.8.8.8:8787 "$BIN" -tls -allow-lan` | 공인 IP라 기동 거부 | |
| 9.4 | `bash scripts/install-macos.sh --lan "$MAC_IP:8787" "$BIN"` | `serving https://<MAC_IP>:8787 (TLS, LAN)` | |
| 9.5 | `WITHALBUM_LISTEN="$MAC_IP:8787" "$BIN" tls` | `spki_sha256` 64자리와 `withalbum://pair?v=1&url=https://...&spki_sha256=...`. 값 기록: ____ | |
| 9.6 | macOS 방화벽이 "들어오는 연결 허용"을 묻는다면 그 동작 기록 | | |
| 9.7 | **다른 기기**에서: `curl -sk "https://$MAC_IP:8787/api/v1/health"` | `{"status":"ok",...}` | |
| 9.8 | 다른 기기에서 지문: `openssl s_client -connect "$MAC_IP:8787" -tls1_3 </dev/null 2>/dev/null \| openssl x509 -pubkey -noout \| openssl pkey -pubin -outform der \| shasum -a 256` | 9.5의 `spki_sha256`과 같음 | |
| 9.9 | 다른 기기에서 TLS 1.2: `openssl s_client -connect "$MAC_IP:8787" -tls1_2 </dev/null` | 핸드셰이크 실패 | |
| 9.10 | 다른 기기에서 평문: `curl -s "http://$MAC_IP:8787/api/v1/health"; echo "exit=$?"` | 응답 없음/오류(평문은 루프백만) | |
| 9.11 | 다른 기기에서 로그인(9.7과 같은 `-k`로): `curl -sk -X POST "https://$MAC_IP:8787/api/v1/auth/login" -H 'Content-Type: application/json' -d '{"login_name":"e2eadmin","password":"<비밀번호>","device_name":"lan","platform":"ios"}' \| head -c 200` | 토큰 응답(`session_expires_at` 포함). 셸 기록에 비밀번호가 남으니 시험 후 `history -c` 또는 시험 계정 비밀번호 변경 | |
| 9.12 | 서버 재시작(`launchctl kickstart -k gui/$(id -u)/org.withalbum.server`) 후 `WITHALBUM_LISTEN="$MAC_IP:8787" "$BIN" tls` | 지문 변하지 않음 | |
| 9.13 | 시험 후 루프백으로 되돌리기: `bash scripts/install-macos.sh "$BIN"` | | |

## 10. 대시보드·설치 화면 브라우저 점검

[`DASHBOARD_CHECKLIST.md`](DASHBOARD_CHECKLIST.md)를 이 Mac mini의 Safari와 Chrome에서 그대로 수행하고, 브라우저·버전·통과/실패 항목을 그 문서 형식대로 기록합니다. 설치 화면 시나리오는 실제 외장 디스크로 한 번 더 확인합니다(2장과 별도로 `WITHALBUM_CONFIG`를 임시 경로로 지정해 실제 설정 파일을 건드리지 않게).

| # | 항목 | 결과 |
|---|---|---|
| 10.1 | DASHBOARD_CHECKLIST "설치 화면 시나리오" — Safari | |
| 10.2 | DASHBOARD_CHECKLIST "공통" 이후 전체 — Safari | |
| 10.3 | 같은 항목 — Chrome | |
| 10.4 | 5장 디스크 분리 중 대시보드 "저장 중단됨" 화면 | |
| 10.5 | 9장 LAN TLS에서 `https://localhost:8787/` 대시보드(자체 서명 경고 문구 기록) | |

## 11. 결과 요약 (릴리스 노트·운영 기록으로 옮길 표)

| 장 | 항목 | 결과 (통과/실패/미검증) | 근거 요약 |
|---|---|---|---|
| 1 | 산출물 체크섬·`-version`·Gatekeeper | | |
| 2 | 설치 화면 경로 | | |
| 3 | 콘솔 경로 | | |
| 4 | launchd 자동 시작·재시작·로그인 후 기동 | | |
| 5 | 디스크 분리 fail closed / 재연결 시 재시작 필요 여부 | | st_dev 전 ____ / 후 ____ |
| 6 | 재부팅 복구 | | |
| 7 | 전원 차단 후 `verify -hash` | | |
| 8 | 백업·복원 훈련 | | |
| 9 | LAN TLS·지문 | | |
| 10 | 브라우저 점검 | | |

마무리: `rm -rf "$E2E"`(토큰·테스트 파일), 시험용 계정 비밀번호 변경 또는 시험 저장소 삭제, LaunchAgent를 계속 쓸지 결정.
