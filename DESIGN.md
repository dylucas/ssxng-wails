# SSXNG 设计说明

本文说明 SSXNG 的结构、关键决策，以及它与 ShadowsocksX-NG 的差异。功能与用法见
[README.md](README.md)，前端接口契约见 [docs/API.md](docs/API.md)。

## 目标与非目标

**目标**：在 Apple Silicon macOS 上提供一个与 ShadowsocksX-NG 行为对等的 Shadowsocks 客户端
（SOCKS5 + HTTP 本地代理、PAC 自动分流、系统代理自动配置、菜单栏常驻），代码可读、可测试、
依赖尽量少。

**非目标**：成为 ss-local 的全能替代（不做插件、订阅、SS2022），也不做跨平台适配。

## 分层

```
main.go                 Wails 启动参数、资源嵌入、单实例准备
app.go                  应用状态机：生命周期、绑定的前端方法、状态快照、后台循环
app_proxy.go            代理开关与模式切换
app_settings.go         设置的视图转换、保存/应用、登录项与 Dock 图标、网络服务枚举
app_servers.go          节点增删改查、排序、ss:// 导入导出、连通性测试
app_pac.go              PAC 生成、PAC 服务器、gfwlist 下载、规则文件入口
app_diag.go             助手状态、自检、诊断报告
app_log.go              日志读取与日志文件
app_tray.go             菜单栏集成
app_util.go             字符串/Finder/下载/原子写等小工具
apply.go                系统代理期望状态计算与 reconcile（调用助手、校验、记录）
frontend/src/           界面（index.html / i18n.js / main.css + 4 个 app 脚本），无构建步骤
cmd/ssxng-helper/       setuid 助手入口（argv 白名单）
internal/sscore/        SOCKS5 + HTTP 代理 + AEAD 隧道 + UDP 中继 + 统计（核心）
internal/ssurl/         ss:// 解析与生成（SIP002）
internal/pac/           gfwlist 解码、规则合并、PAC 生成、PAC HTTP 服务器、abp.js
internal/config/        设置与服务器配置的存取（JSON，原子写）
internal/proxyconf/     网络服务发现、代理状态读写、命令计划、快照、助手安装
internal/diagnose/      诊断检查 + 端到端自检
internal/launchd/       开机自启（LaunchAgent）
internal/tray/          菜单栏图标（cgo + AppKit）
internal/lockfile/      单实例锁
internal/logbuf/        环形日志缓冲
internal/helperbin/     嵌入的助手二进制
internal/api/           前后端 DTO（唯一契约）
```

`package main` 拆成多个 `app*.go` 是按领域切的纯搬移，彼此没有导入关系（本来就是同一个包），
拆分的依据是"改哪一类需求就动哪个文件"。

前端同理：`app.js` 拆成四个脚本，**加载顺序即依赖顺序**
（`i18n.js` → `app-core.js` → `app-views.js` → `app-actions.js` → `app.js`）。
每个脚本各自一个 IIFE，靠 `window.SSXNG` 互相引用；只有最后那个 `app.js` 会产生副作用
（绑定事件、首次拉取状态）。一个文件只允许解构"在它之前加载"的文件导出的名字，
要引用后面文件里的东西一律写 `S.name(...)`，这样加载顺序就不会变成隐式契约。
`scripts/ui-check/measure.js` 会静态校验这两条规则。

依赖方向是单向的：`app*.go` → `internal/*` → 标准库与 `go-shadowsocks2`。
`internal/sscore`、`internal/pac`、`internal/ssurl`、`internal/proxyconf` 都不依赖 Wails，
可以单独测试，也方便将来接到别的外壳上。

## 关键流程

**启动**：`main.go` 关掉 go-shadowsocks2 的重放过滤器（见下）→ Wails 建窗 →
`OnStartup` 里打开配置、取单实例锁、建引擎与 PAC 服务器、按 `hideDockIcon` 决定 Dock 图标、
建菜单栏图标、必要时开启代理、启动两个后台循环（1 s 流量统计、20 s 看门狗）→ `OnDomReady`
推一次完整状态。

