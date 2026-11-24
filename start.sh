#!/bin/sh
# prtseek 감시 시작 스크립트(Linux / macOS / Docker ENTRYPOINT)
#
# 사용법:
#   ./start.sh                       포그라운드 실행(Ctrl-C 로 중지)
#   nohup ./start.sh >prtseek.log 2>&1 &   백그라운드 상주
#   ./start.sh -addr :9000           추가 인자는 prtseek 에 그대로 전달
#
# 이 스크립트는 한 가지만 한다: prtseek 를 실행하고, 프로세스가 종료되면 종료 코드에 따라 다시 띄울지 결정한다.
#
#   0      사용자 정상 중지        → 루프 종료
#   75     프로그램이 재시작 요청     → 즉시 다시 실행(종료 코드 프로토콜 유지)
#   그 외   충돌                → 백오프 후 다시 실행(1→2→4… 최대 60초)
#
# 이 스크립트는 프로세스와 신호만 관리하며, 바이너리를 다운로드하거나 교체하지 않는다.
# prtseek 개인 빌드는 상위 프로젝트 자동 업데이트를 비활성화한다. 종료 코드 75 는 재시작 프로토콜로 계속 유지된다.
set -u

cd "$(dirname "$0")" || exit 1

BIN=./prtseek
mkdir -p .prtseek-run
if ! mkdir .prtseek-run/lock 2>/dev/null; then
    echo '[prtseek] Launcher lock exists. Stop the previous launcher before starting/updating. If it crashed, verify it is stopped before removing .prtseek-run/lock.' >&2
    exit 1
fi
echo $$ > .prtseek-run/lock/pid
trap 'rm -f .prtseek-run/lock/pid; rmdir .prtseek-run/lock' 0
if [ -f go.mod ]; then
    command -v node >/dev/null 2>&1 || { echo 'Source checkout requires Node.js and Go.' >&2; exit 1; }
    if ! node scripts/build-state.mjs check "$BIN"; then
        prtseek_OUTPUT="$PWD/prtseek-next" bash ./build.sh || exit 1
        node scripts/build-state.mjs check ./prtseek-next || exit 1
        if [ -f "$BIN" ]; then cp "$BIN" "$BIN.previous" || exit 1; fi
        mv ./prtseek-next "$BIN" || exit 1
        mv ./prtseek-next.build.json "$BIN.build.json" || exit 1
    fi
fi
[ -x "$BIN" ] || { echo 'Release executable is missing or not executable.' >&2; exit 1; }

RESTART_CODE=75
MAX_DELAY=60

child=0
stopping=0

# 중지 신호를 prtseek 본체에 전달한다.
#
# Docker 에서는 이것이 필수다: docker stop 은 SIGTERM 을 PID 1(즉 이 스크립트)에만 보내고
# 하위 프로세스에는 보내지 않는다. 전달하지 않으면 prtseek 가 신호를 받지 못해 정상 종료를 못 하고, 10초 뒤 SIGKILL 로
# 강제 종료되어 실행 중인 작업이 도중에 끊긴다.
forward() {
	stopping=1
	if [ "$child" -ne 0 ]; then
		kill -TERM "$child" 2>/dev/null || true
	fi
}
trap forward INT TERM

delay=1
while :; do
	"$BIN" "$@" &
	child=$!

	# 신호는 wait 를 끊고 >128 을 반환하게 한다. 이때 하위 프로세스는 사실 정상 종료 중이므로,
	# wait 를 한 번 더 해야 그것의 진짜 종료 코드를 얻을 수 있다.
	wait "$child"
	code=$?
	if [ "$code" -gt 128 ]; then
		wait "$child"
		code=$?
	fi
	child=0

	if [ "$stopping" -eq 1 ]; then
		echo "[prtseek] 중지됨"
		exit 0
	fi

	case "$code" in
		0)
			echo "[prtseek] 정상 종료"
			exit 0
			;;
		"$RESTART_CODE")
			# 애플리케이션이 재시작을 요청함. 이 스크립트는 프로그램을 다운로드하거나 교체하지 않는다.
			echo "[prtseek] 재시작 요청…"
			delay=1
			;;
		*)
			echo "[prtseek] 비정상 종료 (code=$code), ${delay}s 후 재시작" >&2
			sleep "$delay"
			delay=$((delay * 2))
			[ "$delay" -gt "$MAX_DELAY" ] && delay=$MAX_DELAY
			;;
	esac
done
