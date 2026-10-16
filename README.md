# 위드앨범 · WithAlbum

> **내 사진과 앨범을, 내가 선택한 저장소에.**  
> *Self-hosted photo sharing for Mac mini first, Linux/Raspberry Pi later; optional serverless Google Drive mode.*

<!-- WITHALBUM 문서 주석 사용법:
이 README는 사람에게 보이는 설명과, Claude 같은 개발 에이전트를 위한 HTML 주석을 함께 포함한다.
주석도 설계의 일부다. 주석의 '목표/검증/금지'는 개발 요구사항이지 이미 구현된 코드라는 뜻이 아니다.
대화에서 확정된 내용은 v1.4 설계 + 본 문서의 정리본으로 확인한다.
결정의 유래는 docs/DECISIONS_v1.5.md에 정리되어 있다.
읽기 순서: README.md -> CLAUDE.md -> docs/PRODUCT_ARCHITECTURE_v1.4.md -> docs/ACCESS_CONTROL_v1.4.md -> 기존 코드와 테스트.
-->

## ⛔ 현재 상태를 먼저 확인하세요

**이 저장소는 완성 앱이 아니라 개발용 서버 프로토타입 + 전체 제품 설계 + Claude 개발 인수인계 패키지입니다.**

- **기능이 동작하는 코드:** Go 기반 localhost 전용 서버, 간단한 웹 페이지, 폴더 생성, 파일 업로드, SHA-256 중복 확인, 제한적인 이미지 프리뷰, 변경 이벤트 조회.
- **코드·테스트만 있는 기능:** `internal/authz/`의 서버/앨범 권한 판정 함수와 단위 테스트. **실제 HTTP 요청에는 적용되지 않음.**
- **설계만 완료:** SQLite v1.4, 관리자 생성·로그인·세션·초대·폴더 ACL, Mac 설치 마법사, iOS/Android 앱, 역방향 모바일 동기화, Google Drive 어댑터, 장애 복구.
- **아직 없음:** 서명·공증된 DMG, 정식 Google Drive OAuth 연결, 모바일 앱, 프로덕션 안정성 보장.
- **보안 경고:** HTTP 서버는 인증이 없으므로 의도적으로 `127.0.0.1`에만 바인딩됩니다. **LAN/인터넷에 공개하거나 대체할 수 없는 원본 사진의 단일 보관소로 사용하지 마세요.**

<!-- 구현 범위 확인:
server/main.go: JSON state.json을 읽고 쓰며 SQLite를 사용하지 않는다. 현재 전체 파일 한 번 업로드, MAX 512MiB.
server/main.go: 자산 ID는 실제 SHA-256이며 목표 스키마의 랜덤 논리 ID와 다르다. 업로더의 identity도 신뢰할 수 있는 인증 주체에서 오지 않는다.
server/main.go: 현재 예시 원본 다운로드 API는 folder_id/ACL 확인이 없어, 인증을 먼저 통합하지 않고 네트워크를 열면 심각한 정보 유출이 생긴다.
SQLite 스키마와 YAML API 문서는 목표 규격으로만 분류할 것. 결과/HTTP 응답이 현재 구현과 다를 수 있다.
이 패키지는 'v1.5 documentation/handoff'이고 서버 실행파일 기능이 v1.5로 향상되었다는 뜻이 아님.
-->

## 📦 다운로드 · GitHub Releases · 릴리스 노트

> **현재 이 패키지의 ZIP이 개발 인수인계용 원본입니다.** 아직 GitHub 저장소와 정식 Release URL은 정해지지 않았습니다.

| 항목 | 안내 | 현재 상태 |
|---|---|---|
| [GitHub 최신 다운로드](../../releases/latest) | 저장소에 올린 뒤 태그 릴리스 발행 시 활성화되는 상대 링크 | **미게시** |
| [GitHub 모든 버전·릴리스 노트](../../releases) | 버전별 변경 내용과 설치 파일 | **미게시** |
| [Mac mini 설치](docs/INSTALL_MACOS.md) | Apple Silicon(`darwin_arm64`) 최우선; Intel도 목표 | 개발자용 CLI/LaunchAgent |
| [Linux·Raspberry Pi 설치](docs/INSTALL_LINUX.md) | Linux AMD64/ARM64/ARMv7, systemd | 개발자용 |
| [배포 파이프라인](docs/RELEASING.md) | GitHub Actions · GoReleaser · checksum | 골격 구성 |
| [릴리스 노트 예시](docs/releases/EXAMPLE_RELEASE_NOTES.md) | 알파 릴리스 설명 문구 | 예시 문서 |