**开启代理**：校验有服务器 → 起 SOCKS5/HTTP 监听（端口 0 也能起，取实际地址）→ 起 PAC 服务器
（自动模式）→ `apply.reconcile()` 调助手改系统代理。本地代理与系统代理分离处理：改系统代理
失败只提示不失败，因为本地端口仍然可用。

**模式切换**：`SetMode` 先落盘，再 `syncPACServer()`，再 reconcile。关闭（`off`）走
`forceOff()`：先把系统代理恢复成快照，再停监听。

**关闭**：`shutdown` 停循环 → 按 `clearProxyOnQuit` 决定是否恢复系统代理 → 停引擎/PAC/菜单栏
→ 解锁。窗口关闭语义见下。

**PAC 管线**（`internal/pac`）：`gfwlist.txt`（base64，容忍换行/空白）→ 解码 → 与
`user-rule.txt` 合并（用户规则全保留，gfwlist 里与用户规则重复的行丢弃）→ 去掉空行/注释/
`[AutoProxy` 头 → 规则数组填进 abp.js 的 `var rules = __RULES__;` → 替换 `__SOCKS5ADDR__`/
`__SOCKS5PORT__` → 写 `gfwlist.js`。PAC 服务器**每个请求重新读文件**，更新后立即生效；
URL 带 `?v=<epoch>` 让系统代理不缓存（关闭时忽略查询串比较，因此仍能正确清理）。

**系统代理**（`internal/proxyconf`）：`networksetup` 每个动作一条 argv，
`proxyconf.runCommand` 是唯一出口，测试里替换成假实现；命令先算成 `Command` 列表（`PlanGlobal`
/ `PlanAuto`/`PlanOffFor`）再执行，所以 `-dry-run` 能打印真实计划。写操作全部在助手进程里，
GUI 只发 argv。

## 关键决策

**为什么用 setuid 助手，而不是每次弹窗或 root daemon**
`networksetup` 改代理必须 root，而 GUI 进程不能常驻 root。SSX-NG 用 setuid helper 解决，
这里沿用：一次授权安装 `ssxng-helper`（`root:admin`、`4755`），之后所有系统代理改动都是普通
exec 调用，零弹窗。备选方案（LaunchDaemon + XPC/Unix socket、每次 `sudo` 弹窗）要么复杂得多，
要么体验差。助手侧做了加固：要求 euid 0、真实 uid 0 或 admin 组成员、只接受白名单参数、
服务名必须存在于 `networksetup -listallnetworkservices`、支持 `--dry-run`，并且**拒绝**任何
不在白名单里的 argv。

**为什么关掉 go-shadowsocks2 的 salt 重放过滤器**
该过滤器是为服务端防重放设计的，进程内全局单例。客户端用它有两个问题：一是给每条连接引入
约 1e-6 的误报（"repeated salt detected"），二是内置自检需要在同一进程里起一个服务端，
客户端自己写的 salt 会被自己的服务端判为重复。`main.go` 默认设
`SHADOWSOCKS_SF_CAPACITY=-1`，但**不覆盖**用户显式设置的值。

**为什么自己实现 HTTP 代理**
SSX-NG 通过 privoxy 做 HTTP→SOCKS 转换，多一个进程、一份配置。这里直接用 Go 实现
`CONNECT` 隧道和绝对形式请求转发（重写成 origin-form 再发），少一个外部依赖，
也少一处"privoxy 没起来"的故障点。

**为什么前端是纯 HTML/CSS/JS**
界面不复杂（6 个标签页），省掉 npm/Node/打包器这一整条工具链：`//go:embed all:frontend/src`
直接进二进制，`wails dev` 的资产服务器按路径映射到 `frontend/src/`。因此项目只需 Go 工具链
加 Wails CLI 即可构建。

**窗口关闭 / 退出语义**
`hideDockIcon` 打开且菜单栏图标可用时，关窗口只是隐藏（后台常驻，退出走菜单栏）；
否则关窗口即退出。这样即使用户把菜单栏图标关了（或系统没有菜单栏），也不会被困在
"关不掉的进程"里。退出前置 `quitting` 标志位，避免 `OnBeforeClose` 二次拦截。

**为什么配置用 JSON 文件而不是 NSUserDefaults**
便于查看、备份、做多份配置（`SSXNG_CONFIG_DIR`），也避免 `UserDefaults` 与 Wails 生命周期
耦合。写入一律"临时文件 + rename"。

