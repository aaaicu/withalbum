# 백업 · 복원 · 무결성 검사

WithAlbum 서버는 세 개의 콘솔 명령으로 라이브러리(메타데이터 DB + 원본 + 프리뷰)를 지킵니다.

| 명령 | 하는 일 | 서버 실행 중 |
|---|---|---|
| `withalbum-server verify [-hash] [-json]` | DB `integrity_check`·`foreign_key_check`, 모든 원본의 존재·크기(·`-hash`면 SHA-256) 확인. **아무것도 바꾸지 않음** | 가능 (잠금 안 잡음) |
| `withalbum-server backup -to DIR [-rehash] [-json]` | 일관된 스냅샷(DB `VACUUM INTO` + 원본 증분 복사 + SHA-256 매니페스트) | 가능 (백업 대상의 잠금만) |
| `withalbum-server restore -from SNAPSHOT [-check] [-replace] [-json]` | 스냅샷 검증 후 저장소에 원자적으로 활성화 | **불가** — 서버를 먼저 중지 (`LOCKED`로 거부) |

`restore -check`는 저장소를 건드리지 않고 스냅샷만 전수 검증하므로 서버 실행 중에도 됩니다.

> **백업은 민감정보입니다.** 스냅샷 DB에는 계정의 비밀번호 해시(Argon2id), 세션 토큰 해시, 감사 로그, 앨범·파일 이름이 들어 있고, `pool/`에는 모든 원본 사진이 있습니다. 백업 디스크를 라이브러리 디스크만큼 보호하세요. 파일은 0600(원본 0400), 디렉터리는 0700으로 만들어지며 `backup -init`은 대상 디렉터리를 0700으로 바꿉니다. 클라우드 동기화 폴더나 공유 폴더를 백업 대상으로 쓰지 마세요.

> **같은 디스크의 백업은 백업이 아닙니다.** 대상이 저장소와 같은 파일시스템이면 `backup`이 경고를 출력합니다. 디스크 고장·분실에 대비하려면 별도 외장 디스크를 쓰세요.

## 처음 한 번: 백업 디스크 등록

```bash
# 백업 디스크에 빈 디렉터리를 직접 만든다 (WithAlbum은 대상을 자동 생성하지 않음)
mkdir /Volumes/BackupDisk/WithAlbumBackup
WITHALBUM_ROOT=/Volumes/ExternalSSD/WithAlbum ./withalbum-server backup -init -to /Volumes/BackupDisk/WithAlbumBackup
```

`-init`은 마커 `.withalbum-backup-target.json`(랜덤 target_id)을 기록합니다. 저장소 마커(`.withalbum-volume.json`)와 다른 파일이라 백업 디스크를 서버 저장소로 잘못 열 수 없고, 그 반대도 거부됩니다. 이후 모든 백업은 대상의 마커와 장치 번호를 **쓰기 직전마다** 재확인하므로, 백업 디스크가 빠져서 같은 경로가 내장 디스크에 생겨도 거기에 쓰지 않습니다(저장소 볼륨 가드와 같은 원리).

## 백업

```bash
WITHALBUM_ROOT=/Volumes/ExternalSSD/WithAlbum ./withalbum-server backup -to /Volumes/BackupDisk/WithAlbumBackup
```

- 서버를 멈출 필요가 없습니다. 업로드 중에도 스냅샷은 일관됩니다:
  1. DB는 SQLite `VACUUM INTO`로 한 읽기 트랜잭션 시점의 완전한 사본을 만듭니다(WAL 내용 포함, 업로드를 막지 않음).
  2. 그 스냅샷이 가리키는 원본만 복사합니다. 업로드는 원본 파일을 pool에 쓰고 fsync한 **뒤에** DB 행을 커밋하므로, 스냅샷에 있는 원본은 반드시 이미 존재합니다. pool 파일은 한번 쓰면 바뀌지 않습니다.
  3. 원본은 복사하면서 해시를 계산해 이름(SHA-256)과 비교합니다. 원본이 손상돼 있으면 좋은 백업 사본을 덮어쓰지 않고 백업을 실패로 끝냅니다.
