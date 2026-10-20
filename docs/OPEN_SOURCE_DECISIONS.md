# 첫 GitHub 공개 전에 결정해야 할 항목

1. **프로젝트 이름/저장소 경로:** `WithAlbum`는 임시 코드명입니다. 저장소는 `github.com/aaaicu/withalbum`로 확정, `go.mod` 반영 완료. 기존 상표·앱 이름과 충돌 여부는 아직 확인해야 합니다.
2. **라이선스:** **AGPL-3.0으로 확정 (2026-10-17).** 수정본을 네트워크 서비스로 제공할 때 소스 공개 의무가 있어, 타사의 비공개 상용 서비스화를 막는다. 저작권자는 별도 라이선스(듀얼 라이선스)를 제공할 수 있다.
3. **정식 배포 기준:** 인증/접근권한/디스크·DB 내구성/백업 복원 테스트/실기기 매트릭스/GUI 설치 프로그램 확보 후 정식판으로 표시합니다.
4. **Mac mini 우선 순위:** 애플 실리콘, 썬더볼트 외장 SSD/HDD, 자동 시작, 외장 디스크 분리 대응, macOS 코드 서명·공증을 먼저 검증합니다.
5. **기여 정책:** CONTRIBUTING, issue/PR 양식, 행동강령, 보안 신고/지원 버전 정책, CHANGELOG 관리 방법은 공개 전 완성합니다.
6. **데이터 정책:** 인증 정보 저장, 사용자 삭제 요청, 업로드 실패 및 중복 처리, 디스크 장애/백업 복원/기기 분실 위협 모델을 문서화합니다.

## 서드파티 의존성 (AGPL-3.0 호환성 확인)

`go.mod`/`go.sum`에 버전 고정. 모두 허용형(permissive) 라이선스로 AGPL-3.0 프로젝트에 포함·배포할 수 있으며, 바이너리 배포 시 각 저작권 고지를 함께 제공해야 한다: 릴리스 압축 파일의 `NOTICE`와 `third_party/go-modules/`(링크되는 모듈만 모아 `scripts/third-party-licenses.sh`로 생성, CI·릴리스에서 `--check`).

| 모듈 | 버전 | 라이선스 | 용도 |
|---|---|---|---|
| `modernc.org/sqlite` | v1.60.1 | BSD-3-Clause (내장 SQLite 3.53.4는 Public Domain) | cgo 없는 SQLite 드라이버 |
| `modernc.org/libc` | v1.77.1 | BSD-3-Clause (일부 musl 유래 코드 MIT, Go 유래 코드 BSD-3) | SQLite 변환 코드의 C 런타임 |
| `modernc.org/mathutil` | v1.7.1 | BSD-3-Clause | 간접 |
| `modernc.org/memory` | v1.12.1 | BSD-3-Clause (mmap-go 유래 BSD-3) | 간접 |
| `github.com/google/uuid` | v1.6.0 | BSD-3-Clause | 간접 |
| `github.com/remyoudompheng/bigfft` | v0.0.0-20230129092748-24d4a6f8daec | BSD-3-Clause | 간접 |
| `golang.org/x/crypto` | v0.58.0 | BSD-3-Clause | Argon2id 비밀번호 해시 (`argon2` 패키지) |
| `golang.org/x/term` | v0.47.0 | BSD-3-Clause | 콘솔 비밀번호 입력 시 에코 끄기 |
| `golang.org/x/sys` | v0.49.0 | BSD-3-Clause | 간접 |
| `github.com/dustin/go-humanize` | v1.0.1 | MIT | 간접 |
| `github.com/mattn/go-isatty` | v0.0.24 | MIT | 간접 |
| `github.com/ncruces/go-strftime` | v1.0.0 | MIT | 간접 |

## 디자인 토큰 출처 (Apache-2.0)

내장 관리 화면(`internal/httpapi/web/index.html`)의 `<style>` 블록에 있는 색·글자 크기·간격·모서리 값은 **SEED Design** 토큰에서 파생했다. 코드·글꼴·이미지·아이콘 파일은 가져오지 않았고 값(CSS 사용자 정의 속성)만 옮겼다.

| 항목 | 내용 |
|---|---|
| 원본 | SEED Design — `github.com/daangn/seed-design` (커밋 `0684acaa`, `packages/css/base.css`, `packages/rootage`) |
| 저작권 | 주식회사 당근마켓 (Daangn Market Co., Ltd.) |
| 라이선스 | Apache License 2.0 — https://www.apache.org/licenses/LICENSE-2.0 |
| 사용 범위 | 시맨틱 색 토큰(라이트·다크), 타입 스케일(t2–t10), dimension(간격)·radius·shadow 값 |
| 변경 | 브랜드 계열(`bg-brand-solid`·`bg-brand-weak`·`fg-brand`·`fg-brand-contrast`·`stroke-brand-weak`)을 SEED 팔레트 green 계열(라이트 `#00745F`, 다크 `#117956`)로 교체한 WithAlbum "보관 그린" 테마. 원격 글꼴(Pretendard) 대신 시스템 서체 스택, 작은 필수 글자는 대비 4.5:1을 위해 `fg-neutral-subtle` 대신 `fg-neutral-muted` 사용 |

Mac 메뉴 막대 앱(`macos/Sources/WithAlbumServerApp/Windows.swift`의 `Theme`)은 같은 파생 값 중 색 7개(brand·positive·warning·warning-weak·critical·neutral-weak·neutral-muted, 라이트·다크)만 쓴다. 앱 번들의 `Contents/Resources/third_party/seed-design/`에 같은 고지가 들어간다. Mac 앱 자체는 Apple 시스템 프레임워크(SwiftUI·AppKit·WebKit·CryptoKit·ServiceManagement)만 쓰고 서드파티 Swift 패키지가 없다.

Apache-2.0 파생물은 AGPL-3.0 프로젝트에 포함할 수 있다(Apache-2.0은 GPLv3 계열과 호환). 바이너리 배포 시 다른 의존성 고지와 함께 Apache-2.0 전문을 동봉한다: `third_party/seed-design/`(출처·변경 내용·라이선스 전문)과 `NOTICE`.
