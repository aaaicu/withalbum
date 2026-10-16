# GitHub Releases / 버전 배포

## 목표

README의 다운로드 링크 → GitHub Releases → macOS ZIP / Linux tar.gz / Raspberry Pi tar.gz + `checksums.txt` + 변경 내역.

`Releases`는 GitHub 저장소 안의 `release/` 폴더가 아니라 GitHub 자체 릴리스 화면입니다.

## 자동화 파일

- `.github/workflows/ci.yml` — PR, main 테스트/정적검사/5개 플랫폼 크로스빌드
- `.github/workflows/release.yml` — `v*` 태그를 push하면 GoReleaser 실행
- `.goreleaser.yaml` — 빌드 대상, 바이너리 이름, 압축 파일, 체크섬, 변경 내역 설정
- `.github/release.yml` — GitHub UI에서 릴리스 노트 수동 자동 생성 시 PR 분류

## 최초 공개 전 주의

- 코드의 `go.mod` 모듈 경로는 테스트용(`example.com/withalbum`)입니다. 실제 GitHub 위치로 변경합니다.
- 현재 인증이 없는 PoC입니다. **`v0.1.0-alpha.1` 같은 prerelease로만 공개**하고, 실사용을 권장하지 않습니다.
- GitHub에 소스 코드를 올렸다는 사실이 앱 설치/실기기 테스트를 대신하지 않습니다.
- macOS ZIP 바이너리는 아직 Apple 서명·공증이 되지 않았습니다. DMG라고 표시하지 않습니다.
- MIT 라이선스는 초안이므로 첫 공개 전에 확정합니다.

## 태그 릴리스 (저장소 owner가 실행)

```bash
git add .
git commit -m "chore: prepare alpha release"
git push origin main
git tag -a v0.1.0-alpha.1 -m "WithAlbum Server alpha: local-only prototype"
git push origin v0.1.0-alpha.1
```

릴리스 워크플로가 테스트 후 운영체제별 압축 파일과 체크섬, 기본 변경 기록을 업로드합니다. GitHub Actions의 `GITHUB_TOKEN`은 저장소에서 제공하며 contents:write 권한이 필요합니다. 릴리스 후 변경 내역을 사람이 검토·수정하세요.

### 향후 정식판 가이드

- 정식판 전에는 Mac 바이너리 코드 서명 및 공증 검토, 설치기(UI) 배포
- 배포 파일 SHA-256 검증, 가능하면 공급망 증명과 취약점 스캔
- 서버 DB/설정 백업 확인 후 안전한 업데이트 및 자동 롤백
- 역호환 API, DB 마이그레이션 가이드 및 Release Notes의 Breaking changes 섹션
