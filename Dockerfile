# syntax=docker/dockerfile:1
#
# 베이스 이미지는 빌드 인자로 교체할 수 있다. 기본값은 Docker Hub 공식 이미지이며,
# Docker Hub 접근이 불가능해지거나 태그가 사라지는 경우를 대비해
# scripts/mirror-base-images.sh 로 ghcr.io/0hakxx/... 에 미러링한 뒤 아래처럼 빌드하면 된다.
#   docker build \
#     --build-arg NODE_IMAGE=ghcr.io/0hakxx/mirror/node:22-bookworm-slim \
#     --build-arg GO_IMAGE=ghcr.io/0hakxx/mirror/golang:1.26.10-bookworm \
#     --build-arg RUNTIME_IMAGE=ghcr.io/0hakxx/mirror/python:3.12-slim-bookworm \
#     -t prtseek:local .
ARG NODE_IMAGE=node:22-bookworm-slim
ARG GO_IMAGE=golang:1.26.10-bookworm
ARG RUNTIME_IMAGE=python:3.12-slim-bookworm

FROM ${NODE_IMAGE} AS web-build
WORKDIR /src
COPY web/package*.json ./web/
RUN cd web && npm ci --no-audit --no-fund
COPY . .
RUN cd web && npm run build:static

FROM ${GO_IMAGE} AS go-build
# GOPROXY 를 바꾸면 사설 프록시(Athens 등)나 오프라인 캐시(file://)로 모듈을 받을 수 있다.
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY}
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web-build /src/web/out ./server/webui/dist
RUN CGO_ENABLED=0 go build -trimpath -tags embedui -ldflags="-s -w -X main.version=$(tr -d '\r\n' < VERSION)" -o /prtseek ./cmd/prtseek

FROM ${RUNTIME_IMAGE}

# Playwright 패키지는 재현 가능한 빌드를 위해 버전을 고정한다(@latest 금지).
# @playwright/mcp·@playwright/cli 가 의존하는 playwright 버전과 맞춰야
# 미리 설치한 chromium 리비전을 그대로 재사용한다.
ARG PLAYWRIGHT_MCP_VERSION=0.0.84
ARG PLAYWRIGHT_CLI_VERSION=0.1.23
ARG PLAYWRIGHT_VERSION=1.65.0-alpha-2026-10-30

RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates ripgrep curl wget vim git jq unzip \
      dnsutils iputils-ping netcat-openbsd inetutils-telnet whois nmap \
    && rm -rf /var/lib/apt/lists/*
# Node.js 는 원격 설치 스크립트(deb.nodesource.com | bash) 대신
# 이미 받아 둔 web-build 단계 이미지에서 그대로 복사한다. (둘 다 Debian bookworm 기반)
COPY --from=web-build /usr/local/bin/node /usr/local/bin/node
COPY --from=web-build /usr/local/lib/node_modules /usr/local/lib/node_modules
RUN ln -sf ../lib/node_modules/npm/bin/npm-cli.js /usr/local/bin/npm \
    && ln -sf ../lib/node_modules/npm/bin/npx-cli.js /usr/local/bin/npx \
    && node --version && npm --version
# Playwright MCP 와 CLI 를 전역으로 미리 설치해 런타임에 npx 가 네트워크로 내려받지 않게 한다.
# @playwright/mcp: browser MCP 는 `npx @playwright/mcp` 로 바로 실행(전역 설치 완료, -y/@latest 불필요).
# @playwright/cli: playwright-cli 를 제공하며, 설치 후 --help 로 실행 가능 여부를 확인한다.
# playwright(브라우저 관리 제공)도 설치한 뒤 --with-deps 로 chromium 과 시스템 의존성을 미리 받아 두어,
# 컨테이너 안에서 MCP/CLI 가 첫 실행부터 바로 동작하고 브라우저를 다시 내려받지 않게 한다.
RUN npm install -g \
      "@playwright/mcp@${PLAYWRIGHT_MCP_VERSION}" \
      "@playwright/cli@${PLAYWRIGHT_CLI_VERSION}" \
      "playwright@${PLAYWRIGHT_VERSION}" \
    && playwright-cli --help \
    && playwright install --with-deps chromium \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /app
# 위 단계에서 소스로 빌드한 산출물을 사용한다.
COPY --from=go-build /prtseek /app/prtseek
# 감시(supervisor) 시작 스크립트: 프로세스가 종료되면 종료 코드에 따라 재시작 여부를 결정한다.
# 또한 SIGTERM 을 prtseek 에 전달해 컨테이너 중지 시 정상 종료되도록 한다.
COPY start.sh /app/start.sh
RUN chmod +x /app/prtseek /app/start.sh
COPY skills/ /app/skills/
# data/(SQLite + jwt.key) 영속화 지점
VOLUME ["/app/data"]
EXPOSE 8787 8788
LABEL org.opencontainers.image.source="https://github.com/0hakxx/prtseek"
ENTRYPOINT ["/app/start.sh"]
CMD ["-addr", ":8787", "-proxy", ":8788"]