<!-- 릴리스 요구사항:
GitHub Releases는 git의 'release/' 폴더가 아니라 GitHub Releases 화면이다.
파일 명명, checksums.txt, 릴리스 노트, prerelease 표시, 설치 방법, 지원 아키텍처를 매번 검증한다.
고객용 Mac DMG/PKG는 아직 존재하지 않으므로 README에서 다운로드 가능하다고 적지 않는다.
실제 리포지터리가 만들어지면 GitHub URL, go.mod placeholder, workflow 동작, README 링크를 갱신한다.
미서명 바이너리와 실제 Mac 실기기 테스트 여부를 명시한다.
-->

### 프로젝트 최상위 실행 명령

Go 1.23+에서 테스트용 파일로만 확인하세요.

```bash
go test ./...
go vet ./...
go build -o withalbum-server ./server
WITHALBUM_ROOT="$HOME/WithAlbumTest" ./withalbum-server
# 같은 컴퓨터 브라우저: http://127.0.0.1:8787
```

명령 실행 전 `mkdir -p "$HOME/WithAlbumTest"`를 권장합니다. 이 서버는 해당 경로 아래 실험 데이터를 생성합니다. 프로덕션 외장하드 폴더를 지정하지 마세요.

<!-- 운영 시 제약:
WITHALBUM_LISTEN=127.0.0.1:8787 기본. 주의: 바인딩 가드는 운영 인증을 대신하지 않는다.
Go 모듈은 example.com/withalbum 임시 경로. 새 GitHub 조직/저장소 위치를 알고 난 다음 변경.
멀티 플랫폼 컴파일은 Mac/Pi 런타임과 성능 테스트를 증명하지 않는다.
-->

---

## 1. 최종 제품 정의: 서버와 모바일 앱

**위드앨범(WithAlbum)**은 아이폰·갤럭시에서 사진/동영상을 공유하고, 원본은 사용자가 선택한 저장소에 보관하는 **공유 앨범 서비스**입니다. 서버를 직접 운영하는 사람은 Mac mini와 외장 스토리지를 우선 사용할 수 있고, 이후 Linux·Raspberry Pi·NAS에서도 같은 Go 코어를 돌릴 수 있어야 합니다. 서버가 없는 사용자는 **Google Drive 모드(추후 개발)**를 선택합니다.

- **브랜드:** 위드앨범 / WithAlbum. 과거 `PhotoHub` 표기는 임시 이름으로, 과거 문서 내에서는 역사적 표현입니다.
- **1순위:** 상시 가동하는 Mac mini + Thunderbolt 외장 SSD/HDD. 원본 파일 보호·관리·안전한 복구를 최우선시합니다.
- **2순위:** 동일한 서버 코어의 Linux x86-64·ARM64·ARMv7 / Raspberry Pi / NAS 호환.
- **별도 제품 경로:** Google 계정 기반 서버 없는 저장 모드. **Go 서버의 설치 옵션이 아니라 모바일의 독립 Storage Adapter**입니다.
- **클라이언트:** iOS / Android 모바일 앱, 서버 관리자 웹 UI; 모바일은 Flutter + OS별 네이티브 Share Extension/Intent 권장 설계.
- **초기 초점:** 핵심 업로드·공유·원본 저장·용량 최적화·권한. 댓글/좋아요, 유료 릴레이, 클라우드 복제 등은 후순위.

<!-- 제품 철학 / Claude:
사용자가 단순히 폴더 링크를 아는 것으로 원본에 접근하는 시스템을 만들지 않는다.
서버, 사용자, 앨범, 자산, blob, 기기, 동기화 상태를 별도 도메인으로 분리한다.
self-hosted 모드와 Drive 모드가 데이터 저장·ACL·실시간성에서 완전히 동일하다고 과장하지 않는다.
최우선 Mac-first, cross-platform; 특정 macOS 미디어 가속 코드는 선택적 어댑터로만 허용한다.
-->

## 2. 확정된 핵심 UX — 기본 사진 앱을 '감시'하지 않는다

**송신/업로드(Outbound)**

1. 사용자가 기본 iOS 사진/Android 갤러리에서 사진 또는 영상을 선택합니다.
2. 시스템 **공유 → 위드앨범**을 누릅니다.
3. 공유 확장 UI에서 저장소 연결과 참여 중인 앨범을 선택합니다. 쓰기 권한이 있는 앨범만 업로드 대상으로 표시합니다.
4. 위드앨범 앱 내부 안전한 스테이징 큐에 임시 원본·메타데이터를 저장합니다. 필요한 경우 일시적인 저장공간 복제가 발생합니다.
5. **지금 동기화** 버튼으로 즉시 업로드합니다. 자동 재시도/백그라운드 작업은 OS가 허용하는 범위에서 최선 노력으로 실행됩니다.
6. 서버 또는 Drive에 원본이 안전하게 저장되었고 동일 파일임을 검증합니다. 업로드 응답만 믿지 말고 원본 파일 크기·해시·영수증을 확인합니다.
7. **기본 설정 SMART_PREVIEW:** 경량 미리보기 확보 후에만 **위드앨범 앱 내부 스테이징 원본 복사본**을 제거합니다. `KEEP_ORIGINAL` 옵션을 지원합니다.
8. 기본 사진 앱에 있던 원본은 **절대로 몰래 삭제하지 않습니다.** 사용자가 직접 정리할 수 있고, 추후 보조 UI를 만들더라도 OS 권한과 명시적 사용자 승인을 받습니다.