**空例外列表不是"没有例外"**
全局模式会把例外列表写进系统 bypass。如果用户清空了例外列表，`Settings.ExceptionList()`
会回落到默认列表，而不是把 bypass 清空——后者会把回环和局域网流量也塞进代理，通常直接断网。
同理，外部 PAC 的地址在交给助手之前先用 `pac.ValidateURL` 校验（`desiredProxy` 是唯一出口），
避免一个笔误变成系统设置。

**先落盘、再应用（`SetEnabled` 的顺序）**
`desiredProxy()` 是从**已保存的设置**推导"系统代理应该是什么样"。所以打开代理时必须先把
`enabled=true` 写进配置，再去启动监听、调用助手。早期实现是反的：开启时 `reconcile` 读到的
仍是 `enabled=false`，于是只推导出 `off`，助手只做清理、从不安装系统代理——表现就是"开关是开的、
本地端口也在监听，但浏览器走直连"。看门狗（`verify`）现在会比较"设置期望的助手模式"与
"上次实际安装的模式"（含服务列表），不一致就重新应用，所以"以为装上了其实没装"最多 20 秒自愈；
`SetEnabled` 失败时会把 `enabled` 回滚，避免界面显示"已开启"。

**界面：窗口拖动与标题栏保留区**
`mac.TitleBarHiddenInset()` 让网页铺满窗口（`FullSizeContentView` + 空 `NSToolbar`），红绿灯浮在
左上角，而 Wails 不会把窗口设成"按背景拖动"。拖动必须由界面显式声明：Wails 运行时只在 `mousedown`
命中元素的 computed `--wails-draggable` 等于 `drag` 时才拖动窗口。做法是顶部固定一条 `.titlebar`
（高度 `--titlebar-h` = 空工具栏的 52px），侧栏与主区都向下让出这段高度，红绿灯正好落在保留区内，
侧栏品牌不再被压住。这条区域里不放交互元素——自定义属性会继承，放了按钮就会吞掉点击；
将来若要扩大拖动范围，必须给交互元素显式写 `--wails-draggable: no-drag`。

**状态页：从"卡片堆"改成"一眼看状态"**
原来的状态页把 5 个模式单选的整列、一个 178px 的大开关、6 个流量数字和 3 行监听端口
堆成两张并排的卡片，最需要一眼看到的信息（开没开、什么模式、走的哪个节点）反而不突出。
现在顶部是一条状态条：左侧圆底图标 + "代理运行中/已停止/代理异常"，第二行是模式与当前节点，
右侧只保留一个跟当前状态相关的主动作（没有节点 → "添加服务器"；已开启 → "关闭代理"；
未开启 → "开启代理"），另有一个"切换模式"入口。状态条下面一行 4 个"关键事实"
（SOCKS / HTTP / PAC 端口与已运行时长），端口从三行堆叠变成一行横排。
流量卡片把上传/下载**速度**提到前面并上色（上传用强调色、下载用绿色），累计量退到后面。
模式单选搬到"模式与代理"页——状态页负责"看"，配置页负责"配"，也避免了同一份状态在两处出现。
状态条会随 `ssxng:state` 事件（含托盘切换）实时更新，`apply_test.go` 与 ui-check 都覆盖了
"开启/关闭/无节点/异常"四种形态。

**密度：先用全局令牌收紧，再谈版面重构**
"模式与代理"页在 680px 的默认窗口里高 1498px（2.2 屏），7 张单列卡片里有 1 张只放了一个复选框。
用户明确要求保持单列，所以这一轮只做密度压缩：`.card` 内边距、`.card-title` 下边距、
`.view-body`/`.grid` 间距、`.check` 行高统一收紧一处生效全局；把只有一个开关的"网络"卡
并进相邻的"网络服务"卡做分组；模式单选原先"标题一行 + 说明一行"，改成标题与说明同行
（`flex-wrap: wrap`，窄窗自动换回两行）。结果 1498px → 1252px（-16%），仍是单列。
教训写在检查里：`scripts/ui-check.sh` 现在把"页高上限"和"必须保持单列"也断言了，
因为密度是会被下一次改动悄悄退回去的东西——它不像颜色那样一眼能看出来。

