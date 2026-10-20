# 혼돈 시험 결과 (macOS 디스크 이미지)

`scripts/chaos-macos.sh`(+ `tools/chaos/`)로 기계가 재현할 수 있는 손실 시나리오를 반복한 결과입니다. **실제 외장 디스크·실제 전원 차단 시험이 아닙니다.** 사람이 하는 실기기 점검은 [`MACMINI_E2E_CHECKLIST.md`](MACMINI_E2E_CHECKLIST.md)입니다.

```bash
bash scripts/chaos-macos.sh -n 100 -r /tmp/chaos.md      # 약 3분, 이미지·프로세스는 끝나면 정리
```

## 방법

- **디스크:** `hdiutil create -type SPARSE -fs APFS`(384 MiB) 이미지를 작업 디렉터리 안 마운트 지점에 `-nobrowse`로 붙이고, 그 안의 `WithAlbum` 디렉터리를 저장소로 씁니다. 실제 디스크는 건드리지 않으며, 종료 시(실패·^C 포함) 작업 디렉터리의 이미지를 모두 떼고 시험 서버를 모두 종료합니다.
- **서버:** 실제 `withalbum-server`를 `-tags chaos`로 빌드한 시험용 바이너리. 이 태그로 빌드했을 때만 `WITHALBUM_CHAOS_FAULT=<지점>`에서 업로드가 멈추고(`server/chaos_on.go`), 릴리스 빌드에는 이 코드가 없습니다. 저장소는 설치 화면과 같은 설정 파일(루트+volume_id)로 찾으므로 디스크가 없으면 설치 모드가 아닌 기동 거부로 갑니다. `WITHALBUM_MIN_FREE_BYTES=0`.
- **업로드:** 자산마다 다른 무작위 바이트(4 KiB~6 MiB), 업로드마다 고유 `Idempotency-Key`. 일부는 32 KiB씩 천천히 보내 장애가 요청 본문 중간에 걸리게 합니다.
- **장애(라운드마다 하나, 섞어서 고르게):**

| 시나리오 | 내용 |
|---|---|
| `kill_mid_body` | 본문 전송 중 SIGKILL |
| `kill_at:after_stage` | 임시 파일 기록·fsync 직후 SIGKILL |
| `kill_at:after_promote` | 원본이 pool에 들어간 뒤, DB 커밋 전 SIGKILL |
| `kill_at:before_commit` | DB 트랜잭션 안, COMMIT 직전 SIGKILL |
| `kill_at:after_commit` | 커밋 후 응답 전 SIGKILL |
| `powercut_mid_body`, `powercut_after_commit` | 위처럼 SIGKILL 직후 동기화 없이 `hdiutil detach -force` 후 다시 붙임(전원 차단 근사) |
| `detach_same_dev`, `detach_new_dev` | 업로드 중 `detach -force`. 분리 중 health·업로드 거부 확인, 마운트 경로에 내장 디스크 디렉터리를 다시 만들어도 거기에 쓰지 않는지 확인, 다시 붙인 뒤(new_dev는 다른 이미지가 먼저 장치 번호를 차지해 st_dev가 바뀜) 재시작 없이도 계속 거부하는지 확인 |
| `disk_full` | 디스크를 채운 뒤 큰 업로드·작은 업로드·길이 없는(chunked) 업로드, 공간을 비운 뒤 정상 업로드 |
| `sigterm_mid_body` | 본문 전송 중 정상 종료(SIGTERM) |
| `once:copied_marker` | 라이브러리(마커+`.system`)를 다른 디스크 이미지로 복사 → 서버·`verify` 모두 디스크 UUID 불일치로 거부 |
| `once:start_without_disk` | 디스크를 뗀 채 기동, 마운트 경로에 빈 디렉터리가 있어도 기동 거부·아무것도 쓰지 않음 |

- **매 라운드 뒤 확인(하나라도 어긋나면 실패):** 서버 재시작 → `verify -hash -json` 정상(DB integrity·외래 키, 모든 원본 존재·크기·SHA-256, 해시한 수 = 원본 수, DB에 없는 pool 파일 0) → 폴더 목록이 보낸 것과 일치(**2xx 응답을 받은 업로드는 모두 있고**, **오류 상태로 거부된 업로드는 없고**, 모르는 원본·중복·크기 다른 자산 없음) → 최근 원본은 내려받아 SHA-256 비교 → 응답을 못 받은 업로드를 같은 키로 재시도해 모두 정확히 한 번 저장(폴더 자산 수 = 보낸 서로 다른 업로드 수).