- **증분:** 원본·프리뷰는 대상의 `pool/`·`previews/`를 모든 스냅샷이 공유하므로 새 파일만 복사됩니다. 이미 있는 사본은 크기로 확인합니다. `-rehash`를 주면 대상 사본도 다시 해시해, 손상된 사본은 `damaged/`로 옮기고(삭제 안 함) 원본에서 다시 복사합니다.
- 스냅샷은 `snapshots/.partial-<id>/`에 만들어지고, DB·원본·매니페스트가 모두 fsync된 뒤에야 `snapshots/<id>/`로 rename됩니다. 중간에 끊기면 다음 백업이 `.partial-*`과 `tmp/`를 지우고(복사본일 뿐) 새로 만듭니다. 완성된 이전 스냅샷과 pool 파일은 그대로입니다.
- 종료 코드: `0` 완료, `1` 실패 또는 "스냅샷은 완성됐지만 라이브러리 자체에 사라지거나 크기가 다른 원본이 있음"(대상에 이미 좋은 사본이 있던 경우 — `verify`로 확인 후 복구), `2` 잘못된 인자.

### 백업 대상 구조

```text
WithAlbumBackup/
├── .withalbum-backup-target.json   # 백업 대상 마커
├── withalbum.lock                  # 백업 중 잠금 (동시에 두 백업 금지)
├── pool/<sha[:2]>/<sha>            # 원본 (모든 스냅샷 공유, 0400)
├── previews/<sha[:2]>/<sha>.jpg    # 프리뷰 (모든 스냅샷 공유)
├── snapshots/<UTC>-<rand>/
│   ├── withalbum.db                # DB 스냅샷 (0600)
│   └── manifest.json               # 형식, 생성 시각, 원본 volume_id, 스키마 버전, DB 크기·SHA-256, 원본 목록(sha256·bytes), 프리뷰 목록(파일 SHA-256)
├── tmp/                            # 복사 중 임시 파일
└── damaged/                        # -rehash가 찾은 손상 사본 (자동 삭제 안 함)
```

오래된 스냅샷은 `snapshots/<id>` 디렉터리를 지워 정리할 수 있습니다. `pool/`의 원본은 다른 스냅샷이 쓸 수 있으므로 지우지 마세요(자동 가비지 컬렉션은 아직 없음).

## 무결성 검사 (verify)

```bash
WITHALBUM_ROOT=/Volumes/ExternalSSD/WithAlbum ./withalbum-server verify          # 빠름: 존재·크기
WITHALBUM_ROOT=/Volumes/ExternalSSD/WithAlbum ./withalbum-server verify -hash    # 전수: 모든 원본 SHA-256
```

- 기동 재조정은 크기만 비교하므로 **같은 크기로 내용이 바뀐 손상**(배드 섹터, 비트 반전)은 `verify -hash`로만 찾습니다. 좋은 사본이 백업에 남아 있을 때 찾아야 하므로 정기적으로 실행하세요.
- 문제마다 원본 경로와 그 원본을 쓰는 자산(`asset_id`, 파일명, `folder_id`)을 보고합니다. 손상된 원본은 건드리지 않습니다.
- 경고(실패 아님): 준비 표시된 프리뷰 누락, DB 행 없는 pool 파일(진행 중 업로드이거나 다음 기동 때 `orphans/`로 격리), `.system/incoming` 파일 수, `.system/orphans` 파일 수·용량.
- 종료 코드: `0` 정상, `1` 문제 발견, `3` 검사 자체 실패(저장소·DB 열기 불가 등).
- DB를 짧은 쿼리로 나눠 읽어 서버의 WAL 체크포인트를 오래 막지 않습니다. `-hash`는 라이브러리 전체를 읽으므로 디스크 부하가 큽니다(속도 제한 옵션은 아직 없음).

### 손상·누락 원본 복구