**수신/다운로드(Inbound)**

1. 다른 사용자가 같은 앨범에 업로드하거나, 서버 관리자가 허용된 가져오기 영역에 직접 파일을 추가합니다.
2. 서버는 실제 원본 저장·해싱·폴더 연결 후 **앨범별 변경 이벤트와 증가 커서**를 기록합니다.
3. 모바일은 앱 실행/새로고침/가능한 푸시 힌트 후 `cursor` 기반 증분 변경 목록을 조회합니다.
4. **로컬 DB에 변경을 영속 기록한 뒤에만 커서를 전진**시킵니다. 원본 대신 메타데이터와 프리뷰를 우선 보여줍니다.
5. 원본이 필요한 순간에만 권한을 다시 검증하고 다운로드합니다. 같은 기기에서 업로드한 항목도 메타데이터는 동기화해야 합니다.

<!-- UX의 명시적 포기/금지:
초기 논의의 iCloud 브리지·기본 사진 앨범 실시간 감시·PhotoKit '다이어트 치환' 계획은 폐기됐다.
타사 앱이 시스템 사진 앱의 썸네일 껍데기를 마음대로 교체하거나 확대 이벤트를 관찰하는 것을 요구하지 않는다.
푸시/APNs/FCM는 '변경되었다'는 힌트일 뿐, iOS에서 즉시 백그라운드 실행을 보장하지 않는다.
Share Extension 인입 중 임시 URI 소실/확장 종료/앱 중지/네트워크 오류를 고려한 영속 대기열 필수.
애초에 업로드한 device_id가 같다고 server events 자체를 버리지 않는다.
보존된 기본 갤러리 원본과 앱 내부 사본을 구분하는 것이 기기 저장 공간 설명의 핵심이다.
-->

### 동기화 상태 머신 (목표 규격)

```text
OUTBOUND:
IMPORTING → STAGED → HASHING → READY → UPLOADING → VERIFYING → SYNCED
                                       ↘ RETRY_WAIT ↗              ↓
                                              PREVIEW_PENDING → OPTIMIZED
                                                  └────────→ ORIGINAL_RETAINED (옵션)

INBOUND:
METADATA_ONLY → PREVIEW_QUEUED → PREVIEW_READY → ORIGINAL_TEMPORARY
                     ↘ RETRY_WAIT  /  REMOVED_FROM_FOLDER(tombstone)
```

정확한 상태 전이, 실패 테스트, 메타데이터 우선 수신 동기화는 [CLIENT_SYNC_STATES.md](docs/CLIENT_SYNC_STATES.md)에 있습니다.

<!-- 동기화 구현 기준:
앱의 클라이언트 인덱스 PK 예시: (server_id, folder_id, asset_id), 원본 중복 여부와 폴더 소속은 분리.
Idempotency-Key는 업로드 재시도마다 같아야 한다. 타임아웃 이후 이미 서버가 원본을 저장했을 수 있다.
서버에서 실제 commit 완료 전에는 성공 응답 금지. 디스크 분리, quota 부족, 미리보기 실패 구분.
사용자 기기에 압축본을 받았더라도 원본 파일의 안전한 서버 백업이 끝났다는 의미는 아니다.
-->

## 3. 두 가지 저장 모드 (자체 서버 우선)

```mermaid
flowchart TD
    Share[아이폰 사진 앱 / 갤럭시 갤러리의 공유 메뉴] --> App[위드앨범 iOS / Android 앱]
    App --> Storage[AlbumStorageAdapter · 모바일 공통 도메인]
    Storage --> Hosted[SelfHostedAdapter: 로그인 + REST + 앨범 cursor]
    Storage --> Drive[GoogleDriveAdapter: OAuth + Drive API]
    Hosted --> Go[WithAlbum Go Server + 웹 관리]
    Go --> DB[(SQLite: 목표 구현)]
    Go --> CAS[(외장 SSD/HDD · 보호된 원본)]
    Drive --> GD[(사용자의 Google Drive)]
```

