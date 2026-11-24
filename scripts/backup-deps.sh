#!/usr/bin/env bash
# 외부 의존성(npm 패키지)을 오프라인 아카이브로 백업한다.
# 상위 저장소나 패키지 저장소가 사라져도 이 아카이브로 다시 빌드할 수 있다.
# Go 모듈은 vendor/ 를 저장소에 커밋해 두어 별도 백업이 필요 없다(go build -mod=vendor 로 빌드).
# npm 캐시는 용량이 커서(수백 MB) git 에 커밋하지 않고 dist/ 에 둔다 → GitHub Release 첨부 등으로 보관.
#
# 복원:
#   Go : go build -mod=vendor ./...                     (vendor/ 는 저장소에 포함됨)
#   npm: tar -xzf dist/npm-cache.tar.gz -C /tmp && (cd web && npm ci --offline --cache /tmp/npm-cache)
set -euo pipefail
cd "$(dirname "$0")/.."
mkdir -p dist

echo "Go 모듈 백업: vendor/ 가 저장소에 커밋되어 있어 별도 아카이브가 필요 없습니다(go build -mod=vendor ./...)."
# 깔끔한 vendor 상태를 보장하고 싶으면 아래 주석을 해제:
# go mod vendor
# echo "vendor/ 재생성 완료"

tmp="$(mktemp -d)"
(cd web && npm ci --no-audit --no-fund --cache "$tmp/npm-cache" >/dev/null)
tar -czf dist/npm-cache.tar.gz -C "$tmp" npm-cache
rm -rf "$tmp"
echo "npm 캐시 백업: dist/npm-cache.tar.gz"