1. `verify -hash`가 알려준 sha256의 사본이 백업에 있는지: `ls <백업>/pool/<sha 앞 2자>/<sha>` 후 `shasum -a 256`으로 이름과 같은지 확인.
2. 서버를 중지하고, 손상된 파일을 지우지 말고 다른 곳으로 옮긴 뒤 백업 사본을 `.system/pool/<sha 앞 2자>/<sha>`에 복사(`chmod 0400`).
3. `verify -hash`로 확인. 또는 같은 파일을 앱에서 다시 업로드해도 복구됩니다(같은 SHA-256).
4. 라이브러리 전체가 의심스러우면 아래 "복원"을 하세요.

### orphans/ 보존 정책

`.system/orphans/`에는 기동 재조정이 격리한 "DB에 없는 원본"(업로드가 커밋 직전에 끊긴 파일)과 교체된 손상 파일이 `<sha256>.<사유>.<UTC>` 이름으로 들어갑니다. **서버는 이 디렉터리를 절대 자동 삭제하지 않습니다.** 클라이언트는 성공 응답을 받지 못했으므로 다시 업로드하는 것이 정상이고, 그러면 같은 원본이 pool에 다시 들어옵니다. `verify`가 파일 수·용량을 보고하니, 최근 백업이 완료되고 `verify`가 정상인 상태에서 운영자가 판단해 지우세요.

## 복원

```bash
# 0) 서버 중지 (restore는 저장소 잠금을 잡으며 서버 실행 중이면 LOCKED로 거부)
# 1) 스냅샷만 검증 (아무것도 쓰지 않음, 모든 원본 해시)
./withalbum-server restore -from /Volumes/BackupDisk/WithAlbumBackup/snapshots/<id> -check
# 2) 새 디스크에 저장소 초기화 후 복원
mkdir /Volumes/NewSSD/WithAlbum
WITHALBUM_ROOT=/Volumes/NewSSD/WithAlbum ./withalbum-server -init-storage
WITHALBUM_ROOT=/Volumes/NewSSD/WithAlbum ./withalbum-server restore -from /Volumes/BackupDisk/WithAlbumBackup/snapshots/<id>
# 3) 서버 기동, verify
```

- 기본은 **비어 있는 초기화된 저장소에만** 복원합니다(`.system`에 잠금 파일 외 내용이 있으면 거부). 기존 라이브러리 위에 복원하려면 `-replace`: 기존 `.system`은 `.system.before-restore-<UTC>`로 이름만 바뀌어 **보존**되며 삭제되지 않습니다. 복원 결과를 확인한 뒤 직접 지우세요.
- 절차:
  1. 매니페스트 형식 검사, 스냅샷 DB의 SHA-256이 매니페스트와 같은지, `integrity_check`·`foreign_key_check`, 스키마 버전이 이 바이너리보다 높지 않은지, DB의 blobs 목록이 매니페스트와 정확히 같은지 확인.
  2. `<root>/.system-restore`(스테이징)에 DB·원본(0400)·프리뷰를 복사하며 하나하나 해시 검증. 실패하면 아무것도 활성화하지 않음.
  3. 스테이징 DB를 이 바이너리 스키마로 마이그레이션하고, **모든 세션과 기기를 폐기**(`revoke_reason=restore`)하고 감사 로그 `storage.restore`를 같은 트랜잭션에 기록, WAL 체크포인트.
  4. 저널 `<root>/.withalbum-restore.json` 기록 → 기존 `.system`을 보존 이름으로 rename → 스테이징을 `.system`으로 rename → 저널 삭제. 각 단계 fsync.
- **세션 폐기 이유:** 백업 시점 이후에 해지한 기기(분실한 휴대폰 등)의 세션이 복원으로 되살아나면 안 되기 때문입니다. 복원 후 모든 기기와 웹은 다시 로그인해야 합니다. 비밀번호는 백업 시점의 값으로 돌아갑니다(그 뒤에 바꾼 비밀번호는 무효) — 필요하면 `admin reset-password`.
- 백업 이후에 올라온 사진은 복원된 라이브러리에 없습니다. `-replace`로 덮었다면 `.system.before-restore-<UTC>/pool`에 남아 있습니다.
- 복원 후 저장소 루트에 `.withalbum-restore.lock`(복원 명령끼리의 잠금)이 남습니다. 지워도 됩니다.