| 항목 | SELF_HOSTED | GOOGLE_DRIVE |
|---|---|---|
| 추가 장비/서버 | 필요 | 불필요 |
| 원본 보관 | 사용자 Mac/Linux/Pi/NAS 저장소 | 사용자 Drive 용량 |
| 원본 물리 중복 제거 | 서버 CAS + SHA-256 목표 | Drive 기본 ACL/저장 모델 때문에 최선 노력 |
| 인증 | 자체 서버 계정·기기별 세션 | Google OAuth |
| 서버 관리자/앨범 생성 승인 | 구현 목표 | 중앙 서버 관리자 없음 |
| 기본 읽기·편집 공유 | 자체 세밀 ACL 목표 | Google 공유 권한에 종속 |
| **쓰기 전용(읽기 불가)** | 필수 목표 | **초기 미지원** (Drive writer는 읽기도 가능) |
| 원본·미리보기 서버 변환 | 가능하도록 개발 | 별도 서버 없으므로 기기 중심 |
| 변경 이벤트/실시간 | 자체 커서와 변경 힌트 설계 | 앱 실행/새로고침 중심, 즉시성 제한 |
| 저장공간 비용/운영 | 디스크·전력·백업 부담 | Drive 계정 저장 용량·정책·API 할당량 |
| 현재 구현 | Go **로컬 PoC만** | **전혀 구현되지 않음** |

<!-- Google Drive 검증 필수:
개인 My Drive 공유 폴더 != Workspace Shared Drive. Drive에서 실제 파일 소유권/요금 차감 주체는 검증해야 한다.
Drive의 writer 권한을 SelfHosted UPLOADER(읽기 불가)로 표시하거나 비슷하다고 약속하지 말 것.
`drive.file` 최소 권한으로 시작하되 다중 Google 계정/공유 자산 접근은 실험 없이 성공 가정하지 말 것.
변경 추적 changes.list, watch HTTPS 콜백 조건, 오프라인 시 동기화 지연 및 OAuth 검증 범위를 확인.
앱에서 단순히 UI를 숨기는 것으로 Google Drive 원본을 읽지 못하게 만들 수 없다.
위드앨범 앱 내부 임시 원본은 Drive의 업로드/검증까지 완료하고 나서만 정리한다.
-->

Google Drive 전체 제약과 공개 사용자 안내 문구는 [GOOGLE_DRIVE_MODE_v1.3.md](docs/GOOGLE_DRIVE_MODE_v1.3.md), 공통 인터페이스 설계는 [STORAGE_ARCHITECTURE_v1.3.md](docs/STORAGE_ARCHITECTURE_v1.3.md)를 확인하세요.

## 4. 서버 계정 · 앨범 오너 · 사용자 초대 · 읽기/쓰기 권한

### 서버 범위의 규칙

- 최초 설치 시 로컬 설정 과정에서 **첫 번째 `SERVER_ADMIN`**을 만듭니다. 원격 서버 주소를 알기만 하면 첫 관리자로 가입할 수 있어서는 안 됩니다.
- 서버 관리자는 사용자를 등록/승인/비활성화하고 새 앨범 생성 권한을 별도로 부여합니다.
- 일반 `USER`의 `can_create_albums` 기본값은 **false**입니다. 서버 로그인이 된다고 앨범 오너가 될 수 있는 것은 아닙니다.
- 관리자는 신규 앨범을 만들 수 있지만, 남의 비공개 앨범을 앱 API로 자동 열람하지 않습니다. 단 서버 장치의 실제 파일 접근 권한까지 막힌다는 뜻은 아닙니다(E2EE 별도 문제).

### 앨범 범위의 규칙

앨범마다 **단 한 명의 `OWNER`**가 존재합니다. 생성한 사람이 오너이고, 등록된 계정에 역할을 지정해 초대합니다. 상대가 수락해야 실제 멤버가 됩니다.

| 동작 | OWNER | CONTRIBUTOR(읽기+쓰기) | VIEWER(읽기만) | UPLOADER(쓰기만) |
|---|:---:|:---:|:---:|:---:|
| 앨범 이름·내 역할 최소 조회 | ✅ | ✅ | ✅ | ✅ |
| 사진 목록·증분 변경 목록 | ✅ | ✅ | ✅ | ❌ |
| 미리보기·원본 다운로드 | ✅ | ✅ | ✅ | ❌ |
| 원본 업로드 | ✅ | ✅ | ❌ | ✅ |
| 타인의 사진 삭제/앨범 설정 | ✅ | ❌ | ❌ | ❌ |
| 멤버 초대·권한 변경·추방 | ✅ | ❌ | ❌ | ❌ |
| 소유권 이전·앨범 삭제 | ✅ | ❌ | ❌ | ❌ |

**UPLOADER는 제출함 용도:** 서버에 원본을 올릴 수 있지만, 사진 목록·다른 사람의 파일 존재 여부·원본/프리뷰는 볼 수 없습니다. 원본이 이미 CAS에 있는지를 업로드 응답으로 유추하게 해서도 안 됩니다.

