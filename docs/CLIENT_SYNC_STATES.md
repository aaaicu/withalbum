# 클라이언트 상태 머신 초안

## 업로드 아이템

```text
IMPORTING -> STAGED -> HASHING -> READY -> UPLOADING -> VERIFYING -> SYNCED
                          ^          |          |           |
                          +----------+          v           v
                       (422 hash_mismatch) RETRY_WAIT <-----+
                                                |
                                                +-> READY

IMPORTING | STAGED | HASHING | READY | UPLOADING | VERIFYING -> NEEDS_ATTENTION
NEEDS_ATTENTION -> READY   (사용자 조치 뒤에만)

SYNCED -> PREVIEW_PENDING -> OPTIMIZED (SMART_PREVIEW)
      \-> ORIGINAL_RETAINED (KEEP_ORIGINAL)
```

- IMPORTING: 공유 시트 임시 URI/파일을 안전한 앱 스토리지로 복사; 완료 전에는 성공 표시하지 않음.
- STAGED: 고유 아이템 ID·폴더 ID·실제 파일 위치·크기·파일 수명 확인.
- HASHING: SHA-256. 대용량은 작업 취소/진행률 제공.
- READY: 서버 연결 가능하면 사전 조회(optional), 없으면 스테이징 보존. 422 `hash_mismatch` 뒤에는 HASHING으로 돌아가 스테이징 사본 해시를 다시 확인.
- UPLOADING: 요청 ID/Idempotency-Key 유지, 재시도는 가변 슬롯이 아니라 동일 논리 요청을 재생.
- VERIFYING: 서버 응답이 요청 폴더·정확한 해시·자산 ID와 일치하는지 확인.
- RETRY_WAIT: 일시 오류(연결 끊김·응답 유실, 503·429·`upload_busy`, 500, 앱 강제 종료 뒤 복구). 같은 Idempotency-Key·재개 세션으로 백오프 뒤 READY. 서버 `Retry-After`가 백오프보다 우선.
- NEEDS_ATTENTION: 자동 재시도 금지 — 사용자 조치가 있어야만 READY로 돌아간다(아래 "NEEDS_ATTENTION" 절).
- SYNCED: 원본이 서버에 저장됐다는 사실. **서버 백업 2차본 존재 여부는 별도 표시**.
- PREVIEW_PENDING: 네트워크 장애 시 **원본 스테이징을 유지**. 앱 내부 사본 정리(경량화) 조건: **원격 원본 SHA-256 검증 완료 + `preview_status`가 `pending`이 아님(`failed`/`unsupported`면 앱이 자체 썸네일 보관)**.
- OPTIMIZED: 위 조건을 만족해 앱 내부 원본 스테이징을 삭제, 축소본(서버 프리뷰 또는 앱 자체 썸네일)/메타데이터 유지. 기본 사진 앱(Photos/Gallery)의 원본은 절대 삭제하지 않는다.
- ORIGINAL_RETAINED: 로컬 원본 유지 옵션.

## 수신 아이템

```text
METADATA_ONLY -> PREVIEW_QUEUED -> PREVIEW_READY -> ORIGINAL_TEMPORARY
      |                |
      +-- RETRY_WAIT <-+
      |
      +-- REMOVED_FROM_FOLDER (tombstone)
```

- 클라이언트 인덱스 PK는 `(server_id, folder_id, asset_id)`. 동일 원본 해시가 다른 폴더에도 존재하면 별도 폴더 소속으로 표시.
- `cursor`는 이벤트 메타데이터가 로컬 DB에 영속화된 다음에만 앞으로 보냄.
- `PREVIEW_READY` 파일은 LRU 캐시 정책과 앱 로컬 저장공간 상한으로 제거 가능. 서버 원본은 유지.
- 수신 루프는 uploader_device_id가 같아도 메타데이터를 적용; 캐시가 있을 때만 다운로드 생략.
- `uploader_device_id`는 그 자산을 이 폴더에 올린 업로드의 X-Device-ID 라벨: `asset_added`와 `asset_updated`(같은 자산의 `asset_added`와 같은 값 — 변경은 서버가 했음, 라벨 없이 올렸으면 "")에 실리고, `asset_removed`는 항상 "".
- `asset_removed`에 의해 로컬 폴더 연결을 해제하지만, 동일 자산이 다른 폴더에 있으면 공용 캐시는 유지.

