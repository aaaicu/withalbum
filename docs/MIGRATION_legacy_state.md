# 이전 버전 메타데이터(state.json) → SQLite 이관

WithAlbum 서버의 초기 PoC는 폴더·자산·폴더 연결·변경 이벤트를 `.system/state.json` 하나에 저장했습니다. 현재 버전은 `.system/withalbum.db`(SQLite)를 사용하며, **state.json만 있는 저장소에서는 기동을 거부합니다**(파일은 읽지도 바꾸지도 않음). `withalbum-server migrate-legacy` 명령으로 state.json을 무손실 이관한 뒤 기동하세요.

이관은 서버 기동 시 자동으로 실행되지 않습니다. 운영자가 명시적으로 실행해야 합니다.

## 요약

```bash
# 0) 이전 버전 서버를 완전히 중지한다 (같은 저장소를 쓰는 모든 프로세스)
# 1) (M1 이전 버전에서 쓰던 폴더라면) 볼륨 마커를 한 번 등록
WITHALBUM_ROOT=/Volumes/ExternalSSD/WithAlbum ./withalbum-server -init-storage
# 2) 점검만 (아무것도 쓰지 않음)
WITHALBUM_ROOT=/Volumes/ExternalSSD/WithAlbum ./withalbum-server migrate-legacy
# 3) 이관 실행
WITHALBUM_ROOT=/Volumes/ExternalSSD/WithAlbum ./withalbum-server migrate-legacy -apply
# 4) 평소처럼 서버 기동
WITHALBUM_ROOT=/Volumes/ExternalSSD/WithAlbum ./withalbum-server
```

- `WITHALBUM_VOLUME_ID`를 지정해 두었다면 이관 명령도 같은 볼륨에서만 실행됩니다.
- `-json`을 붙이면 사람용 요약 대신 JSON 보고서를 표준 출력에 씁니다. 진행률은 표준 에러로 나옵니다.
- 종료 코드: `0` 성공, `1` 실패(원인 출력), `2` 잘못된 인자.

## 점검(dry-run, 기본값)

`-apply` 없이 실행하면 **아무것도 쓰지 않고**(볼륨 쓰기 가능 여부 확인용 임시 probe 파일만 만들었다 지움) 다음을 확인해 보고합니다.

- state.json 구문: 손상·잘림, 알 수 없는 필드, 중복 키, 뒤에 붙은 데이터, 최상위 키(`folders`·`assets`·`links`·`events`·`next_seq`) 누락은 모두 거부합니다.
- 내부 일관성: 키와 `id` 필드 일치, 자산 키가 SHA-256 형식인지, 폴더별 이벤트 seq가 1부터 빈틈없이 이어지는지, `next_seq`가 이벤트 수와 같은지, 폴더 연결마다 `asset_added` 이벤트가 정확히 하나 있는지, 존재하지 않는 폴더·자산 참조가 없는지.
- 파일: 모든 원본(`.system/pool/xx/<sha256>`)이 일반 파일로 존재하고 **크기와 SHA-256을 다시 계산해** state.json과 일치하는지, `preview_ready`인 자산의 프리뷰(`.system/previews/xx/<sha256>.jpg`)가 있는지.
- 경고(이관은 계속, 아무것도 지우지 않음): state.json이 참조하지 않는 pool 파일(이전 버전 크래시로 남은 고아 원본), 어느 폴더에도 연결되지 않은 자산(그대로 이관됨), 이전 버전의 임시 파일(`.system/.state-*`), `.system/incoming`의 미완료 업로드.

보고서에는 폴더·자산·연결·이벤트 수, 폴더별 마지막 seq, 원본 총 용량, 재해시에 걸린 시간이 나옵니다. `-apply`는 원본을 다시 한 번 재해시하므로 최소 그만큼 걸립니다.

## 이관(-apply) 절차

문제가 하나라도 있으면 아래 단계에 들어가기 전에 중단합니다(아무것도 쓰지 않음). 각 쓰기 단계 직전·직후에 저장소 볼륨(마커·장치 번호)을 다시 확인합니다.

1. **백업**: state.json을 `.system/state.json.bak-<UTC 시각>`으로 복사(임시 파일 → fsync → rename → 디렉터리 fsync)하고 다시 읽어 바이트가 같은지 확인합니다.
2. **임시 DB 작성**: `.system/withalbum.db.importing`(모드 0600)에 스키마 생성(마이그레이션 전체)과 모든 행 기록을 **트랜잭션 하나**로 커밋합니다. 같은 트랜잭션에 state.json 바이트의 SHA-256을 `legacy_imports`에 기록합니다.
3. **검증**: 임시 DB를 닫고 다시 열어 schema 버전, `integrity_check`, `foreign_key_check`, 테이블별 행 수, 그리고 폴더·자산·연결·이벤트·시퀀스 **모든 행의 모든 값**을 state.json과 대조합니다. WAL을 체크포인트해 DB 파일 하나에 모든 데이터가 들어 있게 한 뒤 파일과 디렉터리를 fsync합니다.
4. **확정**: state.json이 1단계 이후 바뀌지 않았는지(SHA-256) 다시 확인하고, `withalbum.db`가 없을 때만 임시 DB를 `withalbum.db`로 rename, 디렉터리 fsync.
5. **보존**: state.json을 `.system/state.json.migrated-<UTC 시각>`으로 rename합니다. **삭제하지 않습니다.**

