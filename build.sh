#!/usr/bin/env bash
# prtseek cross-platform release builder.
#
# The default mode builds one target and embeds the already-exported frontend.
# `./build.sh --release` builds and packages all supported desktop/server targets.
#
# Environment variables:
#   prtseek_TARGET_OS=linux             One target OS in single-target mode.
#   prtseek_TARGET_ARCH=amd64           One target arch in single-target mode.
#   prtseek_TARGETS=linux/amd64,...     Comma-separated targets for multi-target mode.
#   prtseek_BUILD_VERSION=prtseek-v1.3.0        Version embedded in the binary and archive name.
#   prtseek_OUTPUT=/path/to/prtseek       Explicit binary path in single-target mode.
#   prtseek_OUTPUT_DIR=dist             Directory for default binary paths.
#   prtseek_PACKAGE=1                   Create a zip archive for each target.
#   prtseek_PACKAGE_DIR=dist            Directory for release archives.
#   prtseek_COMPRESS=off                UPX mode: off, auto, or required.
#   prtseek_UPX_ARGS="--best --lzma"    Arguments passed to UPX.
#   prtseek_SKIP_FRONTEND=1             Reuse server/webui/dist (for CI artifact builds).
#   prtseek_SKIP_NPM_CI=1               Skip npm ci while rebuilding the frontend.
#   prtseek_GOSUMDB=sum.golang.org      Go checksum database.
set -euo pipefail

cd "$(cd "$(dirname "$0")" && pwd)"

info() { printf '\033[36m[*]\033[0m %s\n' "$*"; }
ok() { printf '\033[32m[+]\033[0m %s\n' "$*"; }
warn() { printf '\033[33m[!]\033[0m %s\n' "$*" >&2; }
die() { printf '\033[31m[x]\033[0m %s\n' "$*" >&2; exit 1; }

usage() {
  cat <<'EOF'
사용법:
  ./build.sh                         현재 시스템, 현재 아키텍처로 컴파일
  ./build.sh --target linux/amd64   지정한 대상 하나를 컴파일
  ./build.sh --release               지원하는 모든 대상을 컴파일하고 패키징

옵션:
  --release              Linux, macOS, Windows 의 amd64/arm64 대상을 빌드하고 zip 생성
  --target OS/ARCH       단일 대상 지정, 예: windows/amd64
  --upx                  UPX 로 바이너리를 강제로 압축(일부 Linux 환경 호환성에 영향을 줄 수 있음)
  --no-compress          UPX 를 쓰지 않고 Go linker 로만 줄인 뒤 zip 압축
  --help                 도움말 표시

다중 대상 목록은 prtseek_TARGETS 로 재정의할 수 있다. 예:
  prtseek_TARGETS=linux/amd64,windows/amd64 ./build.sh --release
EOF
}

RELEASE_TARGETS_DEFAULT="linux/amd64,linux/arm64,darwin/amd64,darwin/arm64,windows/amd64"
# prtseek_* takes precedence; ARTEX_* remains a compatibility fallback.
prtseek_TARGET_OS="${prtseek_TARGET_OS-${ARTEX_TARGET_OS-}}"
prtseek_TARGET_ARCH="${prtseek_TARGET_ARCH-${ARTEX_TARGET_ARCH-}}"
prtseek_TARGETS="${prtseek_TARGETS-${ARTEX_TARGETS-}}"
prtseek_BUILD_VERSION="${prtseek_BUILD_VERSION-${ARTEX_BUILD_VERSION-}}"
prtseek_OUTPUT="${prtseek_OUTPUT-${ARTEX_OUTPUT-}}"
prtseek_OUTPUT_DIR="${prtseek_OUTPUT_DIR-${ARTEX_OUTPUT_DIR-}}"
prtseek_PACKAGE="${prtseek_PACKAGE-${ARTEX_PACKAGE-}}"
prtseek_PACKAGE_DIR="${prtseek_PACKAGE_DIR-${ARTEX_PACKAGE_DIR-}}"
prtseek_COMPRESS="${prtseek_COMPRESS-${ARTEX_COMPRESS-}}"
prtseek_UPX_ARGS="${prtseek_UPX_ARGS-${ARTEX_UPX_ARGS-}}"
prtseek_SKIP_FRONTEND="${prtseek_SKIP_FRONTEND-${ARTEX_SKIP_FRONTEND-}}"
prtseek_SKIP_NPM_CI="${prtseek_SKIP_NPM_CI-${ARTEX_SKIP_NPM_CI-}}"
prtseek_GOSUMDB="${prtseek_GOSUMDB-${ARTEX_GOSUMDB-}}"
prtseek_RELEASE="${prtseek_RELEASE-${ARTEX_RELEASE-}}"
prtseek_RELEASE="${prtseek_RELEASE:-0}"
prtseek_COMPRESS="${prtseek_COMPRESS:-off}"
prtseek_PACKAGE="${prtseek_PACKAGE:-0}"