**状态页收尾：奇数张卡片不留半行空洞**
状态页底部是两栏栅格，却放了 3 张卡，"特权助手"独占半行、右边空着。现在给它加
`is-wide` 跨整行，并把里面的 kv 在该宽度下排成两栏。同时删掉"启动时间/已运行"两行——
顶部"关键事实"已经在显示运行时长，绝对启动时间改为该事实的 `title`，信息不再重复、
语义也归位（它本来就不属于"特权助手"这张卡）。状态页 970px → 795px。

**行内操作：常驻 4 个 + 一个"更多"菜单**
服务器列表每行原来有 7 个按钮（选择/测试/上移/下移/复制/编辑/删除）。`.row-actions` 是
`flex: 0 0 auto`，所以窗口一窄，被挤压的不是按钮而是标题：700px 宽、8 个节点时按钮固定占
414px，标题只剩 134px、被撑成 111px 高，单行 103px、整页 1495px。现在常驻只留选择/测试/编辑/
删除，上移/下移/复制 ss:// 收进"更多"菜单；再把列表行的竖向内边距从 9px 收到 6px、副标题上边距
2px→1px。结果：700px 下整页 **1495px → 777px**、行高 **103px → 79px**、标题可用宽度 134px → 202px。
菜单项就是普通的 `data-act` 按钮，复用既有动作，没有新增任何后端调用；点空白处或按 Escape 收起，
`aria-haspopup`/`aria-expanded` 跟着渲染写回。

**关闭代理时，必须仍然能认出"这条 PAC 是我们写的"**
用户报"关了代理之后浏览器上不了网"，查到 macOS 的"自动代理"仍然是**打开**状态、URL 指向
`http://127.0.0.1:1089/proxy.pac`，而那时 PAC 服务器已经停了。根因是助手的一个安全设计：
`proxyconf.Matches` 只清理"看着像我们写的"设置，而在 PAC 模式下唯一的身份标记就是 PAC URL
（SOCKS 是被关掉的，没法当标记）。关闭时的代码却用"当前是否启用 + PAC 服务器是否在跑"去推导
这个 URL——可关闭时 `enabled` 已经落盘成 false、PAC 服务器也已经停掉，于是 `--pac-url` 根本没传，
助手把这条设置当成"别人的配置"跳过，什么都没清。更糟的是日志照样打印"退出前已清理系统代理"，
把故障掩盖成了成功。

现在改成一份**候选列表**（`pacMatchCandidates`）：先是"我们实际装过的那个 URL"（记录在
`appliedProxy` 里），再是设置里的外部 PAC，最后是由监听地址推出的本地 PAC；调用方逐个尝试，
助手认出哪个就用哪个。同时把"什么都没清掉"和"清掉了 N 个服务"分开记录——前者在我们确实装过
东西时要报 warn，因为用户会直接表现为断网。三条回归测试分别覆盖 `forceOff`、重启后的
`forceOff`（进程内没有记录）和 `SetMode(off)` 走 `reconcile` 的分支。

教训有两条：一是"容错/跳过"这类保护逻辑必须有对应的**可观测**输出，否则它会把失败伪装成成功；
二是推导身份标记时不能依赖"当前状态"，因为清理动作本身就会先把状态改掉。

**自检的桩必须与真实 DTO 同形**
`scripts/ui-check/stub.js` 里 `Diagnose` 一直写成 `{checks: [...]}`，而 `app*.go` 真实返回的是
`[]api.Check`。前端 `arr(r.value)` 对错误形状是容错的（拿到空数组、显示"未运行"），于是
**诊断结果表从未在自检里渲染过一次**——它的横向溢出与文字对比度都没有被覆盖，而自检一直是全绿。
修好形状后表格才真的出现，也才被纳入检查。教训是：容错解析会把桩的错误静默吞掉，
所以桩的形状要单独对照 `internal/api/types.go` 和 `app*.go` 的返回类型核一遍——
这件事现在由 `scripts/ui-check/measure.js` 的 `staticAPIShapeCheck` 自动做。

