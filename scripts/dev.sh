#!/usr/bin/env bash
# Development mode: rebuild the helper, then run the app with Wails' dev server.
# The asset server listens on http://localhost:34115 so the UI can also be
# inspected in a normal browser.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

WAILS="$(command -v wails || echo "$(go env GOPATH)/bin/wails")"
if [ ! -x "$WAILS" ]; then
  echo "找不到 wails 命令，请先安装：go install github.com/wailsapp/wails/v2/cmd/wails@v2.13.0" >&2
  exit 1
fi

echo "==> 构建特权助手"
mkdir -p internal/helperbin/bin
CGO_ENABLED=0 go build -trimpath -o internal/helperbin/bin/ssxng-helper ./cmd/ssxng-helper
codesign --force --sign - --identifier org.ssxng.ssxng-helper internal/helperbin/bin/ssxng-helper >/dev/null

echo "==> wails dev"
# 注意：wails v2 的 dev 子命令没有 -platform 参数；前端热重载依赖
# wails.json 里的 "assetdir"（只影响 dev，release 仍使用 go:embed）。
exec "$WAILS" dev