while [ "$#" -gt 0 ]; do
  case "$1" in
    --release)
      prtseek_RELEASE=1
      prtseek_PACKAGE=1
      shift
      ;;
    --target)
      [ "$#" -ge 2 ] || die "--target 에는 OS/ARCH 인자가 필요합니다"
      target_arg="$2"
      case "$target_arg" in
        */*)
          prtseek_TARGET_OS="${target_arg%%/*}"
          prtseek_TARGET_ARCH="${target_arg##*/}"
          prtseek_TARGETS="$target_arg"
          ;;
        *) die "대상은 OS/ARCH 형식이어야 합니다. 예: linux/amd64" ;;
      esac
      shift 2
      ;;
    --no-compress)
      prtseek_COMPRESS=0
      shift
      ;;
    --upx)
      prtseek_COMPRESS=required
      shift
      ;;
    --help|-h)
      usage
      exit 0
      ;;
    *) die "알 수 없는 인자: $1 (--help 로 사용법 확인)" ;;
  esac
done

command -v go >/dev/null 2>&1 || die "Go 를 찾을 수 없습니다(이 프로젝트는 Go 1.26 이상이 필요합니다)"

prtseek_GOSUMDB="${prtseek_GOSUMDB:-sum.golang.org}"
if [ -z "${prtseek_BUILD_VERSION:-}" ]; then
  prtseek_BUILD_VERSION="$(tr -d '\r\n' < VERSION)"
fi
# Every local build keeps the fork marker so upstream self-update stays disabled.
# Accept prtseek-v1.3.0, v1.3.0, 1.3.0, or a development revision.
prtseek_PACKAGE_VERSION="${prtseek_BUILD_VERSION#prtseek-}"
prtseek_PACKAGE_VERSION="${prtseek_PACKAGE_VERSION#v}"
case "$prtseek_PACKAGE_VERSION" in
  [0-9]*) prtseek_BUILD_VERSION="prtseek-v${prtseek_PACKAGE_VERSION}" ;;
  *) prtseek_BUILD_VERSION="prtseek-${prtseek_PACKAGE_VERSION}" ;;
esac
prtseek_OUTPUT_DIR="${prtseek_OUTPUT_DIR:-dist}"
prtseek_PACKAGE_DIR="${prtseek_PACKAGE_DIR:-$prtseek_OUTPUT_DIR}"
prtseek_UPX_ARGS="${prtseek_UPX_ARGS:---best --lzma}"

if [ "${prtseek_RELEASE}" = "1" ]; then
  prtseek_TARGETS="${prtseek_TARGETS:-$RELEASE_TARGETS_DEFAULT}"
else
  prtseek_TARGET_OS="${prtseek_TARGET_OS:-$(GOSUMDB="$prtseek_GOSUMDB" go env GOOS)}"
  prtseek_TARGET_ARCH="${prtseek_TARGET_ARCH:-$(GOSUMDB="$prtseek_GOSUMDB" go env GOARCH)}"
  prtseek_TARGETS="${prtseek_TARGETS:-${prtseek_TARGET_OS}/${prtseek_TARGET_ARCH}}"
fi

if [ "${prtseek_SKIP_FRONTEND:-0}" = "1" ]; then
  [ -d server/webui/dist ] || die "prtseek_SKIP_FRONTEND=1 이지만 server/webui/dist 가 없습니다"
else
  command -v npm >/dev/null 2>&1 || die "npm 을 찾을 수 없습니다(프론트엔드 정적 빌드에는 Node.js/npm 이 필요합니다)"
  command -v rsync >/dev/null 2>&1 || die "rsync 를 찾을 수 없습니다"
  info "프론트엔드 정적 리소스 빌드"
  if [ "${prtseek_SKIP_NPM_CI:-0}" != "1" ]; then
    (cd web && npm ci)
  fi
  (cd web && npm run build:static)
  info "프론트엔드 리소스를 server/webui/dist 로 동기화"
  mkdir -p server/webui/dist
  rsync -a --delete web/out/ server/webui/dist/
fi

compress_binary() {
  binary="$1"
  goos="$2"
  case "$prtseek_COMPRESS" in
    0|off|false|none)
      info "UPX 건너뜀: $binary"
      return 0
      ;;
    auto|required|true|1) ;;
    *) die "prtseek_COMPRESS 는 off, auto, required 중 하나여야 합니다" ;;
  esac

  if ! command -v upx >/dev/null 2>&1; then
    if [ "$prtseek_COMPRESS" = "required" ]; then
      die "prtseek_COMPRESS=required 이지만 upx 를 찾을 수 없습니다"
    fi
    warn "upx 를 찾을 수 없어 linker 압축 결과를 유지합니다: $binary"
    return 0
  fi

  before=$(wc -c < "$binary" | tr -d ' ')
  upx_args="$prtseek_UPX_ARGS"
  [ "$goos" = "darwin" ] && upx_args="$upx_args --force-macos"
  # shellcheck disable=SC2086
  if ! upx $upx_args -- "$binary"; then
    if [ "$prtseek_COMPRESS" = "required" ]; then
      die "UPX 압축 실패: $binary"
    fi
    warn "UPX 가 이 대상 형식을 지원하지 않아 압축하지 않은 바이너리를 유지합니다: $binary"
    return 0
  fi
  after=$(wc -c < "$binary" | tr -d ' ')
  ok "UPX 압축 완료: $binary (${before} -> ${after} bytes)"
}