**界面：颜色只走变量，并有对比度自检**
所有颜色都在 `:root`（深色）与 `prefers-color-scheme: light` 覆盖里定义为变量。
`scripts/ui-check.sh` 会静态断言"颜色字面量只能出现在自定义属性声明里"（否则浅色模式会拿到写死的深色值），
并实测深浅两套主题下每个文字元素的前景/背景对比度。最初三级文字（提示、字段标签、统计标签）
只有 3.3–3.6:1、白字压强调色只有 2.9:1，因此 `--text-3` 提亮/加深，并拆出
`--accent-text`（强调色文字）与 `--accent-strong`（承载白字的按钮底色），
`--accent` 继续用于图标/边框这类非文字场景。

**命令行代理：复制"当前生效"的 export 命令**
SSX-NG 的菜单里有 "Copy HTTP Proxy Shell Export Command"，这里把它做成两处入口：
「模式与代理」页的"命令行代理"块（HTTP 与 SOCKS5 各一行等宽的
`export http_proxy=…;export https_proxy=…;`，右侧一个复制按钮），以及菜单栏里的
"复制 HTTP 代理命令"/"复制 SOCKS5 代理命令"两项。菜单栏这两项与窗口同屏可见的命令是
**同一串文字**，都从已保存的监听设置生成，所以"菜单里复制的"和"窗口里显示"不会分叉。

窗口侧的两个刻意取舍：命令文本从**已保存的 settings** 生成，而不是编辑中的草稿——端口改了但
没保存时监听端口并没有变，命令也就不该跟着变（此时用一行提示说明"保存并应用后命令才会更新"）；
点击复制时按 `data-id` 重新生成命令，而不是读 DOM 里的文字，避免界面文本（截断、省略号、
本地化）污染剪贴板。HTTP 关闭时只保留 SOCKS5 并说明如何启用；代理未开启时照常给出命令，
只加一行"需要先开启代理才有效"。

菜单栏侧的取舍：命令由 Go（`App.trayProxyCommands`）生成后随 `tray.Snapshot` 一起下发，
AppKit 把每项要复制的文字挂在 `representedObject` 上，**点击时由 Cocoa 直接写
`NSPasteboard`**，不绕前端——这样窗口隐藏、甚至 webview 还没起来时也能复制，
`ssxngTrayEvent(5, …)` 只是回来让 Go 记日志并弹一个"已复制到剪贴板"。
剪贴板在窗口侧首选 Wails 的 `ClipboardSetText`（`wails://` 不是安全上下文，
`navigator.clipboard` 常常不存在，而 `execCommand` 依赖用户手势、且已被标记废弃，
所以另外两条只作回退）。

这条链路横跨 Go、cgo 和界面三层，坏起来又都是静默的，所以三处各自钉住同一串字面量：
`tray_test.go` 断言 Go 生成的命令、`scripts/ui-check/measure.js` 断言窗口显示与复制的内容、
`scripts/tray-check.sh` 把真实的 `tray_darwin.m` 编译成无界面小程序，断言菜单项标题、
`representedObject`（即写进剪贴板的文字）与 `copyCommand:` 动作都对。
菜单项的解析规则是 `key|title|command`，第二个分隔符之后的全部内容都算命令，
所以命令里出现 `|` 也不会被截断（`EncodeMenuSpecs` 另有单元测试）。

**菜单栏图标：一眼看出代理开没开**
图标是代码画的纸飞机，18×18，设成 template——macOS 只按 alpha 通道渲染 template 图，
所以它在浅色栏上是黑的、深色栏上是白的，高亮时自动反色。这带来一个约束：**开关状态不能靠
颜色表达**（颜色会被系统吃掉），只能靠形状。于是开启时画**实心**剪影，关闭时画**同一轮廓的
空心**（描边不填充），"空 = 关"在两种菜单栏外观下都成立。