## 결과 (2026-10-21, 최종 코드)

| 항목 | 값 |
|---|---|
| 환경 | Apple Silicon Mac (arm64), macOS 27.0.1, 내장 SSD 위의 APFS 스파스 이미지 384 MiB |
| 시드 | 1792521555554714000 |
| 라운드 | 100 (+ 1회성 시나리오 2) |
| 소요 | 2분 35초 |
| 보낸 서로 다른 업로드 | 356 (최종적으로 모두 2xx) |
| 응답 없이 끝나 재시도한 업로드 | 167 |
| 서버 재시작 | 148 |
| `verify -hash` | 100회 |
| 분리 라운드 / st_dev가 바뀐 재연결 | 19 / 12 |
| **실패(손실·불일치·잘못된 응답)** | **0** |

| 시나리오 | 라운드 |
|---|---|
| `detach_new_dev` | 9 |
| `detach_same_dev` | 10 |
| `disk_full` | 9 |
| `kill_at:after_commit` | 9 |
| `kill_at:after_promote` | 9 |
| `kill_at:after_stage` | 9 |
| `kill_at:before_commit` | 9 |
| `kill_mid_body` | 9 |
| `powercut_after_commit` | 9 |
| `powercut_mid_body` | 9 |
| `sigterm_mid_body` | 9 |
| `once:copied_marker` | 1 |
| `once:start_without_disk` | 1 |

| 관찰(실패 아님) | 횟수 |
|---|---|
| 응답 503 `storage_unavailable`(분리 중·재연결 후 재시작 전) | 57 |
| 응답 503 `storage_io_error`(분리 순간 진행 중이던 업로드) | 18 |
| 응답 507 `insufficient_storage`(가득 참) | 20 |
| 응답 없음(연결 끊김) | 72 |
| 응답 없이 끝났지만 재시작 후 저장돼 있음(커밋 후 종료: `after_commit` 계열) → 재시도는 같은 결과 재응답 | 18 |
| 응답 없이 끝났고 저장 안 됨 → 재시도로 저장 | 54 |
| 분리 직전에 끝나 2xx를 받은 업로드(재시작 후 있음) | 1 |

## 시험 중 발견해 고친 문제

1. **재연결 후 "정상"으로 돌아오는 저장소(수정).** 디스크를 강제로 뗐다가 **같은 장치 번호**로 다시 붙이면, 재시작하지 않은 서버의 health가 `storage: ok`로 돌아왔습니다. 마커·장치 번호 확인은 통과하지만, 서버가 열어 둔 SQLite 핸들과 인스턴스 잠금(flock)은 떼어진 마운트의 것이라 쓰기는 I/O 오류가 나고, 잠금이 풀린 상태라 두 번째 서버가 같은 저장소를 열 수도 있었습니다. 이제 `storage.Volume.Check`는 한 번 실패하면 그 프로세스에서는 계속 실패하고(재시작 필요, 문서화된 동작과 일치), 회귀 테스트 `TestCheckStaysFailedAfterVolumeReturns`·`TestStorageLostStaysUnavailable`을 추가했습니다.
2. **분리 중 인증 경로의 500(수정).** 디스크가 없을 때 세션 확인(DB 읽기)이 실패하면 `500 internal_error`였습니다. 저장소 확인도 실패하면 `503 storage_unavailable`로 답합니다(같은 테스트).
3. **디스크 UUID(새 기능 확인).** 복사한 마커로는 서버·`verify`가 `SECURITY: ... created on another disk`로 거부했습니다.

## 한계 — 이 시험이 증명하지 않는 것

- 실제 USB/Thunderbolt 외장 디스크, 케이블 분리, 디스크 잠자기, macOS의 "올바르게 꺼내지 않음" 경로.
- 진짜 전원 차단: `detach -force`는 커널 캐시를 비우고 떼므로 디스크 쓰기 캐시 소실을 재현하지 못합니다. 서버는 응답 전에 `F_FULLFSYNC`(SQLite `fullfsync`)를 쓰지만, 그 효과는 실제 하드웨어에서만 확인됩니다.
- exFAT·HFS+ 외장 디스크, 네트워크 파일 시스템, 큰 라이브러리(여기서는 수백 개 원본), 동시 다중 클라이언트, 재개 업로드 세션·라이브 포토 묶음 경로(단일 요청 업로드만 주입).