package_binary() {
  binary="$1"
  goos="$2"
  goarch="$3"
  package_name="prtseek-${prtseek_PACKAGE_VERSION}-${goos}-${goarch}"
  package_root="${prtseek_PACKAGE_DIR}/${package_name}"
  archive="${prtseek_PACKAGE_DIR}/${package_name}.zip"

  command -v zip >/dev/null 2>&1 || die "패키징에는 zip 이 필요합니다"
  rm -rf "$package_root" "$archive"
  mkdir -p "$package_root"
  cp "$binary" "$package_root/"
  # 대상 시스템에 맞는 감시 시작 스크립트를 함께 넣는다. 개인 빌드는 prtseek 버전 표시를 유지하고 상위 프로젝트 자동 업데이트를 비활성화한다.
  if [ "$goos" = "windows" ]; then
    cp start.bat start-prtseek.cmd start-prtseek.ps1 update-prtseek.cmd "$package_root/"
  else
    cp start.sh "$package_root/"
    chmod +x "$package_root/start.sh"
  fi
  cp -R skills "$package_root/"
  cp "$binary.build.json" "$package_root/"
  cp VERSION "$package_root/"
  cp config.example.json "$package_root/"
  if [ -f README.md ]; then cp README.md "$package_root/"; fi
  cp LICENSE "$package_root/"
  (cd "$prtseek_PACKAGE_DIR" && zip -q -r -9 "$(basename "$archive")" "$(basename "$package_root")")
  rm -rf "$package_root"
  ok "Release 압축 패키지: $archive"
}

build_target() {
  target="$1"
  case "$target" in
    */*) ;;
    *) die "유효하지 않은 대상: $target (OS/ARCH 형식이어야 합니다)" ;;
  esac
  goos="${target%%/*}"
  goarch="${target##*/}"
  case "$goos" in
    linux|darwin|windows) ;;
    *) die "지원하지 않는 시스템: $goos (linux, darwin, windows 지원)" ;;
  esac

  binary_name="prtseek"
  [ "$goos" = "windows" ] && binary_name="prtseek.exe"
  if [ -n "${prtseek_OUTPUT:-}" ] && [ "$prtseek_RELEASE" != "1" ]; then
    output="$prtseek_OUTPUT"
  else
    output="${prtseek_OUTPUT_DIR}/prtseek-${goos}-${goarch}/${binary_name}"
  fi
  mkdir -p "$(dirname "$output")"

  info "${goos}/${goarch} 컴파일, 버전 ${prtseek_BUILD_VERSION}"
  GOSUMDB="$prtseek_GOSUMDB" \
  CGO_ENABLED=0 \
  GOOS="$goos" \
  GOARCH="$goarch" \
  go build \
    -buildvcs=false \
    -tags embedui \
    -trimpath \
    -ldflags "-s -w -buildid= -X main.version=${prtseek_BUILD_VERSION}" \
    -o "$output" \
    ./cmd/prtseek

  compress_binary "$output" "$goos"
  node scripts/build-state.mjs write "$output"
  if command -v file >/dev/null 2>&1; then file "$output"; fi
  if [ "$prtseek_PACKAGE" = "1" ]; then package_binary "$output" "$goos" "$goarch"; fi
  ok "컴파일 완료: $output"
}

write_checksums() {
  [ "$prtseek_PACKAGE" = "1" ] || return 0
  checksum_file="$prtseek_PACKAGE_DIR/SHA256SUMS"
  if command -v sha256sum >/dev/null 2>&1; then
    (cd "$prtseek_PACKAGE_DIR" && for archive in *.zip; do sha256sum "$archive"; done > "$(basename "$checksum_file")")
  elif command -v shasum >/dev/null 2>&1; then
    (cd "$prtseek_PACKAGE_DIR" && for archive in *.zip; do shasum -a 256 "$archive"; done > "$(basename "$checksum_file")")
  else
    warn "sha256sum 이나 shasum 을 찾을 수 없어 SHA256SUMS 를 건너뜁니다"
    return 0
  fi
  ok "검증 파일: $checksum_file"
}

mkdir -p "$prtseek_OUTPUT_DIR"
if [ "$prtseek_PACKAGE" = "1" ]; then mkdir -p "$prtseek_PACKAGE_DIR"; fi

old_ifs="$IFS"
IFS=','
read -r -a targets <<< "$prtseek_TARGETS"
IFS="$old_ifs"
[ "${#targets[@]}" -gt 0 ] || die "prtseek_TARGETS 는 비워 둘 수 없습니다"
for target in "${targets[@]}"; do
  target="${target//[[:space:]]/}"
  [ -n "$target" ] || continue
  build_target "$target"
done

if [ "$prtseek_PACKAGE" = "1" ]; then
  write_checksums
  info "Release 패키지 생성 위치: $prtseek_PACKAGE_DIR"
fi