<!-- 인증/보안 불변식:
* 인증됨 != 앨범 소속됨 != 앨범 생성 허가됨.
* API의 모든 리소스 검증 시 folder_id + user_id + 역할 + 실제 asset-folder 연결을 재조회 (deny-by-default).
* owner_user_id는 folders의 단일 출처. 초대/소유권 이전 트랜잭션과 감사 로그 필수.
* 회원 탈퇴/정지 전에 가진 앨범 소유권 이전 필요. 서버 관리자도 멤버 아닌 앨범 콘텐츠 열람 기본 거부.
* 초대는 기존 서버 사용자에게, 만료/일회성/대상 계정 고정. 초대 수락 전 접근 불가.
* 업로드 진행 중 권한이 철회될 수 있으니 '파일 커밋 직전' 다시 권한 검사.
* 공개 해시 조회/asset id를 통해 UPLOADER가 다른 사용자 파일 존재 여부를 알 수 없도록 랜덤 논리 ID 사용.
* Argon2id 비밀번호 해시, 단기 access token, 기기별 rotate/revoke refresh, 웹 HttpOnly 쿠키/CSRF 보호, TLS.
* UI 제한은 보안 제어가 아니다. 이미 기기에서 내려받은 파일을 원격 강제 회수하겠다고 약속하지 않는다.
-->

- 상세 정책: [ACCESS_CONTROL_v1.4.md](docs/ACCESS_CONTROL_v1.4.md)
- 순수 정책 함수(구현됨, **미연동**): [`internal/authz/policy.go`](internal/authz/policy.go)
- 계정·초대 API 목표: [`docs/API_AUTH_v1.4.yaml`](docs/API_AUTH_v1.4.yaml)
- SQLite 목표: [`docs/schema_v1.4.sql`](docs/schema_v1.4.sql)

## 5. 저장 엔진 · CAS · 원본 보호

**원본은 해시 기반 pool에 한 번 보관하고, 앨범과 파일 소속 관계를 별도 데이터로 관리**합니다. 같은 내용이 다른 폴더에 속하더라도 원본 물리 저장 공간을 재사용할 수 있어야 합니다. 필요 시 서버에서 생성하는 썸네일·프리뷰는 원본과 별도 관리합니다.

목표 구조(최종 마운트·인입 정책은 안전성 검증 후 확정):

```text
WithAlbumRoot/
├── .system/
│   ├── metadata.sqlite             # 목표 DB: 현재는 없음
│   ├── incoming/                   # 불완전 업로드 격리 영역
│   ├── pool/<hash-prefix>/<sha256> # 보호된 원본 blob (현재 PoC 존재)
│   ├── previews/                   # 경량 이미지·영상 프리뷰
│   └── backups/                    # 보존/복구 정책 확정 후 (미구현)
└── Imports/                        # Finder에서 직접 놓는 안전한 가져오기 영역 (미구현)
```

현재 프로토타입은 `metadata.sqlite`가 아닌 `.system/state.json`을 사용하고, 일부 원본 풀·기본 프리뷰 디렉터리만 만듭니다. 목표 DB는 사용자/기기/세션/앨범/멤버/초대/blobs/assets/폴더 연결/이벤트/감사 로그를 다룹니다.

<!-- 저장 안정성 / Claude의 최우선 개선 사항:
**현재 위험** newStore(root)가 경로를 mkdirAll로 생성한다. 외장 볼륨이 언마운트된 상태에서 같은 마운트 경로에 새 디렉터리가 생겨 다른 디스크에 기록되는 일을 막아야 한다.
볼륨 고유 ID/실제 마운트 여부/가용 용량/읽기쓰기 상태를 확인하고 fail-closed.
CAS pool 원본에 대해 해시가 같더라도 멀티파일 Live Photo, 메타데이터/자산 ID/폴더 연결은 별도 객체.
여러 계정·동시 업로드·디스크 전원 차단·DB 커밋 전후 실패·중복 응답 손실에서 원본 손실 없이 재시도.
최종 원본은 불변(immutable) 처리, 파일 경로 외부 노출 금지, Finder로 옮기는 폴더와 시스템 pool 분리.
원본 pool과 사용자가 보는 폴더를 하드링크로 마냥 연결하면 외부 변경으로 풀 원본이 훼손될 수 있어 무조건 사용하지 않는다.
백업·복구·원자적 파일 저장을 최우선으로 개발하며, 단일 디스크 저장을 안전한 독립 백업이라고 부르지 않는다.
-->

## 6. 네트워크 · API · 미디어 품질

**현재 HTTP 데모 API(현실):**

