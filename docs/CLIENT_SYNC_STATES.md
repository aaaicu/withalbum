# 클라이언트 상태 머신 초안

## 업로드 아이템

```text
IMPORTING -> STAGED -> HASHING -> READY -> UPLOADING -> VERIFYING -> SYNCED
                                                    |           |
                                                    v           v
                                               RETRY_WAIT <----+
                                                    |
                                                    +-> READY

SYNCED -> PREVIEW_PENDING -> OPTIMIZED (SMART_PREVIEW)
      \-> ORIGINAL_RETAINED (KEEP_ORIGINAL)
```

- IMPORTING: 공유 시트 임시 URI/파일을 안전한 앱 스토리지로 복사; 완료 전에는 성공 표시하지 않음.
- STAGED: 고유 아이템 ID·폴더 ID·실제 파일 위치·크기·파일 수명 확인.
- HASHING: SHA-256. 대용량은 작업 취소/진행률 제공.
- READY: 서버 연결 가능하면 사전 조회(optional), 없으면 스테이징 보존.
- UPLOADING: 요청 ID/Idempotency-Key 유지, 재시도는 가변 슬롯이 아니라 동일 논리 요청을 재생.
- VERIFYING: 서버 응답이 요청 폴더·정확한 해시·자산 ID와 일치하는지 확인.
- SYNCED: 원본이 서버에 저장됐다는 사실. **서버 백업 2차본 존재 여부는 별도 표시**.
- PREVIEW_PENDING: 네트워크 장애 시 **원본 스테이징을 유지**. 프리뷰 수신 또는 로컬 생성 검증 후에만 정리.
- OPTIMIZED: 로컬 원본 스테이징을 삭제, 축소본/메타데이터 유지.
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
- `asset_removed`에 의해 로컬 폴더 연결을 해제하지만, 동일 자산이 다른 폴더에 있으면 공용 캐시는 유지.

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
