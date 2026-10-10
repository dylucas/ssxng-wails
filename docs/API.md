# SSXNG 前端契约（冻结版 v2）

Wails v2 + plain 模板。**没有打包器、没有 npm、没有网络请求**：`frontend/src` 由 Go 直接 `//go:embed`，
Wails 的 asset server 把它挂在 `/`（`index.html` → `/`，`main.css` → `/main.css`）。
Wails 会自动把 `window.go` 与 `window.runtime` 注入到页面里，**直接使用全局对象，不要 import**。

## 1. 调用后端

```js
// 所有方法都返回 Promise；出错时 reject，可用 catch 拿到中文错误信息
const st = await window.go.main.App.GetState();
await window.go.main.App.SetMode('global');
```

## 2. 事件（后端主动推送）

```js
window.runtime.EventsOn('ssxng:state',   (state)  => { /* api.State */ });
window.runtime.EventsOn('ssxng:logs',    (entries)=> { /* api.LogEntry[] */ });
window.runtime.EventsOn('ssxng:traffic', (traffic)=> { /* api.Traffic */ });
window.runtime.EventsOn('ssxng:toast',   (toast)  => { /* {level, message} */ });
```

`ssxng:state` 在启动时以及任何状态变化后触发（这是 UI 的唯一真相来源，收到就整页重渲染）。
`ssxng:traffic` 每秒一次。

`ssxng:logs` 是**成批**推送的：后端把日志行合并到约 100ms 一个批次里（单批最多 256 条），
而不是每写一行就发一次事件。代理每建立一条连接都会写日志，逐条推送等于每条连接一次
JSON 序列化 + 一次 IPC，所以这里换成了批量。

由此产生两条前端的硬性约定：

1. **必须按 `seq` 去重**，不能假设"每批都是新的、有序的"。批次内部混有重复行或迟到的旧行
   时，水位线（`App.logSeq`）要能挡住它们；`seq <= logSeq` 的行一律丢弃。
2. **必须容忍缺行**。订阅缓冲（1024 条）满时后端会丢弃推送，但环形缓冲与日志文件仍是完整的，
   `GetLogs(afterSeq)` 就是补齐手段：切到日志页、改日志等级过滤时都要带水位线拉一次增量。

> v1 → v2：`ssxng:log`（单条）被 `ssxng:logs`（批量）取代。前后端在同一个二进制里一起发布，
> 没有需要兼容的旧前端。

## 3. 方法签名

| 方法 | 参数 | 返回 |
|---|---|---|
| `GetState()` | – | `State` |
| `SetEnabled(on)` | bool | – |
| `SetMode(mode)` | `'auto'\|'global'\|'manual'\|'externalPAC'\|'off'` | – |
| `AddServer(view)` | `ServerView` | `ServerView`（含生成的 `id`） |
| `UpdateServer(view)` | `ServerView` | – |
| `DeleteServer(id)` | string | – |
| `SelectServer(id)` | string | – |
| `MoveServer(id, delta)` | string, number | – |
| `ImportSSURL(raw)` | string | `ServerView` |
| `ExportSSURL(id)` | string | string |
| `GetLogs(afterSeq)` | number | `LogEntry[]`（`afterSeq<=0` 时返回最近 500 条；否则只返回 `seq > afterSeq` 的行，用于补齐被丢弃的批次） |
| `ClearLogs()` | – | – |
| `ListNetworkServices()` | – | `NetworkService[]` |
| `InstallHelper()` | – | – （会弹出 macOS 管理员授权对话框，耗时几秒到几十秒） |
| `HelperStatus()` | – | `HelperStatus` |
| `UpdatePACFromGFWList()` | – | `PACView` |
| `PACStatus()` | – | `PACView` |
| `OpenUserRuleFile()` | – | – （Finder 中显示 user-rule.txt） |
| `OpenConfigFolder()` | – | – |
| `TestServer(id)` | string | – （TCP 连通性测试，结果进日志与 toast） |
| `SelfTest()` | – | `SelfTestReport`（本地起 SS 服务端跑通全链路，可能耗时数秒） |
| `Diagnose()` | – | `Check[]` |
| `SaveSettings(view)` | `SettingsView` | – |
| `ApplySettings(view)` | `SettingsView` | – （保存并立即生效：重开监听/重设系统代理） |
| `SetLaunchAtLogin(on)` | bool | – |
| `SetHideDockIcon(on, ask)` | bool, bool | – |
| `Quit()` | – | – （真正退出，会按设置清理系统代理） |
| `ShowWindow()` | – | – （托盘“显示主窗口”用；窗口隐藏时恢复并取消最小化） |

