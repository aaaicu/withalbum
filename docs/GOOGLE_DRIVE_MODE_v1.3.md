# 위드앨범 Google Drive 모드 v1.3 — 기술·제품 타당성

- 상태: **구현 전 설계/리스크 분석** (2026-10-17)
- 결론: **핵심 사진 공유 기능은 충분히 구현 가능**. 단, 로컬 서버의 완전한 대체품은 아니다.
- Google Drive API 정식 문서 근거는 하단 링크 참조.

## 제품 시나리오

1. 사용자가 `Google Drive로 시작`을 선택하고 Google OAuth로 로그인한다.
2. 위드앨범이 앱 관리용 폴더와 앨범 폴더를 만든다. `drive.file` 최소 권한부터 시작한다.
3. 앨범 소유자가 Google 계정 이메일로 참여자를 초대한다. **개인 계정의 일반 공유 폴더**를 사용한다. Workspace의 `Shared drive`와 혼동하면 안 된다.
4. 참여자가 자기 Google 계정으로 로그인하여 해당 폴더 및 위드앨범에서 만든 파일에 접근하는지 검증한다. `drive.file` 접근 범위 때문에 **공유된 하위 파일 자동 접근 여부는 여러 계정 실험이 필요**하다.
5. 사진 앱에서 공유 → 위드앨범 앨범 선택. 로컬 대기열에서 원본의 SHA-256/메타데이터와 작은 프리뷰를 계산.
6. Google Drive `files.create`의 resumable upload를 사용하여 원본을 올리고 file ID/크기·해시/상태를 기록한다. Drive 반환 `md5Checksum`은 일부 파일 형식에서만 제공되므로 **SHA-256을 직접 유지**한다.
7. Drive 목록과 변경 이력을 조회해 다른 사람이 추가한 파일을 위드앨범 화면에 반영한다. 미리보기는 기기에서 생성/캐시하고 부족하면 지연 로딩.
8. 원본이 필요한 순간 OAuth 인증으로 Drive에서 다운로드한다. 원본의 내보내기와 공유 권한을 확인한다.
9. 업로드 이후 자동 정리는 **위드앨범 스테이징 복사본만**. 기본 Photos/Gallery 원본은 별도로 사용자가 관리.

## 6가지 중요한 제약

### 1. 사용량과 파일 소유권

- 개인 Google 계정은 일반적으로 최대 15GB 기본 저장 공간을 Gmail·Drive·Photos 등과 공유한다.
- 가족이 하나의 폴더를 공유하더라도, 일반 My Drive에서 파일 소유권과 저장소 사용량은 **파일 소유자**에 연결된다. '폴더 만든 사람의 15GB를 모두 같이 쓰는 것'으로 오해시키지 않는다.
- Workspace용 `Shared drives`는 조직 공유 저장소 모델이 별도로 존재하지만 개인 Gmail 계정이 일반적으로 만들 수 있는 것이 아니다.

### 2. 원본 완전 보호 / 중복 제거

- Drive는 데이터베이스 트랜잭션이나 CAS 서버가 아니다. 폴더 안에 같은 이름/해시의 동시 업로드가 가능해 서버의 원자적 중복 제거와 동급으로 약속하면 안 된다.
- 중복 탐지는 파일 메타데이터/사전 검색/클라이언트 SQLite/후속 조정으로 **최선 노력**. 중복 파일이 일시적으로 생성될 수 있다.
- 편집 권한자가 Drive 웹에서 파일을 변경/삭제할 수 있으므로 자체 서버처럼 원본 불변을 강제할 수 없다. 데이터 복구·권한 모델은 별도 고지한다.

### 3. 실시간 알림

- `changes.list`로 증분 조회는 가능하다. `changes.watch`는 리소스 변경 힌트만 전달한다.
- Push watch 콜백에는 공개 HTTPS 웹훅 엔드포인트가 필요하다. 사용자에게 서버 설치를 요구하지 않는 초기 Drive 모드에서는 **앱 실행, 새로고침, OS가 허용하는 백그라운드 조회**에 집중한다.
- 지연 없이 항상 모든 기기에 알림이 온다고 약속하지 않는다.

### 4. Google OAuth 권한/승인

- 권장 시작 권한은 `drive.file`(비민감 범위). Google Picker/앱이 만든 파일 중심으로 접근 범위를 좁힌다.
- 개인 계정의 기존 Drive 전체를 자동 스캔하려고 광범위한 `drive` 권한을 얻는 설계는 초기에 피한다. `drive`는 제한 범위이며 엄격한 심사가 필요하다.
- iOS/Android OAuth 공개 클라이언트 사용(비밀키 앱 내 포함 금지), 토큰은 OS 안전 저장소. OAuth 동의 화면, 앱 개인정보처리방침, 공개 앱 검증 절차가 필요하다.
- Android·iOS에서 Picker의 실제 접근 권한과 다중 사용자 접근 범위는 PoC로 검증할 것.

### 5. 용량 최적화 / 변환 기능

- 서버가 없으므로 HEIC·RAW·Live Photo·영상 프리뷰는 모바일 기기에서 가능한 범위 내에 생성. 앱 간 코덱 차이는 별도 시험.
- Drive의 `thumbnailLink`는 짧은 유효기간과 인증·CORS 제약이 있어 독립적인 영구 CDN처럼 사용하지 않는다.
- 고품질 영상 트랜스코딩, 전체 라이브러리 인덱싱은 서버 모드보다 제한될 수 있다.

### 6. Google Drive API 할당량과 비용