| Method | Path | 동작 | 보안 상태 |
|---|---|---|---|
| `GET` | `/` | 로컬 브라우저 UI | 인증 없음/loopback |
| `GET` | `/api/v1/health` | 서버 상태 | 인증 없음 |
| `GET` | `/api/v1/folders` | 전체 데모 폴더 반환 | **권한 없음** |
| `POST` | `/api/v1/folders` | 이름으로 폴더 생성 | **권한 없음** |
| `POST` | `/api/v1/folders/{folderID}/assets` | 원본 전체 업로드/해시 검증 | **권한 없음** |
| `GET` | `/api/v1/folders/{folderID}/sync?cursor=N` | 데모 변경 이벤트 | **권한 없음** |
| `GET` | `/api/v1/assets/{assetID}/original` | 원본 다운로드 | **권한 없음** |
| `GET` | `/api/v1/assets/{assetID}/preview` | 기초 프리뷰 | **권한 없음** |

이 표의 경로는 실제 `server/main.go`를 기준으로 합니다. 설계 문서의 API(`docs/API_v1.2.yaml`, `docs/API_AUTH_v1.4.yaml`)와 **서로 다르므로 목표 명세를 구현 완료로 오해하지 마세요.**

**미디어 목표:** HEIC/HEIF, JPG, PNG, RAW, Live Photo(HEIC+MOV 묶음), MP4/MOV, 4K/HDR, EXIF 및 촬영 날짜 유지. 원본 바이트는 변환하지 않고 보존, 프리뷰만 별도 생성. 현재 JPEG/PNG/GIF 기초 이미지 디코딩/간이 JPEG 미리보기 외에는 구현돼 있지 않습니다. 영상 프리뷰·변환은 미구현입니다.

<!-- API 개발 지시:
server/main.go는 512MiB 일괄 업로드만 지원. 대용량 영상 업로드 재개, idempotency receipt, hash preflight 정합성 등은 미구현.
차기 API에서 원본 다운로드는 단순 assets/{sha}만으로 접근하지 말고, 인증된 앨범 연결·역할을 검증.
초대/관리/원본/프리뷰/이벤트 모든 핸들러가 인증 미들웨어를 통과해야 한다.
새 OpenAPI 명세를 작성할 때 기존 API_v1.2+API_AUTH_v1.4의 차이를 조정하고 계약 테스트 작성.
네트워크 ACL을 완성하기 전 loopback 제한을 제거하지 말 것. HTTPS/페어링 설계 후 LAN 확장.
-->

## 7. macOS 최우선 설치 · Linux/Pi 배포 · 오픈소스 공개

| 대상 | 목표 패키지 | 현재 상태 |
|---|---|---|
| **Mac mini Apple Silicon (M1+)** | 서명·공증 DMG/PKG, 최초 설정 화면, 외장하드 지정, 메뉴바/백그라운드 서비스 | **기본 바이너리/실험용 LaunchAgent만** |
| Intel Mac | 동일 사용자 흐름 | 크로스빌드 대상 |
| Linux x86-64 | tar.gz / 장차 `.deb` 또는 systemd | 기본 스크립트 존재 |
| Raspberry Pi 64/32-bit | ARM64/ARMv7 바이너리, 메모리 경량화 | 크로스빌드 대상 |
| NAS | Docker/Compose와 스토리지 볼륨 명시 | 미구현 |

`GitHub Actions` CI는 테스트 및 5개 아키텍처 크로스빌드를 의도하고, `GoReleaser`는 태그 기반 릴리스/체크섬을 생성하도록 구성했습니다. 그러나 실제 Mac mini·Pi에서 설치/실행/재부팅 복구한 실기기 테스트는 여전히 필요합니다. **Releases의 프리릴리스와 정식 배포를 구분**하세요.

<!-- 배포/오픈소스 인수인계:
프로젝트 GitHub repository 및 리포지터리 소유자 아직 미정, go.mod는 example.com/withalbum placeholder.
릴리스 태그는 불완전한 로컬 시제품에 대해서 prerelease로만 발행. 정식 버전 1.0 / DMG 인증/상용 안전성 선언 금지.
GitHub README는 Release asset 이름과 Install docs, Known limitations를 사용자 눈에 보이도록 연결.
라이선스 LICENSE는 현 저장소에 MIT 텍스트가 들어 있지만 원래 기획상 '공개 전 재검토 초안'. 공개 전 최종 결정 및 검토가 필요하다.
macOS 서명/공증은 Apple Developer 권한과 실기기 검증이 필요하다; CI가 빌드됐다는 사실이 검증을 뜻하지 않는다.
-->

## 8. 개발 우선순위와 완료 조건 (Claude 작업 큐)

### P0 — 정식 서버 이전에 반드시 완성

