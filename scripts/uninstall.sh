#!/usr/bin/env bash
# Removes everything SSXNG installed on this machine:
#   * the setuid helper in /Library/Application Support/SSXNG
#   * the proxy state snapshot of the helper
#   * the login item LaunchAgent
#   * optionally, the user configuration in ~/Library/Application Support/SSXNG
#
# The system proxy is cleared first (through the helper, while it still exists).
set -euo pipefail

HELPER_DIR="/Library/Application Support/SSXNG"
HELPER="${HELPER_DIR}/ssxng-helper"
CONFIG_DIR="${HOME}/Library/Application Support/SSXNG"
AGENT="${HOME}/Library/LaunchAgents/org.ssxng.SSXNG.plist"

echo "==> 先关闭系统代理"
if [ -x "$HELPER" ]; then
  for svc in $(/usr/sbin/networksetup -listallnetworkservices 2>/dev/null | tail -n +2); do
    "$HELPER" --mode off --network-service "$svc" >/dev/null 2>&1 || true
  done
  echo "    完成"
else
  echo "    助手不存在，跳过（若系统代理仍指向 127.0.0.1，请在系统设置里手动关闭）"
fi

echo "==> 删除助手（需要管理员密码）"
if [ -e "$HELPER_DIR" ]; then
  osascript -e "do shell script \"rm -rf '${HELPER_DIR}'\" with administrator privileges"
fi

echo "==> 删除登录项"
if [ -e "$AGENT" ]; then
  launchctl unload -w "$AGENT" 2>/dev/null || true
  rm -f "$AGENT"
fi

if [ "${1:-}" = "--purge-config" ]; then
  echo "==> 删除用户配置 ${CONFIG_DIR}"
  rm -rf "$CONFIG_DIR"
fi

echo "完成。"