- 공개 API에는 사용자/프로젝트별 요청 제한, 대역폭 정책, `403`/`429` 오류가 있다. 지수 백오프 및 최종 일관성을 전제로 한다.
- Google은 2026-05-01부터 신규 프로젝트에 업데이트된 할당량/전송량 정책을 적용했다. 앱 규모가 늘면 단순히 'Drive만 쓰니 운영비 0원'이라고 볼 수 없다.
- Drive에 접근할 수 없거나 저장소가 가득 차면, 성공 상태를 표시하거나 대기 중 원본을 삭제하면 안 된다.

## 기능 표시 제안

| 기능 | Drive 모드 목표 | 구현 등급 |
|---|---|---|
| 개인 사진·동영상 저장 | 원본 업로드/다운로드 | 필수 |
| 기기 저장 최적화 | 원본 전송 확인 후 앱 내부 경량화 | 필수 |
| 멤버 초대 | Google Drive 이메일 기반 폴더 공유 | PoC 필수 |
| 역방향 동기화 | 앱 실행/새로고침 때 증분 반영 | 필수 |
| 원본 중복 제거 | 동일 앨범 중복 조회, 사후 조정 | 제한적 |
| 실시간 푸시/즉시 반영 | 웹훅 중계 인프라 없이 보장 불가 | 초기 미지원 |
| 앱 고유의 세밀한 권한/댓글/좋아요 | 일관된 동시 편집 서버가 필요할 수 있음 | 초기 미지원 |
| Google Drive에서 직접 추가한 임의 파일 감지 | `drive.file` 범위·Picker 접근과 충돌 가능 | 별도 검증 필요 |
| 외장 디스크 자동 해싱·미리보기 변환 | 전용 서버가 없음 | 미지원 |

## 그대로 활용할 수 있는 사용자 안내 문구

**저장 장소 선택 화면**
> Google Drive를 사용하면 별도 서버 없이 위드앨범을 시작할 수 있습니다. 사진 원본은 연결한 Google 계정의 Drive에 저장되며, Google의 저장 용량·공유 권한·API 제한을 따릅니다.

**Drive 모드 제한 안내**
> Google Drive 모드에서는 실시간 동기화, 고급 중복 제거, 서버 기반 미디어 변환, 세밀한 앨범 권한 및 댓글·반응 기능이 제공되지 않거나 제한될 수 있습니다. 새로운 사진은 앱 실행 또는 새로고침 시 반영됩니다.

**정리 전 경고**
> 원본이 Google Drive에 저장되었는지 확인한 후에만 위드앨범 앱 내부 임시 파일을 정리합니다. 기본 사진 앱에 있는 원본은 자동으로 삭제되지 않습니다. Google Drive 저장본 하나만으로 별도의 백업이 확보되는 것은 아닙니다.

## 출시 전 반드시 통과할 PoC

- A(개인 Gmail)가 생성한 앨범에 B(다른 개인 Gmail)가 editor로 원본 업로드 -> A, B 모두 `drive.file`로 열람/다운로드 가능?
- B 업로드 원본의 실제 파일 소유자와 용량 차감은 누구인가? Drive 웹 및 API로 확인.
- A/B 각각의 `changes.list` 토큰으로 새 파일·삭제·권한 회수 감지 가능? 권한 접근 실패 시 로컬 상태는?
- iOS/Android Google OAuth 토큰 재인증, 오프라인 전송, 대용량(예 4GB) 재개 정상 동작?
- 동일 사진을 A/B가 동시에 업로드할 때 중복 파일이 생기면 탐지/정리 가능한가?
- Drive 파일명 변경, 휴지통, 폴더 이동, 공유 권한 취소 시 안전한 동기화가 되는가?
- Drive 쿼터/용량 부족/일시 403/429 이후 업로드 작업이 재개되는가?
- 앱 업데이트/재설치/기기 변경 뒤 원격 목록으로 로컬 DB를 복구할 수 있는가?

## 공식 참고 자료 (2026-10-17 확인)

- Google Drive API OAuth scopes: https://developers.google.com/workspace/drive/api/guides/api-specific-auth
- Drive Picker/mobile: https://developers.google.com/workspace/drive/picker/guides/desktop-mobile-picker
- Resumable uploads: https://developers.google.com/workspace/drive/api/guides/manage-uploads
- Changes feed: https://developers.google.com/workspace/drive/api/guides/manage-changes
- Webhook push prerequisites: https://developers.google.com/workspace/drive/api/guides/push
- API limits (2026 rules): https://developers.google.com/workspace/drive/api/guides/limits
- Files metadata / thumbnail: https://developers.google.com/workspace/drive/api/reference/rest/v3/files
- Google storage allocation: https://support.google.com/accounts/answer/9312312
- Shared drive account constraints: https://developers.google.com/workspace/drive/api/guides/manage-shareddrives


## v1.4 권한 모델 갱신 (2026-10-17)

자체 서버에서는 OWNER/CONTRIBUTOR/VIEWER/UPLOADER와 서버 관리자/앨범 생성 허가를 구분한다. 하지만 개인 Google Drive 폴더의 `writer`는 읽기·수정·삭제도 할 수 있으므로 **진짜 UPLOADER(읽기 불가) 역할을 네이티브 Drive에서 지원한다고 말할 수 없다**. 초기 Google Drive 모드에서는 UPLOADER 선택지를 감추고 VIEWER/EDITOR 성격의 역할만 제공한다.

자세한 차이는 [ACCESS_CONTROL_v1.4.md](ACCESS_CONTROL_v1.4.md)의 Google Drive 절 참조.
