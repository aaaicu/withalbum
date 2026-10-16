# 위드앨범(WithAlbum) 제품·아키텍처 정의 v1.3

- 기준 날짜: 2026-10-17
- 상태: **제품 방향 확정 및 Google Drive 확장 설계 제안**. 이 문서는 구현 완료를 뜻하지 않는다.
- 제품명: **위드앨범(WithAlbum)**. 기존 PhotoHub는 임시 작업명이다.
- 상속: `PRODUCT_ARCHITECTURE_v1.2.md`의 Share Sheet 우선 UX·스마트 저장·정방향 및 역방향 동기화를 유지한다.

## 핵심 제품 정의

**위드앨범은 iOS와 Android에서 사진·영상을 공유하고, 원본은 사용자가 선택한 저장 방식에 보존하는 앨범 앱이다.**

- `SELF_HOSTED` : Mac mini/외장 SSD의 WithAlbum Server가 우선. Linux·Raspberry Pi·NAS로 확장. Go 단일 코어와 SQLite, 자체 권한과 동기화 API.
- `GOOGLE_DRIVE` : 서버 설치가 불가능한 사용자가 Google 계정을 연결하여 Google Drive에 보존. iOS/Android 클라이언트가 Drive API에 직접 연결. 원본 관리·공유 권한은 Google의 정책과 사용자 저장 용량에 종속.
- 동일한 앱 안에서 저장소를 선택할 수 있는 UX를 지향하되, 초기는 **앨범 단위로 백엔드 고정**. 서로 다른 백엔드 간 자동 복제/공유는 후속 기능.
- 서버나 Drive 어느 쪽도 기본 Photos/Gallery 앱을 몰래 감시하거나 그 앱의 원본을 자동 삭제하지 않는다.

## 변함없는 공통 핵심 UX

1. 기본 사진/갤러리에서 사진 선택 → 공유 → 위드앨범.
2. 연결된 저장소와 대상 앨범을 선택한다.
3. 안전한 전송 대기열에 등록하고 수동 동기화를 제공한다. 자동 전송은 OS 정책 내 최선 노력.
4. 원본 업로드와 원격 파일 검증을 확인한다. 자동으로 정리하는 것은 **위드앨범 앱 내부 스테이징 사본뿐**이다.
5. `SMART_PREVIEW`가 기본. 앱에는 경량 미리보기, 사용자가 선택하면 원본 보관.
6. 다른 사용자가 올린 사진은 앱 실행/동기화 때 증분 동기화. 필요한 원본은 요청 시 다운로드.
7. 기본 사진 앱에 남은 원본은 사용자가 직접 정리한다.

## 기능별 책임

| 관심사 | 개인 서버 | Google Drive |
|---|---|---|
| 원본 바이트 보존 | 보호된 CAS pool + 서버 무결성 검증 | Drive 파일 + 클라이언트 업로드 결과 확인/재검증 |
| 앨범 목록 및 멤버 | SQLite + 자체 권한 | Drive 폴더와 Google 권한 |
| 업로드 재개 | 서버 전송 세션 구현 | Drive resumable upload API |
| 원본 중복 검사 | 전역 해시 기반 물리 중복 제거 | 클라이언트에서 동일 앨범 내 사전 조회/정리. 원자적 전역 보장 없음 |
| 미리보기 | 서버 변환/생성 | 주로 클라이언트 생성, 필요 시 Drive 썸네일 활용(제약) |
| 변경 이력 | 자체 폴더별 이벤트 커서 | Drive changes.list + 기기별 커서/목록 보정 |
| 실시간 변경 힌트 | 서버 WebSocket/SSE, 푸시(향후) | 단순 모드는 앱 실행/주기 동기화. 웹훅은 별도 HTTPS 운영 없이는 적용하지 않음 |
| 접근 제어 | OWNER/EDITOR/VIEWER 등 자체 모델 | Drive 폴더 공유 권한 기반, 세밀한 정책은 제한 |
| 댓글/좋아요 | 자체 서버 DB로 개발 가능 | 초기 단계 미지원. 동시 쓰기 중재용 별도 백엔드 필요할 수 있음 |
| 백업/복구 | 사용자의 독립 백업 장치 필요 | Drive 보관은 별도의 독립 백업과 동의어가 아님 |

## 공통 모바일 내부 데이터 모델 (제안)

```text
StorageConnection: id, provider_type, name, account_ref, capability_flags
Album: album_id, connection_id, provider_folder_id, display_name, role
Asset: asset_id, album_id, provider_file_id, original_sha256, mime_type, size, status
UploadJob: job_id, album_id, local_staged_path, resume_token, retry_state
SyncCursor: connection_id, account_ref, cursor_or_token, last_reconcile_at
```

백엔드간 `(asset_id, album_id)` 등 논리 키를 별도로 유지해야 Google Drive 파일 ID와 서버 자산 ID를 혼동하지 않는다.

## 사용자 고지 원칙

저장소 선택 시 가능/제한/미지원 기능을 비교해 설명한다. 동기화가 늦거나 실패하더라도 **업로드 완료처럼 표시하지 않는다.** 앱 내부 원본의 자동 삭제는 실제 저장 성공 확인 후에만 수행한다.

Google Drive 폴더의 편집자는 Google Drive에서도 직접 파일을 수정·삭제할 수 있어 원본 불변성을 자체 서버만큼 강하게 보장할 수 없다. Google Drive 용량·API 할당량·권한 정책이 적용된다.

## 개발 단계

1. **P0 / Mac-first:** 서버 파일 내구성·SQLite·인증/권한·복구, macOS 실제 설치/운영을 먼저 완성.
2. **P1 / 모바일 코어:** 공유 시트, 안전한 대기열, 업로드, 스마트 미리보기, 다중 기기 앨범.
3. **P2 / 공통 저장 인터페이스:** 로컬 서버와 Google Drive 모드 간에 UI·도메인·전송 상태를 재사용.
4. **P3 / Google Drive PoC:** OAuth `drive.file`·공유 폴더·다중 사용자 수정 권한·변경 감지·복구 검증.
5. **P4 / Drive 베타:** Google OAuth 검증·사용자 고지·용량 경고·폴더 제한·마이그레이션 시험.
6. **P5 / 고도화:** 서버 측 댓글/반응, Drive 협업 기능의 대안 검토, 저장소 간 복사/이관.

**현재 완료:** Mac 로컬 Go 초기 프로토타입/릴리스 자동화 골격. **미완료:** 프로덕션 서버, 정식 모바일 앱, Google Drive 실제 통합.

세부 검증 항목은 [GOOGLE_DRIVE_MODE_v1.3.md](GOOGLE_DRIVE_MODE_v1.3.md), 공통 계층은 [STORAGE_ARCHITECTURE_v1.3.md](STORAGE_ARCHITECTURE_v1.3.md).