### 중단된 복원

- 저널을 쓰기 전에 끊기면: 기존 `.system`은 그대로이고 서버도 정상 기동합니다. `.system-restore`는 다음 복원이 지우고 새로 만듭니다.
- 저널을 쓴 뒤에 끊기면: 서버·`admin`·`migrate-legacy`·`verify`·`backup`이 모두 "복원이 중단됨"으로 기동을 거부합니다(반쯤 바뀐 저장소에서 새 DB를 만들지 않도록). **같은 `restore` 명령을 다시 실행**하면 복사 없이 rename만 이어서 끝냅니다.

## 서버 실행 중 가능한 작업 / 중지가 필요한 작업

| 작업 | 잠금 | 서버 실행 중 |
|---|---|---|
| 서버 | `.system/withalbum.lock` (배타) | — |
| `verify`, `verify -hash` | 없음 (읽기 전용) | 가능 |
| `backup` | 백업 대상의 `withalbum.lock` | 가능 |
| `restore -check` | 없음 | 가능 |
| `restore` | `.system/withalbum.lock` + 루트 `.withalbum-restore.lock` | **불가** |
| `admin ...`, `migrate-legacy -apply` | `.system/withalbum.lock` | 불가 |

## 정기 실행 예시 (macOS launchd)

자동 스케줄러는 서버에 없습니다. 아래는 매일 03:00 백업, 매주 일요일 04:00 전수 검사 예시입니다. 경로는 본인 환경으로 바꾸세요. 결과는 종료 코드와 로그 파일로 확인합니다(실패 시 `StandardErrorPath`에 원인).

`~/Library/LaunchAgents/org.withalbum.backup.plist`:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>org.withalbum.backup</string>
  <key>ProgramArguments</key>
  <array>
    <string>/usr/local/bin/withalbum-server</string>
    <string>backup</string>
    <string>-to</string>
    <string>/Volumes/BackupDisk/WithAlbumBackup</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>WITHALBUM_ROOT</key><string>/Volumes/ExternalSSD/WithAlbum</string>
  </dict>
  <key>StartCalendarInterval</key>
  <dict><key>Hour</key><integer>3</integer><key>Minute</key><integer>0</integer></dict>
  <key>StandardOutPath</key><string>/tmp/withalbum-backup.log</string>
  <key>StandardErrorPath</key><string>/tmp/withalbum-backup.err</string>
</dict>
</plist>
```

전수 검사는 같은 형식으로 `ProgramArguments`를 `verify`, `-hash`로, `StartCalendarInterval`에 `<key>Weekday</key><integer>0</integer>`과 `Hour` 4를 넣은 `org.withalbum.verify.plist`를 만듭니다. 등록: `launchctl load ~/Library/LaunchAgents/org.withalbum.backup.plist`. 백업 디스크가 연결되지 않은 날은 "does not exist"로 실패하며 아무 데도 쓰지 않습니다. Linux는 같은 명령을 systemd timer나 cron으로 실행하세요.

## 복원 훈련 (분기마다 권장)

1. `restore -from <최신 스냅샷> -check` → `OK`.
2. 빈 디렉터리에 `-init-storage` 후 `restore` (실제 라이브러리 아님).
3. 그 디렉터리로 서버를 다른 포트(`WITHALBUM_LISTEN=127.0.0.1:18787`)에 기동해 로그인, 앨범·사진 몇 장의 원본을 내려받아 원래 파일과 SHA-256 비교. 기동 로그 `recovery: ... missing=0 size_mismatch=0`.
4. `verify -hash` → `OK`. 훈련용 디렉터리 삭제.

## 알려진 한계

- 실제 외장 디스크 분리·전원 차단 중 백업/복원은 실기기에서 검증하지 않았습니다(테스트는 단계별 장애 주입으로 재현).
- 백업 대상의 오래된 원본 정리(가비지 컬렉션), 스냅샷 보존 정책, 원격/클라우드 백업, 점진(분할) scrub과 마지막 검사 시각 기록은 없습니다.
- 백업 대상이 exFAT이면 파일 모드(0600/0400)가 적용되지 않습니다.
