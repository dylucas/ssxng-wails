# SSXNG

面向 **Apple Silicon（M 系列）macOS** 的 Shadowsocks 客户端，使用 **Wails + Go** 实现，
功能对照 [ShadowsocksX-NG](https://github.com/shadowsocks/ShadowsocksX-NG) 重新实现。

界面是原生 WebView + 纯 HTML/CSS/JS（无 npm、无打包器），代理协议来自
[go-shadowsocks2](https://github.com/shadowsocks/go-shadowsocks2)，PAC 模板沿用
ShadowsocksX-NG 的 gfwlist2pac/abp.js。

```
┌──────────────────────────────────────────────┐
│  Wails (macOS WebView)  frontend/src/*.js    │
│        ▲ 事件 ssxng:state/log/traffic/toast  │
│        ▼ window.go.main.App.<Method>()       │
│  app*.go / apply.go       菜单栏图标 (Cocoa)  │
│  ┌──────────────┬─────────────────────────┐  │
│  │ sscore       │ pac / config / proxyconf│  │
│  │ SOCKS5+HTTP  │ PAC 服务器、配置、系统代理│  │
│  └──────┬───────┴───────────┬─────────────┘  │
│         │ AEAD 加密隧道      │ argv 调用       │
└─────────┼───────────────────┼────────────────┘
          ▼                   ▼
   Shadowsocks 服务器   setuid 助手 ssxng-helper
                        → /usr/sbin/networksetup
```

## 功能

**已实现（与 ShadowsocksX-NG 对等）**

- SOCKS5（默认 `127.0.0.1:1086`）与 HTTP 代理（默认 `127.0.0.1:1087`），纯 Go 实现，
  不依赖 privoxy；HTTP 代理同时支持绝对形式请求与 `CONNECT` 隧道。
- 加密方式：`aes-128-gcm`、`aes-192-gcm`、`aes-256-gcm`、`chacha20-ietf-poly1305`（SIP004 AEAD）。
- 四种模式 + 关闭：**自动代理（PAC）**、**全局代理**、**手动模式**、**外部 PAC**，以及 **关闭**；
  可逐个模式在设置里隐藏，托盘/菜单栏可在启用的模式间切换。
- 内置 PAC 服务器（默认 `127.0.0.1:1089/proxy.pac`）：下载 gfwlist → base64 解码 →
  合并 `user-rule.txt` → 生成 PAC（`__RULES__` 内嵌规则数组，ABP 语义由 abp.js 执行）。
  下载失败不会覆盖已有规则；PAC URL 带 `?v=<时间戳>` 缓存击穿参数（兼容外部 PAC 客户端）。
- **UDP 转发**：SOCKS5 `UDP ASSOCIATE`，与 SOCKS 共用端口（默认开启），QUIC/DNS/WebRTC 这类
  走 UDP 的应用因此也能走代理；一客户端一条加密会话，多目标复用，空闲 60 秒回收。
- 服务器管理：新增/编辑/删除/排序/选中、`ss://` 导入导出（SIP002）、连通性测试。
- **命令行代理**：「模式与代理」页给出 HTTP 与 SOCKS5 两条
  `export http_proxy=…;export https_proxy=…;`，一键复制到剪贴板；菜单栏图标里也有
  "复制 HTTP 代理命令 / 复制 SOCKS5 代理命令"两项，窗口收起来时可直接复制（由 Cocoa 直接写
  剪贴板，不依赖窗口是否打开）。两处内容一致，端口都取自已保存的监听设置。
- 系统代理通过 **setuid 助手** 调用 `networksetup` 完成：改动前把原有代理设置快照到
  `/Library/Application Support/SSXNG/proxy-state.json`，关闭时按快照恢复（含 bypass 列表）。
  助手只接受白名单参数、只允许 root+admin 执行。
- 菜单栏图标（无 Dock 图标模式；代理开启时是实心纸飞机、关闭时是空心，一眼可辨）、
  开机自启（LaunchAgent）、流量统计、连接日志、日志文件、
  诊断页 + **内置端到端自检**（本机起临时 Shadowsocks 服务端，真实跑一遍
  SOCKS5/HTTP/CONNECT/UDP ASSOCIATE）。

**未实现（有意不做）**

| 项目 | 说明 |
| --- | --- |
| DNS 强制策略 | UDP 走代理与否取决于应用本身；浏览器自带 DoH 时可能绕过代理，本版本不做强制 |
| Shadowsocks 2022 (`2022-blake3-*`) | 需要 SIP022；本版本只支持 AEAD 经典协议 |
| 旧式流加密（`rc4-md5`、`aes-256-cfb`…） | 已不安全，go-shadowsocks2 也不再提供 |
| `xchacha20-ietf-poly1305` | 未被主流客户端广泛支持，避免"能选却连不上" |
| SIP003 插件（v2ray-plugin 等） | 需外部二进制，本版本不管理插件进程 |
| 订阅链接、二维码、规则分流（acl） | 本版本不做 |
| 命令行工具 | 只有 GUI 与内部助手；终端程序可复制「模式与代理」页的 export 命令接入 |

## 环境要求

- macOS 13+，Apple Silicon（arm64）
- Go 1.23+，Xcode Command Line Tools（`xcode-select --install`）
- Wails CLI v2.13：
  `go install github.com/wailsapp/wails/v2/cmd/wails@v2.13.0`

## 构建

```bash
make build          # 等价于 scripts/build.sh：先编译助手，再打包 App
open build/bin/SSXNG.app
```

> `wails dev` 与 `wails build` 写的是同一个 `build/bin`：别在 dev 还开着的时候跑
> `make build`，`wails dev` 退出时的清理会把刚构建出来的可执行文件一起删掉。

改界面（`frontend/src/` 下的 html/css/js）时用 `make dev`：`wails.json` 里的
`"assetdir": "frontend/src"` 让 dev 直接读磁盘并热重载，改完立刻能在窗口里看到效果
（该字段只影响 `wails dev`，发布产物仍然是 `//go:embed frontend/src`）。改完记得
`make build` 重新打包，否则 `build/bin/SSXNG.app` 里还是旧界面。

其它入口：

```bash
make helper         # 只编译并 ad-hoc 签名 ssxng-helper
make test           # 单元测试（不触碰系统设置）
make vet            # go vet
make dev            # wails dev（http://localhost:34115 可在浏览器里调后端）
make run            # 构建并启动
make uninstall      # 卸载助手与登录项；PURGE=1 时连用户配置一起删除
```

## 首次使用

1. 启动 `SSXNG.app`。
2. 打开「状态」页 → **安装助手**：会弹出一次管理员授权（`osascript ... with administrator privileges`），
   把 `ssxng-helper` 安装到 `/Library/Application Support/SSXNG/`，属主 `root:admin`、权限 `4755`。
   不安装助手也能用：只是无法自动改系统代理，需要自己把系统代理指向 `127.0.0.1:1086`。
3. 「服务器」页添加节点（或粘贴 `ss://` 链接导入），选中。
4. 「PAC」页点击 **从 GFWList 更新**（首次会从 `raw.githubusercontent.com` 下载规则，
   可在设置里改用 jsDelivr 镜像）。
5. 「状态」页打开开关，或从菜单栏图标切换模式。

## 文件位置

| 路径 | 内容 |
| --- | --- |
| `~/Library/Application Support/SSXNG/config.json` | 设置 |
| `~/Library/Application Support/SSXNG/servers.json` | 服务器列表（含密码，权限 0600） |
| `~/Library/Application Support/SSXNG/gfwlist.txt` | 下载的 gfwlist（base64） |
| `~/Library/Application Support/SSXNG/user-rule.txt` | 用户自定义规则（ABP 语法） |
| `~/Library/Application Support/SSXNG/gfwlist.js` | 生成的 PAC 文件 |
| `~/Library/Application Support/SSXNG/ssxng.log` | 日志（超过 2 MiB 自动截断） |
| `/Library/Application Support/SSXNG/ssxng-helper` | setuid 助手 |
| `/Library/Application Support/SSXNG/proxy-state.json` | 改动前的系统代理快照 |

设 `SSXNG_CONFIG_DIR` 可把配置目录改到别处（调试、多份配置时有用）。

## 验证

```bash
go test ./...        # 单元 + 集成测试（本机起 Shadowsocks 服务端跑真实流量）
```

```bash
scripts/ui-check.sh   # 界面自检：无头 Chromium 渲染真实前端文件后做几何与对比度断言
scripts/tray-check.sh # 菜单栏自检：无界面编译真实的 tray_darwin.m，断言复制命令菜单项
```

界面自检会把 `frontend/src/{index.html,main.css,i18n.js,app-core.js,app-views.js,app-actions.js,app.js}`
装进一个注入 Wails 运行时桩的临时页面，用本机 Playwright 缓存的 `chrome-headless-shell`
（无 npm 依赖）渲染，然后断言：
窗口标题栏/拖动区几何（品牌与视图内容都在标题栏之下）、980/880/700 三档宽度下无横向溢出、
深浅两套主题里所有文字元素的前景/背景对比度（正文 ≥ 4.5:1、大字 ≥ 3:1）、6 个视图都渲染出内容、
`main.css` 里没有硬编码颜色且所有 `var(--x)` 都有定义。除此之外还有**版面密度护栏**：
"状态页 ≤ 820px、模式与代理页 ≤ 1450px 内容高（基线分别是 970 / 1498）"、
"模式与代理页必须保持单列"、"模式单选的说明在窄窗下仍与标题同行"、"8 个节点时服务器页
700px ≤1200px / 980px ≤760px、单行 ≤80px、每行 4 个动作 + 1 个更多"——密度是最容易被下一次
改动悄悄退回去的东西，所以让它在自检里报错。截图落在 `/tmp/ssxng-ui-shots/`。

自检还会**真的点开**"更多"菜单，验证它出现/含 3 项/`aria-expanded` 正确/点空白与 Escape 收起，
以及诊断页点"运行检查"后表格确实渲染出行且无横向溢出。（`scripts/ui-check/stub.js` 的返回形状
必须与 `internal/api/types.go`、`app*.go` 一致——写错会被前端的容错解析静默吞掉，让整块内容
永远不被检查。）自检还会静态校验前端四个脚本的接线：加载顺序必须是
`i18n → core → views → actions → app`、每个脚本各自一个 IIFE（不泄漏全局、不用 ES 模块）、
每个 `const {...} = S` 里的名字都由更早加载的文件导出、每处 `S.name` 都有对应的导出，
并且 `stub.js` 的 fixture 字段与 `internal/api/types.go` 的 JSON tag 一一对应。
找不到 `chrome-headless-shell` 时脚本会提示并跳过（不算失败），也可以用 `SSXNG_UI_CHROME` 指定。

菜单栏自检把真实的 `internal/tray/tray_darwin.m`（连同它依赖的 Cocoa 框架）编译成一个无界面
小程序——不创建 `NSStatusBar`，因此 SSH / 无图形会话下也能跑——然后断言"复制命令行代理"
菜单项的条数、标题、`representedObject`（点击后会写进剪贴板的文字）、`copyCommand:` 动作与
目标对象，外加空 spec / 坏行 / 命令含 `|` 的边界。它还断言菜单栏图标的开关形态：实心/空心
两张图不同、都是 18×18 的 template、**纸飞机重心处"开启不透明、关闭透明"**、轮廓中点都有
笔画（空心不等于空白）、以及图标被缓存。原生菜单的失效方式是静默的（标题空、复制出错位的
文字、图标没跟着开关走），界面自检看不见这一层，所以单独钉住。命令文本必须与窗口里显示的
逐字一致，`tray_test.go`、`measure.js`、这个脚本各自钉住同一串字面量。

想亲眼看图标长什么样，可以让它导出 PNG（含深浅两种菜单栏底色的 6× 预览）：

```bash
SSXNG_TRAY_ICON_OUT=/tmp/ssxng-icons scripts/tray-check.sh
```

应用内：「诊断」页的 **自检** 会临时启动一个本机 Shadowsocks 服务端与 HTTP 回显服务，
用自己的 SOCKS5/HTTP 前端各跑一遍请求、CONNECT 隧道和 256 KiB 数据校验，然后释放端口；
「诊断」页的静态检查会逐项报告配置目录、服务器可达性、端口占用、助手状态、PAC 规则数、
系统代理实际读回值。

## 已知偏差与限制

完整的对照表见 [DESIGN.md](DESIGN.md#与-shadowsocksx-ng-的差异)，要点：

- 配置目录改为 `~/Library/Application Support/SSXNG/`，不使用 `NSUserDefaults`。
- 自动代理模式**不改动**你已有的 bypass 列表；全局模式会把它设置为本应用的例外列表，
  关闭时按快照恢复。
- 开机自启用 LaunchAgent 实现，不再内置 SMLoginItem 辅助 App。
- 只监听回环地址。UDP 转发与 SOCKS 共用同一个回环端口（TCP/UDP 是两套命名空间），
  因此不需要额外配置；不支持旧式流加密与 SS2022。
- 修改系统代理需要 setuid 助手，这是 macOS 上唯一的免重复弹窗方案（与 SSX-NG 一致）。

## 许可

GPL-3.0-or-later（见 [LICENSE](LICENSE)）。原因是 PAC 模板 `internal/pac/abp.js` 来自
gfwlist2pac / Adblock Plus，采用 GPL-3.0；ShadowsocksX-NG 同样因此选择 GPL-3.0。
`github.com/shadowsocks/go-shadowsocks2` 为 Apache-2.0，与 GPL-3.0 兼容。

## 致谢

- [ShadowsocksX-NG](https://github.com/shadowsocks/ShadowsocksX-NG)：功能与行为参照。
- [go-shadowsocks2](https://github.com/shadowsocks/go-shadowsocks2)：AEAD 协议与 SOCKS 编解码。
- [gfwlist2pac](https://github.com/clowwindy/gfwlist2pac) / [Adblock Plus](https://adblockplus.org/)：PAC 模板。
- [gfwlist](https://github.com/gfwlist/gfwlist)：规则数据。
- [Wails](https://wails.io/)：桌面外壳。