- [ ] **기본 안정성**: SQLite 스키마·마이그레이션·WAL, `state.json` PoC에서의 안전한 데이터 전환, DB/파일 원자적 일관성, 외장 디스크 미마운트/용량 부족 차단, 백업/복원 실전 테스트.
- [ ] **계정·인증**: 로컬 최초 관리자 생성, 관리자 회원관리, Argon2id, 브루트포스 보호, 기기·세션·토큰 갱신/폐기, 인증된 대시보드.
- [ ] **앨범 ACL**: 새 앨범 생성 허가 기본 false, 단일 OWNER, 초대/수락, VIEWER/UPLOADER/CONTRIBUTOR, 회수 즉시 적용, 무단 원본/프리뷰/이벤트·hash 존재 정보 접근 차단.
- [ ] **보호된 네트워크**: 인증/ACL·HTTPS·CSRF/쿠키 경계 점검 이전에는 LAN listen 불가 유지.
- [ ] **데이터 테스트**: 강제 종료, 불완전 업로드, 저장 매체 분리, 응답 손실, 동시 업로드, 업그레이드 실패에서 복원 가능.

### P1 — Mac mini 실제 사용 UX + 모바일

- [ ] Apple Silicon Mac mini / Thunderbolt 외장 디스크 장시간 실사용 테스트.
- [ ] 사용자가 설치하는 Mac GUI DMG/PKG·백그라운드 서비스·초기 설정 마법사·복구 UI.
- [ ] iOS Share Extension, Android Send Intent, Flutter 앱·공유 앨범·업로드 큐/재시도/오프라인, 프리뷰·원본 다운로드.
- [ ] 가족이 새 사진을 넣으면 앨범별 증분 동기화; 클라이언트 저장공간 최적화; 미디어 포맷별 테스트.

### P2 — 플랫폼 및 서비스 확장

- [ ] Linux/Pi/NAS 실제 구동/설치/업데이트/자원 제한 검증, Docker Compose.
- [ ] Google Drive 다중 계정/실제 초대/OAuth `drive.file`/용량 차감/변경 추적 PoC 통과.
- [ ] Google Drive가 지원하는 역할만 표시하며 미지원 기능 안내; 서버와 Drive 간 수동 이관은 후속.
- [ ] 감사 로그, 운영 진단, 릴리스 노트, SBOM, 업데이트/롤백.

<!-- 개발 가드레일:
P0 구현 중 임의로 Product 요구사항을 후퇴/변경하지 않는다. 기존 테스트를 통과하면서 새 실패·권한 회수·디스크 장애 테스트를 추가한다.
최초 작업은 전체 코드와 문서를 확인하고 실제 동작과 목표의 차이를 정리하는 감사 보고서 + M1 세부 구현 계획이다.
GitHub에 곧바로 public push, Release 발행, 실제 원본 정리/삭제 등 파괴적 동작은 명시적 승인 전에 하지 않는다.
구현 완료 항목마다 README의 완료 상태/근거/테스트 변경. 그냥 TODO를 checked로 바꾸지 않는다.
-->

## 9. 저장소 파일 안내 — Claude에게 중요한 소스·문서

### 실행 코드와 개발 도구

| 경로 | 역할 / 구현 상태 |
|---|---|
| `server/main.go` | **실행됨:** localhost Go HTTP PoC, 원본·해싱·동기화·기초 프리뷰, 현재 인증·SQLite 없음 |
| `server/web/index.html` | **실행됨:** 내장 로컬 관리 UI; 관리자 인증 화면 아님 |
| `server/main_test.go`, `server/dashboard_test.go` | **테스트:** 현 PoC 동작 확인, 프로덕션 안전성 테스트 아님 |
| `internal/authz/policy.go`, `policy_test.go` | **부분 구현:** 역할 판정·단위 테스트. HTTP 서버 미연결 |
| `go.mod` | Go 모듈 버전, GitHub 주소 정하면 `module` 변경 필요 |
| `.goreleaser.yaml` | 5개 CPU/OS 조합 릴리스 압축 산출물 설정 |
| `.github/workflows/ci.yml`, `release.yml` | 테스트/크로스빌드 및 태그 릴리스 CI/CD |
| `.github/release.yml` | GitHub 릴리스 노트 분류 예시 |
| `scripts/install-macos.sh`, `install-linux.sh` | 개발용 백그라운드 실행 설치 스크립트, 정식 설치기 아님 |
| `Makefile`, `.env.example`, `.gitignore` | 개발 명령·환경·제외 경로 |
| `LICENSE`, `SECURITY.md` | MIT 초안, 현재 시제품 보안 경고 |

### 제품·설계·인수인계 문서

