#!/usr/bin/env bash
# 외부 의존성(Go 모듈, npm 패키지)을 오프라인 아카이브로 백업한다.
# 상위 저장소나 패키지 저장소가 사라져도 이 아카이브로 다시 빌드할 수 있다.
# 결과물은 용량이 커서(수백 MB) git 에 커밋하지 않고 dist/ 에 둔다 → GitHub Release 첨부 등으로 보관.
#
# 복원:
#   Go : tar -xzf dist/go-vendor.tar.gz && go build -mod=vendor ./...
#   npm: tar -xzf dist/npm-cache.tar.gz -C /tmp && (cd web && npm ci --offline --cache /tmp/npm-cache)
set -euo pipefail
cd "$(dirname "$0")/.."
mkdir -p dist

go mod vendor
tar -czf dist/go-vendor.tar.gz vendor
rm -rf vendor
echo "Go 모듈 백업: dist/go-vendor.tar.gz"

tmp="$(mktemp -d)"
(cd web && npm ci --no-audit --no-fund --cache "$tmp/npm-cache" >/dev/null)
tar -czf dist/npm-cache.tar.gz -C "$tmp" npm-cache
rm -rf "$tmp"
echo "npm 캐시 백업: dist/npm-cache.tar.gz"
