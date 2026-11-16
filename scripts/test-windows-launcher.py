"""Exercise the actual launcher against isolated services; the builder is a fixture.

The separate build-state tests cover real source hashing. This fixture avoids a
full npm/Go rebuild per launcher scenario, while running the real application.
"""
import hashlib
import ctypes
import json
import os
from pathlib import Path
import shutil
import socket
import subprocess
import sys
import tempfile
import urllib.request

ROOT = Path(__file__).resolve().parent.parent
PS = str(Path(os.environ['SystemRoot']) / 'System32/WindowsPowerShell/v1.0/powershell.exe')


def quote(value):
    return "'" + str(value).replace("'", "''") + "'"


def main():
    binary = Path(sys.argv[1]).resolve(strict=True)
    build = json.loads((ROOT / 'server/webui/dist/build-info.json').read_text())
    with tempfile.TemporaryDirectory(prefix='prtseek launcher ') as temporary:
        # Windows runners may expose TEMP through RUNNER~1, while CIM returns
        # the long executable path. Compare canonical filesystem paths.
        root = Path(temporary).resolve()
        (root / '.dev').mkdir()
        (root / 'scripts').mkdir()
        for name in ['start-prtseek.ps1', 'start-prtseek.cmd', 'update-prtseek.cmd']:
            shutil.copy2(ROOT / name, root / name)
        (root / 'go.mod').write_text('fixture')
        (root / 'generation').write_text('1')
        # Legacy root exe must never shadow the source checkout build.
        (root / 'prtseek.exe').write_bytes(b'old non-executable root artifact')
        state = r"""
const fs=require('fs'), crypto=require('crypto'), path=require('path');
const root=path.resolve(__dirname,'..'), binary=process.argv[3];
const manifest=()=>({...JSON.parse(process.env.FIXTURE_BUILD),fixture_generation:fs.readFileSync(path.join(root,'generation'),'utf8'),binary_sha256:crypto.createHash('sha256').update(fs.readFileSync(binary)).digest('hex')});
if(process.argv[2]==='write') fs.writeFileSync(binary+'.build.json',JSON.stringify(manifest()));
else {try {process.exitCode=JSON.stringify(JSON.parse(fs.readFileSync(binary+'.build.json')))==JSON.stringify(manifest())?0:1;}catch {process.exitCode=1;}}
"""
        (root / 'scripts/build-state.mjs').write_text(state.replace("const fs=require('fs'), crypto=require('crypto'), path=require('path');", "import fs from 'node:fs'; import crypto from 'node:crypto'; import path from 'node:path'; import {fileURLToPath} from 'node:url'; const __dirname=path.dirname(fileURLToPath(import.meta.url));"))
        builder = """
$ErrorActionPreference='Stop'
if(Test-Path -LiteralPath (Join-Path $PSScriptRoot 'fail-build')) { throw 'Fixture compilation failure' }
Copy-Item -LiteralPath $env:FIXTURE_BINARY -Destination (Join-Path $PSScriptRoot '.dev/prtseek-next.exe') -Force
& node (Join-Path $PSScriptRoot 'scripts/build-state.mjs') write (Join-Path $PSScriptRoot '.dev/prtseek-next.exe')
if($LASTEXITCODE -ne 0) {throw 'fixture metadata failed'}
"""
        (root / 'build-prtseek.ps1').write_text(builder)
        env = dict(os.environ, FIXTURE_BINARY=str(binary), FIXTURE_BUILD=json.dumps(build),
                   prtseek_CONFIG=str(root/'missing.json'), prtseek_DB_PATH=str(root/'test.db'), prtseek_SKILL_DIR=str(root/'skills'))
        sock = socket.socket()
        sock.bind(('127.0.0.1', 0))
        port = sock.getsockname()[1]
        sock.close()

        def launch(*extra, ok=True, short_path=False):
            # A background Windows child can inherit a pipe handle even when
            # its own stdout is redirected. Use files so communicate() does
            # not wait for a healthy long-lived service to close that handle.
            script = str(root/'start-prtseek.ps1')
            if short_path:
                buffer = ctypes.create_unicode_buffer(32768)
                length = ctypes.windll.kernel32.GetShortPathNameW(script, buffer, len(buffer))
                assert 0 < length < len(buffer), 'cannot resolve Windows short path'
                script = buffer.value
            with (root/'launcher-test.log').open('w') as log:
                result = subprocess.run([PS, '-NoProfile', '-ExecutionPolicy', 'Bypass', '-File', script, '-NoBrowser', '-Port', str(port), '-ProxyPort', '0', *extra], env=env, stdout=log, stderr=subprocess.STDOUT, timeout=90)
            assert (result.returncode == 0) == ok, (root/'launcher-test.log').read_text(errors='replace')
            return result

        def instances():
            code = f"@(Get-CimInstance Win32_Process -Filter \"Name='prtseek.exe'\" | Where-Object {{$_.ExecutablePath -eq {quote(root/'.dev/prtseek.exe')} -or $_.ExecutablePath -eq {quote(root/'prtseek.exe')}}} | ForEach-Object {{$_.ProcessId}}) | ConvertTo-Json -Compress"
            result = subprocess.check_output([PS, '-NoProfile', '-Command', code], text=True).strip()
            if not result:
                return []
            value = json.loads(result)
            return value if isinstance(value, list) else [value]

        def stop():
            # Select by exact test-installation path again before any termination.
            code = f"Get-CimInstance Win32_Process -Filter \"Name='prtseek.exe'\" | Where-Object {{$_.ExecutablePath -eq {quote(root/'.dev/prtseek.exe')} -or $_.ExecutablePath -eq {quote(root/'prtseek.exe')}}} | ForEach-Object {{Stop-Process -Id $_.ProcessId; Wait-Process -Id $_.ProcessId -Timeout 15 -ErrorAction SilentlyContinue}}"
            subprocess.run([PS, '-NoProfile', '-Command', code], check=True, capture_output=True)

        def health():
            with urllib.request.urlopen(f'http://127.0.0.1:{port}/api/health', timeout=3) as response:
                return json.load(response)

        try:
            launch(short_path=True)
            assert health()['build_id'] == build['source_id']
            first = instances()
            assert len(first) == 1, f'Expected one instance under {root}, got {first}'
            launch()
            assert instances() == first, 'duplicate start created a second process'
            (root/'generation').write_text('2')
            launch(ok=False)
            assert instances() == first, 'ordinary launch silently replaced a stale running instance'
            (root/'fail-build').touch()
            launch('-Update', ok=False)
            assert instances() == first and health()['ok'], 'failed build stopped the old service'
            (root/'fail-build').unlink()
            launch('-Update')
            second = instances()
            assert len(second) == 1 and second != first
            assert (root/'.dev/prtseek.exe.previous').exists()
            stop()
            # A different process owning the requested port must be left alone.
            with socket.socket() as foreign:
                foreign.bind(('127.0.0.1', port)); foreign.listen()
                launch('-Update', ok=False)
                assert not instances()
                assert foreign.getsockname()[1] == port
            # A release install must work without source/build tooling.
            (root/'go.mod').unlink()
            shutil.copy2(binary, root/'prtseek.exe')
            manifest = dict(build, binary_sha256=hashlib.sha256(binary.read_bytes()).hexdigest())
            (root/'prtseek.exe.build.json').write_text(json.dumps(manifest))
            (root/'node.cmd').write_text('@echo off\nexit /b 99\n')
            launch()
            assert health()['ok'] and len(instances()) == 1
            launch()
            print('PASS: short-path launch, source rebuild, root exe precedence, reuse, stale service refusal, build-failure preservation, explicit update, foreign port, release launch')
        finally:
            stop()


if __name__ == '__main__':
    main()