| 경로 | 읽을 내용 |
|---|---|
| **`CLAUDE.md`** | Claude Code 에이전트가 항상 따라야 할 프로젝트 규칙 |
| **`docs/FEATURE_STATUS_v1.5.md`** | 실제 코드/부분 구현/설계/미구현 구분표와 검사 항목 |
| **`docs/DECISIONS_v1.5.md`** | 변경된 사용자 요구·버림/유지한 방향 기록 |
| **`docs/PRODUCT_ARCHITECTURE_v1.4.md`** | 최신 통합 제품 정의·우선순위 |
| **`docs/ACCESS_CONTROL_v1.4.md`** | 계정, OWNER, 초대, 읽기/쓰기, 정보누출 예외까지 상세 |
| **`docs/schema_v1.4.sql`** | **목표** SQLite DDL, 현 PoC에 적용된 DB 아님 |
| **`docs/API_AUTH_v1.4.yaml`** | **목표** 인증/초대/권한 API 초안 |
| **`docs/API_v1.2.yaml`** | 초기 **목표** 업로드/동기화 API 초안, PhotoHub 표기 일부 잔존 |
| **`docs/GOOGLE_DRIVE_MODE_v1.3.md`** | OAuth·공유 폴더·Drive 제약/PoC 실험 계획 |
| **`docs/STORAGE_ARCHITECTURE_v1.3.md`** | 모바일 공통 저장 어댑터·provider capability 구조 |
| **`docs/CLIENT_SYNC_STATES.md`** | 송수신 큐·커서·실패·복구 상태 머신 |
| **`docs/ROADMAP.md`** | Mac-first 단계별 세부 로드맵 |
| **`docs/INSTALL_MACOS.md`, `INSTALL_LINUX.md`** | 현 개발자용 설치 지침 |
| **`docs/RELEASING.md`, `docs/releases/`** | GitHub Releases 자동 배포 및 릴리스 노트 |
| **`docs/OPEN_SOURCE_DECISIONS.md`** | 저장소 주소/라이선스 등 공개 전 결정 사항 |
| **`docs/PRODUCT_ARCHITECTURE_v1.2.md`, `_v1.3.md`** | 과거 버전, 결정 변화 확인용 |
| **`docs/schema_v1.2.sql`** | 과거 목표 DB, 최신 DB는 v1.4 |
| `CHANGELOG.md` | 문서·코드 변경 내역 |

<!-- 파일별 작업 규칙:
참조 우선순위: 명시적인 최신 사용자 요구 > README/CLAUDE.md의 통합 결정 > v1.4 설계 > v1.3 설계 > v1.2.
'모든 기능이 이미 코드에 있다'고 가정하지 마라. 파일은 요구의 근거이기도 하다.
과거 PhotoHub 이름·스키마를 발견하면 일괄 rename보다는 실제 외부 API/마이그레이션 호환성을 검토할 것.
-->

## 10. Claude가 작업을 시작할 때

**처음 읽어야 할 문서:** [CLAUDE.md](CLAUDE.md), [docs/FEATURE_STATUS_v1.5.md](docs/FEATURE_STATUS_v1.5.md).

Claude는 저장소 전체를 읽고 먼저 실제 구현 현황과 목표 설계의 차이를 제시한 후, **P0 서버 데이터 안전성 + 계정/ACL**부터 테스트와 함께 구현해야 합니다. Google Drive 개발이나 iOS/Android UI를 먼저 만들라는 지시는 아닙니다. 필요한 경우 구현 작업을 작은 PR/커밋 단위로 분리하세요.

<!-- 개발자의 성공 판정:
1. Mac mini에서 외장 디스크 분리시 파일을 잘못된 디스크에 저장하지 않는다.
2. 비인가 사용자는 서버 주소만으로 사진·폴더·프리뷰·원본을 읽지 못한다.
3. 기본 USER는 폴더 생성 불가, 허가받은 USER는 본인 소유 앨범 생성 가능.
4. VIEWER/UPLOADER/CONTRIBUTOR의 서버 측 권한이 매 요청마다 엄격히 강제된다.
5. 관리자도 타인 비공개 앨범을 API로 열람할 수 없다.
6. 업로드 재시도/스토리지 장애/재시작 뒤 메타데이터 및 원본이 정합하다.
7. Release/DMG 문서와 실제 빌드 산출물이 일치한다.
완료됐다고 주장할 땐 테스트 이름/재현 방법/환경을 함께 적는다.
-->

## 📄 라이선스와 공개 정책

이 패키지에는 기존 v1.4에 포함된 **MIT 라이선스 텍스트**를 보존했습니다. 하지만 이는 기획 단계에서 **최종 공개 전에 재검토하기로 했던 초안**입니다. 공식 GitHub 공개 전 라이선스 결정, 저장소/조직 이름, 상표 충돌, 보안 신고 창구, 유지보수 정책을 확정하세요.

**정리:** 위드앨범은 Mac mini를 우선 완성하는 멀티플랫폼 오픈소스 사진 서버와 iOS/Android 앱이며, 서버가 없는 이용자를 위한 Google Drive 모드는 차후 **제약을 명시한 별도 저장 방식**으로 개발합니다.
