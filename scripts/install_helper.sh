#!/bin/sh
# Install the SSXNG privileged helper.
#
#   ./scripts/install_helper.sh <path-to-ssxng-helper>
#
# The GUI does the same thing from inside the app with a single administrator
# prompt. Run this by hand if you prefer to install it yourself, or if the
# app's prompt is unavailable.
set -eu

HELPER="${1:-}"
if [ -z "$HELPER" ]; then
    echo "用法: $0 <path-to-ssxng-helper>" >&2
    echo "提示: make helper 会构建到 internal/helperbin/bin/ssxng-helper" >&2
    exit 2
fi
if [ ! -f "$HELPER" ]; then
    echo "找不到助手文件: $HELPER" >&2
    exit 2
fi

DEST_DIR="/Library/Application Support/SSXNG"
DEST="$DEST_DIR/ssxng-helper"

# A setuid binary must be signed on Apple Silicon, so verify before installing.
if ! /usr/bin/codesign --verify --verbose=0 "$HELPER" 2>/dev/null; then
    echo "助手没有有效签名，正在用 ad-hoc 签名补上..." >&2
    /usr/bin/codesign --force --sign - "$HELPER"
fi

sudo mkdir -p "$DEST_DIR"
sudo cp -f "$HELPER" "$DEST"
sudo chown root:admin "$DEST"
sudo chmod 4755 "$DEST"

echo "已安装: $DEST"
"$DEST" --version