实现上 `ssxngIconForEnabled(BOOL)` 缓存两张图（`Refresh` 每次状态变化都会跑，不该每跑一次
就重画一遍），`ssxngTrayRefresh` 每次按 `enabled` 调 `setImage:`；`SetEnabled` 两条分支末尾
都会 `emitState()`，而它必然调 `tray.Refresh()`，所以从窗口开关、菜单栏开关还是看门狗
恢复，图标都会立刻跟上。测试不去读像素"看起来对不对"，而是问一个能判定对错的问题：
**飞机重心**（顶点均值，射线法确认落在多边形内）在实心图里必须不透明、在空心图里必须透明，
而轮廓中点两种图都必须有笔画（空心不等于空白）；另断言两张图尺寸、template 标志与缓存命中。
`SSXNG_TRAY_ICON_OUT=<dir> scripts/tray-check.sh` 会把两张图和一张深浅底色的 6× 预览导出来，
人工确认观感——像素断言管不了"好不好看"。

**UDP 转发：共用一个端口，按客户端地址分会话**
RFC 1928 的 `UDP ASSOCIATE` 里客户端先建一条 TCP 连接、拿到中继地址，之后把数据报
（`RSV(2) FRAG(1) ATYP DST.ADDR DST.PORT DATA`）发给那个地址；回应同样是这个格式，
只是地址换成来源。出站时把前三个字节去掉、把地址头留在加密载荷里，Shadowsocks 服务端
按地址头自己分流；入站时反过来补上 `RSV/FRAG=0`。所以"一个客户端一条加密 socket"就够，
不需要按目标建连。

几个必须做对、做错了会静默失效的地方：

- **应答里的 `BND.ADDR/BND.PORT` 必须是真的**。CONNECT 的应答历来填 `0.0.0.0:0`，因为客户端
  从不读它；UDP 的应答就是客户端唯一的收件地址，填空等于让所有 UDP 应用无声无息地失败。
  `replyBytesAddr` 因此单独存在，`TestUDPAssociateRewritesTheBoundAddress` 钉住它。
- **UDP 与 SOCKS 共用一个端口**。TCP 与 UDP 是两套命名空间，所以 `127.0.0.1:1086` 上同时
  监听 TCP 和 UDP 不冲突；这也意味着用户不需要在应用里再配一个端口。绑定是事务性的：
  UDP 绑不上时 TCP 监听也一起回滚。
- **会话按客户端地址索引，靠空闲回收**。整个中继只有一个 UDP socket，所有客户端共用它；
  会话不是随 TCP 控制连接结束而销毁的，因为一个客户端可能开多条关联、也可能提前断开而
  继续发数据报（`TestUDPAssociateKeepsControlConnectionOpen` 钉住了控制连接不会被主动关）。
  所以 60 秒无活动就回收，另有 256 条的上限防止会话表把文件描述符耗光。
- **过大的数据报显式丢弃**。`shadowaead` 的 `PacketConn` 用固定 64 KiB 缓冲区，
  超过 `64KiB - salt - tag` 的载荷会被它**静默截断**（`Pack` 只返回 `io.ErrShortBuffer`，
  而写入方只看得到写成功）。中继因此先算容量再转发，宁可丢掉也不发一个半截包。
- **FRAG≠0 直接丢**。分片在 SOCKS5 里是可选的，主流客户端不发；要支持就得在这里重组。

设置里 `udpEnabled` 默认开启。它在 `Settings` 里是普通 `bool` 且默认值为真——`config.Open`
是把文件 `Unmarshal` 到 `DefaultSettings()` 之上，所以**旧配置文件里没有这个键时仍然是开启**，
不会因为升级而被静默关掉（`TestUDPSettingDefaultsToOnForExistingConfigs` 钉住）。切换它
必须重启引擎，因此它和监听地址一样参与 `needRestart` 判定。

## 与 ShadowsocksX-NG 的差异

