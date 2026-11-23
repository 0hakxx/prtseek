#!/usr/bin/env bash
# Dockerfile 이 사용하는 외부 베이스 이미지를 내 레지스트리로 미러링하고
# 선택적으로 tar 파일로 저장한다. Docker Hub 의 태그가 사라지거나 접근이 막혀도
# USE_MIRROR=1 bash scripts/docker-build.sh 로 계속 빌드할 수 있게 하기 위함이다.
#
# 사용법:
#   echo "$GHCR_TOKEN" | docker login ghcr.io -u 0hakxx --password-stdin
#   bash scripts/mirror-base-images.sh            # ghcr.io/0hakxx/mirror/* 로 푸시
#   SAVE=1 PUSH=0 bash scripts/mirror-base-images.sh   # dist/base-images.tar.gz 로만 저장
set -euo pipefail
cd "$(dirname "$0")/.."

MIRROR="${MIRROR:-ghcr.io/0hakxx/mirror}"
IMAGES=(node:22-bookworm-slim golang:1.26.10-bookworm python:3.12-slim-bookworm)

for img in "${IMAGES[@]}"; do
  docker pull "$img"
  if [ "${PUSH:-1}" = 1 ]; then
    docker tag "$img" "$MIRROR/$img"
    docker push "$MIRROR/$img"
  fi
done

if [ "${SAVE:-0}" = 1 ]; then
  mkdir -p dist
  docker save "${IMAGES[@]}" | gzip > dist/base-images.tar.gz
  echo "저장 완료: dist/base-images.tar.gz  (복원: docker load < dist/base-images.tar.gz)"
fi
