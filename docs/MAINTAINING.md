# 독립 유지보수 가이드

이 저장소(`https://github.com/0hakxx/prtseek`)는 원본 저장소
`Team-intN18-SoybeanSeclab/prtseek` 의 전체 이력(브랜치·태그·커밋)을 그대로 이관한 것이다.
원본이 사라지거나 접근 불가해져도 이 저장소와 아래 백업만으로 빌드·실행·유지보수할 수 있도록 구성했다.

## 1. 원격 저장소 구성

| 원격 | URL | 용도 |
| --- | --- | --- |
| `origin` | `https://github.com/0hakxx/prtseek.git` | 기본 저장소(푸시 대상) |
| `upstream` | `https://github.com/Team-intN18-SoybeanSeclab/prtseek.git` | 원본(선택, 읽기 전용). 사라지면 `git remote remove upstream` |

원본에 새 변경이 올라왔을 때 가져오기(원본이 살아 있는 동안만):

```bash
git fetch upstream --tags
git merge upstream/master   # 번역 충돌은 수동 해결
```

## 2. 외부 의존 지점과 대비책

| 지점 | 위치 | 대비책 |
| --- | --- | --- |
| Docker Hub 베이스 이미지 `node:22-bookworm-slim`, `golang:1.26.10-bookworm`, `python:3.12-slim-bookworm` | `Dockerfile` | `scripts/mirror-base-images.sh` 로 `ghcr.io/0hakxx/mirror/*` 에 미러링 → `USE_MIRROR=1 bash scripts/docker-build.sh` |
| NodeSource 원격 설치 스크립트(`curl … \| bash`) | (제거됨) | Node.js 를 node 베이스 이미지에서 직접 복사하도록 변경 |
| npm 전역 패키지 `@playwright/mcp`, `@playwright/cli`, `playwright` (`@latest`) | `Dockerfile` | 버전 고정(`PLAYWRIGHT_*_VERSION` 빌드 인자) |
| Playwright chromium 다운로드(Microsoft CDN) | `Dockerfile` | 빌드된 이미지 자체를 GHCR 에 푸시·`docker save` 로 보관 |
| Debian apt 저장소 | `Dockerfile` | 빌드된 이미지 보관으로 대체 |
| Go 모듈 `github.com/Autumn-27/norma v0.4.3` (131 개 파일에서 import, 상위 메인테이너 소유) | `go.mod` | **1차: `vendor/` 를 저장소에 커밋** → `go build -mod=vendor ./...` 로 네트워크 없이 빌드. **2차: `github.com/0hakxx/norma` 미러 저장소**(v0.4.3 포함 전체 태그/브랜치 미러됨). vendor 가 깨졌을 때 `go.mod` 에 다음 추가: `replace github.com/Autumn-27/norma => github.com/0hakxx/norma v0.4.3` 후 `GOPROXY=https://proxy.golang.org,direct go mod tidy && go mod vendor` |
| 기타 Go 모듈(`golang.org/x/*`, `projectdiscovery/*`, `modernc.org/sqlite` 등) | `go.mod` | `vendor/` 에 소스 포함(253 MB). 네트워크 없이 `-mod=vendor` 로 빌드 가능 |
| npm 패키지(`registry.npmjs.org`) | `web/package-lock.json` | `scripts/backup-deps.sh` → `dist/npm-cache.tar.gz` (`npm ci --offline`) |
| 자동 업데이트(`selfupdate`, `RuoJi6/ARTEX` 릴리스) | `selfupdate/github.go` | prtseek 빌드(`VERSION` 이 `prtseek-` 로 시작)에서는 비활성화되어 있어 런타임 의존 없음 |

> Go 모듈 경로 `github.com/Autumn-27/artex` 는 **import 경로 이름일 뿐** 네트워크 접근이 없으므로 그대로 둔다.

## 3. 이미지 빌드·푸시

```bash
# 로컬 빌드
bash scripts/docker-build.sh                    # → prtseek:local
docker compose up -d                            # 로컬 빌드 후 실행

# GHCR 푸시 (PAT 에 write:packages 권한 필요)
echo "$GHCR_TOKEN" | docker login ghcr.io -u 0hakxx --password-stdin
PUSH=1 bash scripts/docker-build.sh             # → ghcr.io/0hakxx/prtseek:<버전>, :latest

# Docker Hub 로 푸시하려면
docker login -u 0hakxx
REGISTRY=docker.io/0hakxx PUSH=1 bash scripts/docker-build.sh

# 오프라인 백업
SAVE=1 bash scripts/docker-build.sh             # → dist/prtseek-image-<버전>.tar.gz
SAVE=1 PUSH=0 bash scripts/mirror-base-images.sh  # → dist/base-images.tar.gz
bash scripts/backup-deps.sh                     # → dist/npm-cache.tar.gz (Go 는 vendor/ 가 저장소에 있음)
```

## 3.5 "상위가 전부 사라졌을 때" 시나리오별 복구

여기서 **상위**는 다음 네 저장소를 뜻한다: `Team-intN18-SoybeanSeclab/prtseek`, `Autumn-27/norma`,
`Autumn-27/ARTEX`, `RuoJi6/ARTEX`. **공개 인프라**(Docker Hub, npm registry, Debian apt, Microsoft
Playwright CDN, proxy.golang.org)는 상위와는 별개 리스크로 본다.