## NEEDS_ATTENTION

자동 재시도하면 안 되는 실패. 큐는 그 아이템을 멈추고 사유를 표시하며, 사용자 조치 뒤에만 READY로 되돌린다(같은 Idempotency-Key·재개 세션 유지). 앱의 업로드 큐는 아래 사유를 그대로 구분한다.

| 사유 | 원인 | 사용자 조치 → READY |
|---|---|---|
| 공간 부족 | 507 `insufficient_storage` | 서버 공간 확보 뒤 다시 시도 |
| 인증 필요 | 401 (refresh도 실패) | 재로그인 뒤 일괄 재개 |
| 권한 없음 | 403 `forbidden` (VIEWER로 하향 등) | 권한 복구 뒤 다시 시도 |
| 앨범 없음 | 404 `folder_not_found` (추방·보관·삭제) | 멤버십 복구 뒤 다시 시도 |
| 핀 불일치 | TLS SPKI 핀 불일치(보안 사고 가능) | 서버 확인·재페어링 뒤 |
| 잘못된 요청 | 400·413·415 | 원인 확인 뒤(대개 제거) |
| 키 재사용 | 422 `idempotency_key_reused`가 새 키 1회 뒤에도 반복(앱 버그) | 확인 뒤 |
| 검증 실패 | 성공 응답의 폴더·해시·크기·자산 ID가 로컬 값과 다름 | 확인 뒤 |
| 재시도 한도 초과 | 한도에 세는 연속 실패 10회(연결 끊김·응답 유실은 세지 않음) | 다시 시도 |
| 기타 충돌 | 그 밖의 409·지원하지 않는 기능 | 확인 뒤 |
| 가져오기 실패·스테이징 사본 소실·손상 | 앱 저장소 문제 | **복구 불가** — 제거 후 다시 공유 |

- NEEDS_ATTENTION에서 갈 수 있는 상태는 READY 하나뿐이고, 자동 타이머·연결 복구로는 가지 않는다.
- 커밋 여부가 불확실한 실패(연결 끊김·응답 유실)는 NEEDS_ATTENTION이 아니라 같은 키로 RETRY_WAIT — 서버가 저장된 결과를 재응답한다.

## 기본 실패 시나리오/필수 테스트

1. 공유 확장 종료 직후: 원본 스테이징 정상, 앱 다음 실행 시 큐 복구.
2. 업로드 중 네트워크 단절: 전송 재시도, 중복 등록 없음.
3. 서버 파일 저장 후 응답 손실: 같은 idempotency 키로 재시도, 원본 중복 저장 없음.
4. 디스크 분리 또는 가득 참: `SYNCED`로 승격하지 않음.
5. 프리뷰 생성 실패(HEIC, RAW, 4K, 잘못된 파일): 서버 원본만 보존, 오류 및 재처리 상태 표시.
6. 두 사람이 같은 사진을 같은/다른 폴더에 업로드: 물리 원본은 하나, 폴더 연결은 의도대로.
7. 사용자의 기기가 오랫동안 오프라인이었다가 복귀: cursor 이후 events로 최신화.
8. 멤버 권한 철회: 서버 API 즉시 차단, 로컬 캐시 정리 정책 적용.
9. 저장소 유실/서버 DB 복원: 파일 재스캔 및 스냅샷 동기화 설계 필요.
10. 사진 앱 원본 직접 삭제: PhotoHub 스테이징 복사본과 서버 원본에 영향 없음.

## 서버 API 대응 (docs/API_v2.yaml, M16)

