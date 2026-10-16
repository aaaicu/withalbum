# 첫 GitHub 공개 전에 결정해야 할 항목

1. **프로젝트 이름/저장소 경로:** `WithAlbum`는 임시 코드명입니다. 저장소는 `github.com/aaaicu/withalbum`로 확정, `go.mod` 반영 완료. 기존 상표·앱 이름과 충돌 여부는 아직 확인해야 합니다.
2. **라이선스:** **AGPL-3.0으로 확정 (2026-10-17).** 수정본을 네트워크 서비스로 제공할 때 소스 공개 의무가 있어, 타사의 비공개 상용 서비스화를 막는다. 저작권자는 별도 라이선스(듀얼 라이선스)를 제공할 수 있다.
3. **정식 배포 기준:** 인증/접근권한/디스크·DB 내구성/백업 복원 테스트/실기기 매트릭스/GUI 설치 프로그램 확보 후 정식판으로 표시합니다.
4. **Mac mini 우선 순위:** 애플 실리콘, 썬더볼트 외장 SSD/HDD, 자동 시작, 외장 디스크 분리 대응, macOS 코드 서명·공증을 먼저 검증합니다.
5. **기여 정책:** CONTRIBUTING, issue/PR 양식, 행동강령, 보안 신고/지원 버전 정책, CHANGELOG 관리 방법은 공개 전 완성합니다.
6. **데이터 정책:** 인증 정보 저장, 사용자 삭제 요청, 업로드 실패 및 중복 처리, 디스크 장애/백업 복원/기기 분실 위협 모델을 문서화합니다.

## 서드파티 의존성 (AGPL-3.0 호환성 확인)

`go.mod`/`go.sum`에 버전 고정. 모두 허용형(permissive) 라이선스로 AGPL-3.0 프로젝트에 포함·배포할 수 있으며, 바이너리 배포 시 각 저작권 고지를 함께 제공해야 한다(릴리스 패키징에서 처리 예정).

| 모듈 | 버전 | 라이선스 | 용도 |
|---|---|---|---|
| `modernc.org/sqlite` | v1.60.1 | BSD-3-Clause (내장 SQLite 3.53.4는 Public Domain) | cgo 없는 SQLite 드라이버 |
| `modernc.org/libc` | v1.77.1 | BSD-3-Clause (일부 musl 유래 코드 MIT, Go 유래 코드 BSD-3) | SQLite 변환 코드의 C 런타임 |
| `modernc.org/mathutil` | v1.7.1 | BSD-3-Clause | 간접 |
| `modernc.org/memory` | v1.12.1 | BSD-3-Clause (mmap-go 유래 BSD-3) | 간접 |
| `github.com/google/uuid` | v1.6.0 | BSD-3-Clause | 간접 |
| `github.com/remyoudompheng/bigfft` | v0.0.0-20230129092748-24d4a6f8daec | BSD-3-Clause | 간접 |
| `golang.org/x/sys` | v0.48.0 | BSD-3-Clause | 간접 |
| `github.com/dustin/go-humanize` | v1.0.1 | MIT | 간접 |
| `github.com/mattn/go-isatty` | v0.0.24 | MIT | 간접 |
| `github.com/ncruces/go-strftime` | v1.0.0 | MIT | 간접 |
