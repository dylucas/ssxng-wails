#!/usr/bin/env bash
# 界面自检：用真实的前端文件（index.html / main.css / app.js / i18n.js）在
# 无头 Chromium 里渲染，检查标题栏与拖动区几何、横向溢出、文字对比度、
# 六个视图是否渲染出内容。
#
#   scripts/ui-check.sh              # 检查
#   SSXNG_UI_VERBOSE=1 ...           # 打印每一条通过项
#   SSXNG_UI_SHOTS=/some/dir ...     # 截图输出目录（默认 /tmp/ssxng-ui-shots）
#
# 需要的浏览器取自 Playwright 的缓存，不需要安装任何 npm 依赖。
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SRC="$ROOT/frontend/src"
CHECK="$ROOT/scripts/ui-check"

CHROME="${SSXNG_UI_CHROME:-}"
if [ -z "$CHROME" ]; then
  CHROME="$(ls -1 "$HOME"/Library/Caches/ms-playwright/chromium_headless_shell-*/chrome-headless-shell-mac-arm64/chrome-headless-shell 2>/dev/null | tail -1 || true)"
fi
if [ -z "$CHROME" ] || [ ! -x "$CHROME" ]; then
  echo "跳过界面自检：找不到 chrome-headless-shell。" >&2
  echo "可用 npx playwright install chromium-headless-shell 安装，或用 SSXNG_UI_CHROME 指定路径。" >&2
  exit 0
fi

if ! command -v node >/dev/null 2>&1; then
  echo "跳过界面自检：找不到 node。" >&2
  exit 0
fi

TMP="$(mktemp -d /tmp/ssxng-uicheck-XXXXXX)"
cleanup() { rm -rf "$TMP"; }
trap cleanup EXIT

cp "$SRC/main.css" "$SRC/app.js" "$SRC/i18n.js" "$TMP/"
cp "$CHECK/stub.js" "$CHECK/metrics.js" "$TMP/"

# 把 index.html 改造成可直接用 file:// 打开的 harness：
#   - 绝对路径 /main.css 等换成相对路径
#   - 在 deferred 脚本之前注入 Wails 运行时桩
#   - 在 app.js 之后注入度量脚本
python3 - "$SRC/index.html" "$TMP/harness.html" <<'PY'
import sys

src, dst = sys.argv[1], sys.argv[2]
html = open(src, encoding='utf-8').read()
for asset in ('main.css', 'i18n.js', 'app.js'):
    html = html.replace('"/%s"' % asset, '"./%s"' % asset)
html = html.replace('<script defer src="./i18n.js"></script>',
                    '<script src="./stub.js"></script>\n<script defer src="./i18n.js"></script>')
html = html.replace('<script defer src="./app.js"></script>',
                    '<script defer src="./app.js"></script>\n<script defer src="./metrics.js"></script>')
assert './stub.js' in html, '注入 stub.js 失败'
assert './metrics.js' in html, '注入 metrics.js 失败'
assert 'href="/' not in html and 'src="/' not in html, '仍有绝对路径资源引用'
open(dst, 'w', encoding='utf-8').write(html)
PY

export SSXNG_UI_SHOTS="${SSXNG_UI_SHOTS:-/tmp/ssxng-ui-shots}"
mkdir -p "$SSXNG_UI_SHOTS"
node "$CHECK/measure.js" "$TMP" "$CHROME"