- UPLOADING: 큐 아이템마다 `Idempotency-Key`(UUID 권장)를 정해 영속화하고 모든 재시도에 그대로 보낸다. 같은 키·같은 바이트 재시도는 첫 커밋 결과(같은 상태 코드·asset_id, `Idempotency-Replayed: true`)를 받고 서버에 아무것도 더 저장되지 않는다. 같은 키·다른 바이트는 422 `idempotency_key_reused`(앱 버그) → 새 키로 다시. 키는 계정·기기·폴더 범위, 30일 보존.
- VERIFYING: 200/201은 응답 `folder_id`·`sha256`·`asset_id`를, 202(UPLOADER)는 `folder_id`·`sha256`·`bytes`를 로컬 값과 대조. 이 외 상태는 SYNCED 금지(507은 자동 재시도 금지, 503 busy는 Retry-After).
- READY 사전 조회: `POST /folders/{id}/assets/check`는 읽기 권한자만, 이 폴더만. UPLOADER는 사전 조회 없이 업로드.
- 수신: 폴더별 cursor(문자열)를 페이지를 로컬 DB에 저장한 뒤에만 갱신. `has_more`면 이어서 요청. `asset_removed`는 폴더 연결 해제. 409(`cursor_invalid`: 다른 폴더·서버 복원·앞선 cursor / `cursor_expired`: 보존 범위 밖)면 그 폴더 인덱스를 버리고 `GET /folders/{id}/assets` 스냅샷을 `after`로 끝까지 받은 뒤 첫 페이지의 `cursor`부터 피드를 이어 받는다(시나리오 9의 서버 측 대응).
- 권한 철회(8): 다음 요청이 404(추방)·403(UPLOADER로 하향) → 앱은 해당 폴더 캐시 정리 정책 적용.
- 서버 측 자동화된 시나리오: 2·3·6·7·8 (`internal/httpapi/sync_v2_test.go`).

## 재개 업로드·프리뷰 상태 (M22)

- 512MiB를 넘거나 네트워크가 불안정하면 업로드 세션 사용: `POST /folders/{id}/uploads`(전체 크기·SHA-256, 큐 아이템의 같은 `Idempotency-Key`)로 만들고 `upload_id`를 큐 아이템에 영속화. 청크는 `PATCH` + `Upload-Offset`(서버 offset과 같아야 함), 가능하면 `X-Chunk-SHA256`. 앱 재시작·연결 끊김 뒤에는 `GET .../uploads/{uid}`의 `offset`부터 이어서 보낸다(409 `offset_mismatch`의 `offset`도 같은 의미). 끝까지 받으면 `POST .../complete` → 단일 업로드와 같은 VERIFYING 규칙. 응답 유실 시 complete 재시도(또는 같은 키로 세션 재생성 → `status: completed`면 complete)로 같은 결과.
- 커밋된 뒤 세션이 만료·삭제됐으면(24시간, 정리, 완료 후 취소) 같은 키로 세션을 다시 만들 때 세션 대신 **저장된 커밋 결과**가 온다: 단일 업로드 재응답과 같은 형태(읽기 권한자 201 `created`/200 `already_present` + `asset_id`, UPLOADER 202 영수증 — `asset_id` 없음), `Idempotency-Replayed: true`, `Upload-Offset` 없음. 앱은 이 헤더(또는 본문 `result`)로 구별해 complete 응답처럼 VERIFYING한다. 현재 역할 기준이라 권한을 잃었으면 403/404(→ NEEDS_ATTENTION). 영수증 보존 기간(커밋 후 30일)이 지나면 새 요청으로 취급되어 새 세션(201, offset 0)이 만들어지고, 다시 전부 보낸 뒤 폴더에 바이트가 남아 있으면 `already_present`(UPLOADER는 202).
- 세션 404(`upload_not_found`: 24시간 만료·서버 복원 등)면 같은 키로 새 세션을 만들어 0부터. 422 `hash_mismatch`는 서버가 받은 바이트를 버리고 offset 0으로 되돌린 것 → 로컬 원본 해시 재확인 후 재전송. 507은 자동 재시도 금지.
- 프리뷰: 응답·자산의 `preview_status`가 `pending`이면 서버 워커가 만드는 중(HEIC·영상), 끝나면 그 폴더 피드에 `asset_updated`가 오고 `assets`에 최신 자산이 담김. `failed`/`unsupported`는 서버에 원본만 있음(앱은 자체 썸네일 보관, 상태 표시). 읽기 권한이 있는 앱은 **원격 원본 SHA-256 검증 완료(성공 응답의 `folder_id`·`sha256`이 로컬 값과 일치) + `preview_status`가 `pending`이 아님(`failed`/`unsupported`면 앱이 자체 썸네일 보관)**일 때만 앱 내부 임시 사본을 정리한다(SMART_PREVIEW). UPLOADER는 프리뷰를 볼 수 없으므로 검증된 202 영수증만으로 판단. 기본 사진 앱 원본은 삭제하지 않는다.