**重要**：`ServerView.password` 会回传给前端（用户要能编辑），但前端**绝不能**把它写进日志或 DOM 的 data 属性。

## 4. 数据结构（JSON 键名与 Go json tag 一致）

```ts
State = {
  version: string, enabled: boolean, mode: string, modes: string[],
  socks: { address, port, active }, http: { address, port, active },
  udp:   { address, port, active },   // 与 socks 同端口；udpEnabled=false 时 active 恒为 false
  pac:   { address, port, active, url, ruleCount, generatedAt, externalURL, source, lastError },
  server: ServerView | null,
  servers: ServerView[],
  traffic: Traffic,
  helper: HelperStatus,
  settings: SettingsView,
  lastError: string, startedAt: number
}
ServerView = { id, host, port, method, password, remark, plugin, title, hasPlugin, active, connected, lastError }
Traffic    = { up, down, upSpeed, downSpeed, active, sessions }   // 字节/秒
HelperStatus = { path, installed, version, requiredVersion, versionOK, setuid, owner, mode, applied: string[], lastError }
NetworkService = { name, hardwarePort, device, selected, hasOurProxy, webProxy, socksProxy, autoProxy }
LogEntry   = { seq, time, level, scope, message }                  // time 是 Unix 秒
PACView    = { address, port, active, url, ruleCount, generatedAt, externalURL, source, lastError }
Check      = { name, status, detail, suggestion }                  // status: ok|warn|fail|info
SelfTestReport = { ok, steps: {name, ok, detail}[], elapsedMs }
SettingsView = {
  socksAddress, socksPort,
  httpEnabled, httpAddress, httpPort, httpFollowGlobal, udpEnabled,
  pacAddress, pacPort, pacBindLocalhost, gfwlistURL, externalPACURL,
  exceptions, autoConfigureNetworkServices, networkServices: string[],
  enablePAC, enableGlobal, enableManual, enableExternalPAC,
  launchAtLogin, hideDockIcon, clearProxyOnQuit, restoreProxyOnWake
}
```

## 5. 界面要求（单页，左侧竖排标签导航）

1. **状态**：总开关（大按钮，`SetEnabled`）、模式单选（5 个；`modes` 里的才算"启用"）、当前服务器、
   实时流量（`up/down` 人类可读 + `upSpeed/downSpeed`）、活动连接数/累计会话、
   本地端口一览（SOCKS/HTTP/PAC，带 `active` 指示；UDP 与 SOCKS 共端口，所以在 SOCKS
   那一项后面追加一个 `UDP` 标记而不是单独占一项——状态页"关键事实"一行固定 4 项，
   多一项就会换行）、助手状态卡片（未安装时给"安装助手"按钮）。
   最近连接明细暂无独立视图：连接历史只用于日志区展示 `ssxng:logs` 里的 `proxy` 记录
   （状态页的"最近连接"卡片按 ~250ms 节流重建，避免成批到达时反复重排 DOM）。
2. **服务器**：列表（标题、host:port、method、当前选中打勾），支持新增/编辑/删除/上移下移/选中；
   编辑表单字段：备注、地址、端口、加密方式（下拉，来自 `SupportedMethods`，见 Go
   `internal/config/servers.go`：aes-128-gcm / aes-192-gcm / aes-256-gcm /
   chacha20-ietf-poly1305）、密码（password 输入框，带显示/隐藏）、
   "导入 ss:// 链接"文本框 + 导入按钮、"复制 ss:// 链接"按钮、"测试连接"按钮；
   `hasPlugin` 为真时在列表里显示黄色标记"插件（不支持，已忽略）"。
3. **模式与代理**：`SettingsView` 表单 + `ApplySettings`；网络服务勾选列表（`ListNetworkServices`，
   `autoConfigureNetworkServices` 为真时禁用勾选并提示"自动选择 Wi-Fi/Ethernet"）；
   例外列表文本框；退出时清理系统代理、唤醒后自动恢复两个开关；
   "启用 UDP 转发"复选框（`udpEnabled`，与"HTTP 跟随全局模式"同格竖排——监听网格每多一格
   就多一整行，页高有硬门槛）；
   命令行代理块：按已保存的监听设置生成 HTTP / SOCKS5 两条 `export http_proxy=…;export https_proxy=…;`，
   每条一个"复制命令"按钮（复制内容按 `data-id` 重新生成，不读 DOM 文本）。