| 方面 | ShadowsocksX-NG | SSXNG | 原因 |
| --- | --- | --- | --- |
| 实现语言/外壳 | Swift + Cocoa | Go + Wails(WebView) | 项目要求；跨语言复用协议实现 |
| 配置存储 | `NSUserDefaults` | `~/Library/Application Support/SSXNG/*.json` | 可查看、可备份、可分离 |
| 规则文件 | `gfwlist.txt` + `user-rule.txt` | 同名同语义 | 保持一致 |
| PAC URL | `http://127.0.0.1:8090/proxy.pac` | 默认端口 1089，URL 带 `?v=` | 避开常见占用；缓存击穿 |
| 自动模式 bypass | 写入 `ProxyExceptions` | **不改动** | 避免破坏用户自有列表（如 `*.cn`） |
| 全局模式 bypass | 写入 `ProxyExceptions` | 写入例外列表，关闭时按快照恢复 | 与 SSX-NG 一致，但可恢复 |
| HTTP 代理 | privoxy 子进程 | Go 内实现 | 少一个进程/依赖 |
| 命令行代理命令 | 菜单栏 "Copy HTTP Proxy Shell Export Command" | 菜单栏两项 + 「模式与代理」页两条命令，都能一键复制 | 与监听设置同屏，也能在窗口收起时直接用 |
| 开机自启 | SMLoginItem 辅助 App | LaunchAgent（`org.ssxng.SSXNG.plist`） | 无需额外 bundle |
| 加密方式 | 含流式加密、xchacha20 | 仅 4 种 AEAD | 安全性；避免选了连不上 |
| UDP | 支持 | 支持（SOCKS5 `UDP ASSOCIATE`，与 SOCKS 共用端口，默认开启） | 用 TUN 或额外端口都会与"只监听回环"冲突 |
| SS2022 / SIP003 插件 / 订阅 / 二维码 | 部分支持 | 不支持 | 范围外，明确不做 |
| 帮助/诊断 | 无 | 诊断页 + 端到端自检 | 降低排障成本 |
| 助手参数 | `-m/-u/-p/-l/-r/-s/-n/-x/-v` | `--mode/--pac-url/--port/--socks-listen-address/--http-port/--http-listen-address/--network-service/--proxy-exception/--state-path/--dry-run` | 长参数更明确 |

保留的 SSX-NG 语义：模式集合与含义、`AutoConfigureNetworkServices` 按硬件端口
（AirPort/Wi-Fi/Ethernet）自动选服务、默认端口 1086/1087、默认例外列表、PAC 规则合并与去重
（`@`/`|` 前缀剥离后比对）、PAC URL 必须是 `http`/`https`/`file`、关闭时"PAC URL 或
SOCKS host:port 匹配且当前为开启状态"才清理。

## 安全设计

- 监听默认 `127.0.0.1`；`config.Validate` 拒绝非回环地址，除非用户显式改成局域网地址。
- 服务器密码只存在于配置文件（0600）与发往助手的 argv 中，**不进日志**、不推给前端以外的地方
  （前端需要显示/编辑密码，因此 `ServerView` 里有 `password` 字段）。
- 助手 argv 白名单 + 服务名白名单 + `--dry-run`；`proxyconf.Call` 用固定路径
  `/Library/Application Support/SSXNG/ssxng-helper`，不接受任意路径。
- 下载的 gfwlist 先解码校验，成功才覆盖本地文件，避免一次坏响应把规则清空。
- 改系统代理前快照、关闭时恢复；快照损坏只警告不失败（否则用户会"关不掉代理"）。

## 测试与验证

`go test ./...` 覆盖（全部离线、不触碰系统设置）：

- `internal/sscore`：EVP_BytesToKey 已知向量；四种加密方式经 SOCKS5 端到端；HTTP 绝对形式转发；
  HTTPS `CONNECT` 隧道；1 MiB 大流量完整性；8 并发连接；服务器不可达时的 SOCKS 错误码；
  目标拒绝连接的行为；UDP ASSOCIATE 的完整往返（含应答里的 BND.ADDR/PORT、多目标复用同一会话、
  按客户端地址分会话、FRAG≠0 与畸形报文丢弃、会话上限、空闲回收、停止后 UDP 端口释放、
  服务器热切换重建会话）；UDP 关闭时按协议拒绝；停止后端口释放；热切换服务器。
- `internal/pac`：base64 容忍、行切分、用户规则合并与去重、空/注释行过滤、PAC 生成与占位符替换、
  URL 校验、PAC 服务器响应头与缓存、规则计数与缓存击穿 URL 解析。
- `internal/proxyconf`：`networksetup` 输出解析（含 `** Error:` 走 stdout 但退出码 0 的情况）、
  命令计划、快照往返、服务名校验、助手状态判定。
- `internal/ssurl`：SIP002 base64/明文/整串 base64 三种形态、`?plugin=`、`#remark` 往返。
- `internal/diagnose`：检查项结论、自检全流程（真起服务端 + 回显服务，跑 SOCKS5/HTTP/CONNECT/
  256 KiB，最后确认端口释放）、配置目录不可写时正确失败。
