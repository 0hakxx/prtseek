#!/usr/bin/env bash
# prtseek 컨테이너 이미지를 로컬에서 빌드하고, 필요하면 레지스트리로 푸시한다.
#
# 사용법:
#   bash scripts/docker-build.sh                 # prtseek:local 로컬 빌드만
#   PUSH=1 bash scripts/docker-build.sh          # ghcr.io/0hakxx/prtseek:<버전>, :latest 로 푸시
#   REGISTRY=docker.io/0hakxx PUSH=1 bash scripts/docker-build.sh   # Docker Hub 로 푸시
#   USE_MIRROR=1 bash scripts/docker-build.sh    # 베이스 이미지를 내 미러(ghcr.io/0hakxx/mirror)에서 받음
#   SAVE=1 bash scripts/docker-build.sh          # dist/prtseek-image-<버전>.tar.gz 로 오프라인 백업
#
# 푸시 전 로그인:  echo "$GHCR_TOKEN" | docker login ghcr.io -u 0hakxx --password-stdin
set -euo pipefail
cd "$(dirname "$0")/.."

REGISTRY="${REGISTRY:-ghcr.io/0hakxx}"
MIRROR="${MIRROR:-ghcr.io/0hakxx/mirror}"
VERSION="$(tr -d '\r\n' < VERSION)"
LOCAL_TAG="${LOCAL_TAG:-prtseek:local}"

args=()
if [ "${USE_MIRROR:-0}" = 1 ]; then
  args+=(--build-arg "NODE_IMAGE=$MIRROR/node:22-bookworm-slim"
         --build-arg "GO_IMAGE=$MIRROR/golang:1.26.10-bookworm"
         --build-arg "RUNTIME_IMAGE=$MIRROR/python:3.12-slim-bookworm")
fi
[ -n "${GOPROXY:-}" ] && args+=(--build-arg "GOPROXY=$GOPROXY")

docker build "${args[@]}" -t "$LOCAL_TAG" .
echo "로컬 이미지 빌드 완료: $LOCAL_TAG"

if [ "${PUSH:-0}" = 1 ]; then
  for tag in "$VERSION" latest; do
    docker tag "$LOCAL_TAG" "$REGISTRY/prtseek:$tag"
    docker push "$REGISTRY/prtseek:$tag"
  done
  echo "푸시 완료: $REGISTRY/prtseek:{$VERSION,latest}"
fi

if [ "${SAVE:-0}" = 1 ]; then
  mkdir -p dist
  docker save "$LOCAL_TAG" | gzip > "dist/prtseek-image-$VERSION.tar.gz"
  echo "오프라인 백업: dist/prtseek-image-$VERSION.tar.gz  (복원: docker load < 파일)"
fi