### 상위가 사라졌지만 공개 인프라는 살아 있음 (가장 흔한 시나리오)
**추가 설정이 필요 없다**. clone → build → run 이 그대로 된다.
- `go build -mod=vendor ./...` : `vendor/` 가 저장소에 있어 네트워크 0 바이트.
- `docker build .` : Dockerfile 이 `GOPROXY=off` 로 하드코딩되어 Go 는 상위를 전혀 조회하지 않음.
  베이스 이미지는 Docker Hub 에서, 런타임 의존은 npm/apt/Microsoft CDN 에서 받아 온다.
- 이미 `docker pull ghcr.io/0hakxx/prtseek:latest` 된 이미지는 실행 시 어떤 공개 인프라도 필요 없음(LLM API 키 제외).

### vendor/ 가 유실된 상태 (git clean -fdx 실수 등)
`vendor/` 는 git 에 커밋되어 있으니 `git checkout -- vendor/` 로 복원이 먼저다. 커밋에서도 사라졌다면:
```bash
# go.mod 끝에 한 줄 추가 (Autumn-27/norma 미러로 리맵)
echo 'replace github.com/Autumn-27/norma => github.com/0hakxx/norma v0.4.3' >> go.mod
GOPROXY='https://proxy.golang.org,direct' go mod tidy
go mod vendor
```

### Docker Hub 접근이 끊긴 상태 (상위와 무관)
```bash
# 1) base-images.tar.gz 를 미리 다운로드해 뒀다면:
docker load < dist/base-images.tar.gz
bash scripts/docker-build.sh   # 로컬 이미지 그대로 사용

# 2) 또는 GHCR 미러에 접근 가능하면:
docker login ghcr.io -u <id>   # mirror 가 private 이면 로그인 필요
USE_MIRROR=1 bash scripts/docker-build.sh
```

### npm registry 접근이 끊긴 상태 (상위와 무관)
```bash
tar -xzf dist/npm-cache.tar.gz -C /tmp
(cd web && npm ci --offline --cache /tmp/npm-cache)
```

### 완전 air-gap (모든 공개 인프라 접근 불가)
`dist/` 아래 세 아카이브가 사전에 준비되어 있어야 한다.
```bash
docker load < dist/prtseek-image-prtseek-v0.1.0.tar.gz   # 완성 이미지 그대로 로드
docker run -d --name prtseek -p 8787:8787 prtseek-v0.1.0
# 이미지 재빌드가 필요하면 base-images.tar.gz + npm-cache.tar.gz 를 조합한 뒤 USE_MIRROR=1 로 빌드.
```

### 모듈 업데이트가 필요한 경우 (상위 살아 있을 때만)
Dockerfile 의 `GOPROXY=off` 가 모듈 다운로드를 막는다. 일회성으로 덮어쓴다:
```bash
docker build --build-arg GOPROXY='https://proxy.golang.org,direct' -t prtseek:refresh .
go mod tidy && go mod vendor   # 호스트에서 vendor/ 재생성
```

## 3.6 Go proxy 캐시 사전 준비 (선택)

`0hakxx/norma@v0.4.3` 는 저장소가 **private 이던 동안** Go proxy 에 색인되지 못했을 수 있다.
public 전환 후 한 번 요청하면 영구 캐시된다:
```bash
curl -sI https://proxy.golang.org/github.com/0hakxx/norma/@v/v0.4.3.info
curl -sI https://proxy.golang.org/github.com/0hakxx/norma/@v/v0.4.3.mod
curl -sO https://proxy.golang.org/github.com/0hakxx/norma/@v/v0.4.3.zip
```
이렇게 한 번 트리거해 두면, 이후 `replace` 지시어로 미러를 쓸 때 추가 세팅이 필요 없다.

GitHub Actions `.github/workflows/docker-publish.yml` 은 `master`·`prtseek-v*` 태그 푸시 때
`ghcr.io/<owner>/prtseek` 로 이미지를 자동 푸시한다. 수동 실행 시 `mirror_base` 를 켜면 베이스 이미지도 미러링한다.
`dist/` 의 백업 아카이브는 용량이 커서 git 에 넣지 말고 GitHub Release 첨부 등으로 보관한다.

## 4. 번역 관련 주의

중국어 원문 UI/프롬프트/주석을 한국어로 번역했다. 아래처럼 **문자열 자체가 프로토콜로 쓰이는 곳**은
양쪽(생산 코드·프론트엔드·테스트)을 같은 한국어로 맞췄고, 기존 데이터 호환을 위해 일부는 중국어 원문도 함께 인식한다.

- 승인 사유 접두사 `[模型]` → `[모델]`, `人工允许/人工拒绝` → `수동 허용/수동 거부`
- 위험 작업 사유 형식 `实际操作：…；成功后的后果：…` → `실제 작업: …; 성공 시 결과: …`
- 채팅 멘션 `@[漏洞#1]` 등 → `@[취약점#1]` (기존 중국어 멘션도 인식)
