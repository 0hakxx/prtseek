#!/usr/bin/env bash
# 개발 모드: 백엔드(:8787) + 트래픽 프록시(:8788) 와 프론트엔드 next dev(:5173) 를 함께 실행한다.
# 프론트엔드 /api 는 백엔드로 리버스 프록시하며, Ctrl-C 로 함께 종료한다.
#
# 단일 바이너리(프론트엔드 내장) 방식은 README 의 「단일 바이너리」 절을 참고하며, 이 스크립트를 쓰지 않는다.
set -euo pipefail
cd "$(dirname "$0")"

backend_pid=
frontend_pid=
cleanup() {
    trap - EXIT INT TERM
    for pid in "$backend_pid" "$frontend_pid"; do
        if [ -n "$pid" ]; then kill "$pid" 2>/dev/null || true; fi
    done
    wait 2>/dev/null || true
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

command -v go >/dev/null || { echo 'Go is required.' >&2; exit 1; }
command -v node >/dev/null || { echo 'Node.js is required.' >&2; exit 1; }
[ -d web/node_modules ] || (cd web && npm ci)
mkdir -p .dev
go build -o .dev/prtseek-dev ./cmd/prtseek

# 백엔드(일반 go run, 프론트엔드 내장 안 함). 동시 실행 work agent 수는 「시스템 설정」에서 설정한다.
.dev/prtseek-dev -data "$PWD/data" -addr 127.0.0.1:8787 -proxy 127.0.0.1:8788 &
backend_pid=$!

# 프론트엔드 핫 리로드(Vite/Next dev server, /api 는 :8787 로 리버스 프록시).
( cd web && exec node node_modules/next/dist/bin/next dev --hostname 127.0.0.1 --port 5173 ) &
frontend_pid=$!

echo "[dev] 백엔드 :8787 / 프록시 :8788 / 프론트엔드 http://localhost:5173  (Ctrl-C 로 종료)"
while kill -0 "$backend_pid" 2>/dev/null && kill -0 "$frontend_pid" 2>/dev/null; do
    sleep 1
done
if ! kill -0 "$backend_pid" 2>/dev/null; then wait "$backend_pid"; else wait "$frontend_pid"; fi
