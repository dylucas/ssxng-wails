#!/usr/bin/env bash
# Builds everything: the privileged helper, then the app bundle with the helper
# embedded.
#
#   scripts/build.sh            # release build into build/bin/SSXNG.app
#   SSXNG_VERSION=1.2.3 scripts/build.sh
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

HELPER_BIN="internal/helperbin/bin/ssxng-helper"
APP_NAME="SSXNG"
APP_DIR="build/bin/${APP_NAME}.app"
VERSION="${SSXNG_VERSION:-1.0.0}"

WAILS="$(command -v wails || true)"
if [ -z "$WAILS" ]; then
  WAILS="$(go env GOPATH)/bin/wails"
fi
if [ ! -x "$WAILS" ]; then
  echo "找不到 wails 命令，请先安装：go install github.com/wailsapp/wails/v2/cmd/wails@v2.13.0" >&2
  exit 1
fi

echo "==> 构建特权助手（cmd/ssxng-helper）"
mkdir -p "$(dirname "$HELPER_BIN")"
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o "$HELPER_BIN" ./cmd/ssxng-helper
# Ad-hoc sign so that Gatekeeper and our own installer accept it.
codesign --force --sign - --identifier org.ssxng.ssxng-helper "$HELPER_BIN" >/dev/null
echo "    $(ls -l "$HELPER_BIN" | awk '{print $5}') 字节，签名: $(codesign -dv "$HELPER_BIN" 2>&1 | grep -i 'Signature' || echo 'adhoc')"

echo "==> 构建应用（wails build）"
"$WAILS" build -clean -platform darwin/arm64 -o "$APP_NAME"

if [ ! -d "$APP_DIR" ]; then
  echo "构建产物缺失: $APP_DIR" >&2
  exit 1
fi

echo "==> 签名应用"
codesign --force --sign - --identifier org.ssxng.SSXNG "$APP_DIR" >/dev/null
codesign --verify --deep --strict "$APP_DIR" && echo "    签名校验通过"

echo
echo "构建完成: $APP_DIR"
echo "安装助手（首次使用前必须执行一次）:"
echo "    打开 App 后点击「安装助手」，或手动执行 scripts/install_helper.sh"