4. **PAC**：显示 PAC URL、规则数、生成时间、数据源；"从 GFWList 更新"按钮（`UpdatePACFromGFWList`，
   期间禁用按钮并显示加载态）；"编辑用户规则"→ `OpenUserRuleFile()`；外部 PAC URL 输入框；
   GFWList URL 输入框 + "恢复默认/使用镜像"按钮（镜像地址见 Go `config.DefaultGFWListMirror`）。
5. **日志**：虚拟滚动不重要，最多显示最近 500 行，等宽字体，按 level 着色，自动滚动到底部（用户手动
   上滚时不强制），"清空"按钮、"打开配置目录"按钮。
6. **诊断**：`Diagnose()` 表格（name/status/detail/suggestion，用图标+颜色区分 ok/warn/fail/info），
   "运行自检"按钮 → `SelfTest()`，把 `steps` 逐条渲染。

## 6. 交互与视觉约定

- 中文（zh-Hans）界面；所有文案集中在 `i18n.js` 的 `T` 对象里，便于以后加英文。
- 深色为主，跟系统 `prefers-color-scheme` 切换；用 macOS 风格（圆角 8px、系统字体 `-apple-system`、
  克制配色、无外部字体/图标库，图标用内联 SVG）。
- 后端返回的错误统一走 `toast`：底部浮层，3 秒淡出；`ssxng:toast` 事件同样走这个通道。
- 长任务（安装助手、更新 PAC、自检、测试连接）必须有禁用态 + loading 文案，不能重复点击。
- 危险操作（删除服务器）要 `confirm`。
- 端口/密码等输入必须有前端校验提示（端口 1024-65535、回环地址限制），但仍以 Go 端校验为准。
- 前端拆成四个脚本，**加载顺序即依赖顺序**：
  `i18n.js` → `app-core.js` → `app-views.js` → `app-actions.js` → `app.js`。
  每个脚本各自一个 IIFE，靠 `window.SSXNG` 互相引用；只有最后那个 `app.js` 会产生副作用
  （绑定 document 上的事件委托、首次拉取状态），其余文件只定义东西。
- **不要用模块化 import**：asset server 下相对路径 `import` 会 404，`index.html` 里也不要用
  `type="module"`（那样脚本会被推迟到 DOMContentLoaded 之后，且 `file://` 下 CORS 会失败）。
- 跨文件引用规则：只能解构**在它之前加载**的文件导出的名字
  （`const {call, App} = S;`）；要引用后面文件里的东西必须写 `S.name(...)`。
  违反这条会在启动时静默拿到 `undefined`，`scripts/ui-check/measure.js` 里有静态断言拦住它。
- **交互**：左侧标签切换视图时，每个视图各自的 `.main` 滚动位置会被记住并恢复
  （`App.scroll`，不持久化）；日志页仍按既有语义进入即置底。服务器行的"更多"菜单必须能用键盘走完：
  打开后焦点进入第一个**可用**菜单项，↑/↓ 循环、Home/End 到首尾，Escape 收起并把焦点还给触发按钮，
  Tab 收起后按默认行为继续。菜单项为禁用态时方向键不得停在它上面。

## 7. 文件交付清单（只允许改这些）

```
frontend/src/index.html
frontend/src/main.css
frontend/src/i18n.js
frontend/src/app-core.js
frontend/src/app-views.js
frontend/src/app-actions.js
frontend/src/app.js
```

`app-core.js` 放常量、`App` 状态、通用工具、状态规整与后端调用；`app-views.js` 放所有
`render*` 与事件订阅；`app-actions.js` 放表单绑定、设置读写与 `ACTIONS` 表；
`app.js` 只做启动。新增前端文件时，`index.html`、`scripts/ui-check.sh` 的拷贝列表与
`scripts/ui-check/measure.js` 的 `staticScriptOrderCheck` 三处要一起改。

不要修改任何 `.go`、`wails.json`、`build/` 下的文件。