원본(pool)·프리뷰 파일은 읽기만 하고 절대 수정·이동·삭제하지 않습니다. state.json의 내용도 바꾸지 않습니다(5단계에서 이름만 바뀜).

### 데이터 매핑

| state.json | SQLite |
|---|---|
| 폴더 `id`·`name`·`created_at` | `folders` (ID 그대로). 소유자는 로그인 불가한 로컬 bootstrap 관리자 `admin` |
| `next_seq[폴더]` | `folder_sequences.last_seq` (이후 업로드는 그다음 seq부터) |
| 자산(키 = SHA-256) | `blobs`(크기·MIME·pool 경로) + `assets`(**새 랜덤 `asset_id`**, 파일명, `preview_ready`, `created_at` = `uploaded_at`) + `legacy_asset_map`(SHA-256 → asset_id) |
| `links[폴더][sha]` | `folder_assets` (`added_at`, `uploader_device_id` → `uploader_device_label`) |
| `events[폴더][]` | `folder_changes` (**seq 그대로**, 종류, `occurred_at`, `uploader_device_id` → `uploader_device_label`) |
| state.json 바이트의 SHA-256 | `legacy_imports.source_sha256` (+ 이관 보고서 JSON) |

HTTP API의 자산 식별자는 아직 SHA-256이므로, 클라이언트가 보던 폴더 목록·sync 이벤트(커서 포함)·원본·프리뷰 응답은 이관 전과 같습니다.

## 실패와 중단 시 동작

| 언제 | 남는 상태 | 다시 실행하면 |
|---|---|---|
| 점검 단계에서 문제 발견 | 아무것도 바뀌지 않음 | 원인을 고친 뒤 실행 |
| 1~4단계 중 오류 감지(검증 불일치, 볼륨 소실, state.json 변경 등) | 임시 DB 즉시 삭제, `withalbum.db` 없음, state.json·원본 그대로, 백업 파일만 추가됨 | 처음부터 다시 진행 |
| 1~4단계 중 프로세스 강제 종료·정전 | `withalbum.db.importing`이 남을 수 있음. `withalbum.db`는 없음(3단계 검증을 통과한 파일만 rename됨) | 남은 임시 DB를 버리고 처음부터 다시 진행 |
| 4단계와 5단계 사이 중단 | `withalbum.db` + state.json(이관 기록 있음) | 5단계만 마저 실행. 이 상태에서도 서버는 기동함(DB가 이 state.json의 이관을 기록하고 있으므로) |
| 완료 후 | `withalbum.db`, `state.json.bak-*`, `state.json.migrated-*` | "no .system/state.json to import"로 거부(아무것도 하지 않음) |

**재이관 거부:** `withalbum.db`가 이미 있고 그 DB가 현재 state.json(SHA-256)의 이관을 기록하고 있지 않으면, 이관은 거부되고 아무것도 쓰지 않습니다. 이미 사용 중인 DB 위에 덮어쓰는 일은 없습니다.

**남은 DB 보조 파일:** `withalbum.db` 없이 `withalbum.db-wal`/`-shm`/`-journal`만 있으면 새 DB에 엉뚱한 WAL이 재생될 수 있으므로 거부합니다. 내용을 확인한 뒤 다른 곳으로 옮기고 다시 실행하세요.

## 되돌리기(롤백)

이관 후 이전 버전으로 돌아가야 한다면 (이관 후 새로 업로드한 데이터는 state.json에 없으므로 이전 버전에서 보이지 않습니다):

1. 모든 서버를 중지합니다.
2. `.system/withalbum.db`(와 `-wal`/`-shm`이 있으면 함께)를 저장소 밖 안전한 곳으로 **옮깁니다**(삭제하지 말 것).
3. `.system/state.json.migrated-<시각>`(또는 `.bak-<시각>`)을 `.system/state.json`으로 이름을 바꿉니다.
4. 이전 버전 서버를 기동합니다.

원본(pool)·프리뷰 파일은 이관 과정에서 바뀌지 않으므로 별도 복구가 필요 없습니다.

## 주의

- 이관 중에는 이전 버전 서버를 반드시 중지하세요. 이관은 확정 직전에 state.json이 바뀌었는지 확인하고 바뀌었으면 중단합니다. `-apply`는 `.system/withalbum.lock`을 잡으므로 이 버전 이후의 서버가 실행 중이면 `LOCKED`로 거부되지만, 잠금을 모르는 이전 버전 서버와의 동시 실행은 막지 못합니다.
- 백업 파일과 `.migrated-` 파일은 자동으로 지워지지 않습니다. 새 버전에서 충분히 확인한 뒤 직접 정리하세요.
- 대용량 저장소는 원본 전체를 두 번(점검, 이관) 읽습니다. 점검 보고서의 재해시 시간을 참고하세요.