- `internal/config`、`internal/logbuf`、`internal/lockfile`：存取与边界。
- 根包 `apply_test.go`：不开代理时 `desiredProxy` 必须是 `off`（这正是"开启却被算成 off"的坑）、
  `SetEnabled(true)` 时助手必须收到 `--mode global` 且调用助手那一刻设置里已记录为开启、
  关闭时收到 `--mode off` 且监听释放、看门狗能从"上次记录为 off"的坏状态恢复、手动模式不碰系统代理、
  外部 PAC 地址非法时拒绝、切换自动模式时带上 PAC 地址。测试用桩替换 `callHelper`/`helperStatus`，
  端口取临时空闲端口，所以不会碰到真机上的 1086/1087/1089。

界面回归用 `scripts/ui-check.sh`（无头 Chromium + 真实前端文件）：标题栏/拖动区几何、
980/880/700 三档宽度无横向溢出、深浅两套主题里全部文字元素的对比度、6 个视图都能渲染出内容、
`main.css` 无硬编码颜色且 `var()` 引用都有定义。它只做"可断言的硬指标"，观感仍要人眼确认，
所以迭代用 `make dev` 热加载。

菜单栏那层原生代码用 `scripts/tray-check.sh`：把真实的 `tray_darwin.m` 连同它依赖的
Cocoa 框架编译成一个无界面小程序（`gTarget` 手工建好，不碰 `NSStatusBar`），断言
"命令行代理"菜单项的条数、标题、`representedObject`（就是点击后会写进剪贴板的文字）、
`copyCommand:` 动作与目标对象，以及空 spec / 坏行 / 命令含 `|` 的边界。
Go 与 Objective-C 之间的格式（`EncodeMenuSpecs` 的 `key|title|command`）由
`internal/tray/tray_test.go` 单独钉住，所以这条链路的每一层都有断言——
它的失效方式是静默的（标题空、复制出错位的文字），而 ui-check 看不见原生菜单。

手工验证过（本机、沙箱内可做的部分）：

- `wails build` 产出 `build/bin/SSXNG.app`（arm64，12.9 MB，助手二进制已嵌入），ad-hoc 签名校验通过。
- App 启动后写出 `config.json` / `servers.json` / `ssxng.lock` / `ssxng.log`，单实例锁生效，
  进程稳定不崩（菜单栏图标创建成功，无警告）。
- `wails dev` 资产服务器：`/`、`/index.html`、`/i18n.js`、`/main.css` 与四个 app 脚本
  （`/app-core.js`、`/app-views.js`、`/app-actions.js`、`/app.js`）均 200 且
  Content-Type 正确，已删除的 `/main.js` 返回 404。
- 助手：`--version`、`--list-services`（真实读回 Wi-Fi/Thunderbolt Bridge 的代理与 bypass 列表）、
  三种模式的 `--dry-run` 命令计划（全局模式启用 127.0.0.1:1087 的 HTTP/HTTPS 与 1086 的 SOCKS
  并写入例外列表；自动模式只写 PAC URL 与开启自动代理、不动 bypass；关闭模式在当前设置不属于
  本应用时不产生任何命令）。

**未能在沙箱内验证**（需要管理员授权真实改系统设置）：安装助手、真实切换系统代理、
睡眠唤醒后的看门狗恢复。请在真机上按 README 的步骤跑一次「自检」，然后用
`networksetup -getwebproxy Wi-Fi`、`-getsocksfirewallproxy Wi-Fi`、`-getautoproxyurl Wi-Fi`
确认三个模式的改动与关闭后的恢复。

## 后续可做

- DNS over TCP/DoH 的默认策略（目前 DNS 是否走代理取决于应用是否用 UDP，浏览器自带 DoH 时可能绕过）。
- SS2022（SIP022）与 SIP003 插件进程管理。
- 订阅链接导入与规则分流（acl/geo 文件）。
- 用 `notarize`/Developer ID 正式签名，替掉 ad-hoc 签名。
- 把菜单栏图标换成正式设计的模板图（目前是代码绘制的纸飞机，已设为 template 以适配深浅色）。
