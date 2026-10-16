# Linux / Raspberry Pi 설치 — 개발용 패키지

현재 인증이 없는 **localhost-only 테스트 서버**입니다.

## 파일 선택

- Intel/AMD 64-bit Linux: `linux_amd64`
- Raspberry Pi OS 64-bit 등 ARM64: `linux_arm64`
- Raspberry Pi OS 32-bit 일부 모델(ARMv7): `linux_armv7`

다운로드한 `.tar.gz`를 해제하고 `withalbum-server`를 실행합니다. 실제 빌드 산출물 이름은 GitHub Releases Assets에서 확인하세요.

```bash
mkdir -p "$HOME/WithAlbumTest" && WITHALBUM_ROOT="$HOME/WithAlbumTest" ./withalbum-server -init-storage
WITHALBUM_ROOT="$HOME/WithAlbumTest" ./withalbum-server
```

서버는 저장소 루트를 만들지 않습니다. `-init-storage`는 **이미 있는** 디렉터리에 볼륨 마커(`.withalbum-volume.json`)를 한 번 기록하고 종료합니다. 외장 디스크는 마운트된 상태에서 그 안의 폴더로 초기화하세요. 마커가 없거나 실행 중 장치가 바뀌면 기동 거부/업로드 `503 storage_unavailable`(부분 구현, 실기기 미검증). 출력된 `volume_id`를 `WITHALBUM_VOLUME_ID`로 지정하면 다른 볼륨으로는 기동하지 않습니다(선택).

브라우저는 **같은 컴퓨터**의 `http://127.0.0.1:8787`에서 엽니다. 헤드리스 서버/라즈베리파이의 경우 아직 외부 기기로 접근하는 관리 UI가 없습니다.

## systemd --user 자동 시작 (개발용)

```bash
# 저장소 루트를 먼저 -init-storage로 초기화한 뒤 실행
bash scripts/install-linux.sh ./withalbum-server "$HOME/WithAlbumTest"
```

서비스는 사용자 세션에 묶입니다. 재로그인 이후에도 유지하고 싶다면 시스템 관리자가 `loginctl enable-linger` 설정을 검토해야 합니다. systemd가 없는 NAS에서는 스크립트를 사용하지 말고 지원되는 방식으로 바이너리를 직접 실행합니다.

Docker/Compose는 보안/저장소 볼륨 처리와 인증 기능이 준비된 뒤 지원할 계획입니다.
