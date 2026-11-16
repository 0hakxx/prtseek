#!/usr/bin/env bash
# Stop the launcher/service first. A failed build leaves the installed binary intact.
set -euo pipefail
cd "$(dirname "$0")"
mkdir -p .prtseek-run
mkdir .prtseek-run/lock 2>/dev/null || { echo 'Stop the launcher before updating (lock exists).' >&2; exit 1; }
trap 'rmdir .prtseek-run/lock' EXIT
prtseek_OUTPUT="$PWD/prtseek-next" bash ./build.sh
node scripts/build-state.mjs check ./prtseek-next
if [ -f ./prtseek ]; then cp ./prtseek ./prtseek.previous; fi
mv ./prtseek-next ./prtseek
mv ./prtseek-next.build.json ./prtseek.build.json
echo 'Installed current build. Start with ./start.sh (or restart your service).'
