// Wails 运行时桩：让真实的 frontend/src/app.js 能在无后端时渲染出完整界面。
// 由 scripts/ui-check.sh 以 <script src="./stub.js"></script> 注入到 harness 页面的
// deferred 脚本之前。
//
// 保持同步：这里的 fixture 必须覆盖 internal/api/types.go 里 State 的字段。
// 前端 normalizer 对缺失字段是容错的（走默认值），但少字段会让视图变成空状态，
// ui-check 的"视图非空"断言就会失败。
(function () {
    'use strict';

    const state = {
        version: '1.0.0',
        enabled: true,
        mode: 'global',
        modes: ['auto', 'global', 'manual', 'externalPAC', 'off'],
        socks: {address: '127.0.0.1', port: 1086, active: true},
        http: {address: '127.0.0.1', port: 1087, active: true},
        pac: {
            address: '127.0.0.1', port: 1089, active: false,
            url: 'http://127.0.0.1:1089/proxy.pac', ruleCount: 8421, generatedAt: 1770000000,
            externalURL: '', source: 'gfwlist', lastError: ''
        },
        server: {
            id: 'demo', host: '203.0.113.10', port: 54376, method: 'aes-256-gcm',
            password: 'secret', remark: '演示节点', plugin: '', title: '演示节点',
            hasPlugin: false, active: true, connected: true, lastError: ''
        },
        servers: [
            {
                id: 'demo', host: '203.0.113.10', port: 54376, method: 'aes-256-gcm',
                password: 'secret', remark: '演示节点', plugin: '', title: '演示节点',
                hasPlugin: false, active: true, connected: true, lastError: ''
            },
            {
                id: 'backup', host: '198.51.100.7', port: 8388, method: 'chacha20-ietf-poly1305',
                password: 'secret', remark: '备用节点', plugin: '', title: '备用节点',
                hasPlugin: false, active: false, connected: false, lastError: '连接超时'
            }
        ],
        traffic: {up: 1048576, down: 73400320, upSpeed: 20480, downSpeed: 524288, active: 3, sessions: 3},
        helper: {
            path: '/Library/Application Support/SSXNG/ssxng-helper',
            installed: true, version: '1.0.0', requiredVersion: '1.0.0',
            versionOK: true, setuid: true, owner: '0:80', mode: '-rwsr-xr-x',
            applied: ['Wi-Fi'], lastError: ''
        },
        settings: {
            socksAddress: '127.0.0.1', socksPort: 1086,
            httpEnabled: true, httpAddress: '127.0.0.1', httpPort: 1087, httpFollowGlobal: false,
            pacAddress: '127.0.0.1', pacPort: 1089, pacBindLocalhost: true,
            gfwlistURL: 'https://raw.githubusercontent.com/gfwlist/gfwlist/master/gfwlist.txt',
            externalPACURL: '',
            exceptions: '127.0.0.1, localhost, 192.168.0.0/16, 10.0.0.0/8, FE80::/64, ::1, FD00::/8',
            autoConfigureNetworkServices: true, networkServices: ['Wi-Fi'],
            enablePAC: true, enableGlobal: true, enableManual: false, enableExternalPAC: false,
            launchAtLogin: false, hideDockIcon: false, clearProxyOnQuit: true, restoreProxyOnWake: true
        },
        lastError: '',
        startedAt: 1770000000
    };

    const logs = [
        {seq: 3, time: 1770000003, level: 'info', scope: 'proxy', message: '已启用全局代理（开启）：Wi-Fi -> SOCKS 127.0.0.1:1086'},
        {seq: 2, time: 1770000002, level: 'warn', scope: 'pac', message: '下载失败（https://raw.githubusercontent.com/…）：connection reset by peer'},
        {seq: 1, time: 1770000001, level: 'debug', scope: 'http', message: '结束 www.google.com:443，↑618B ↓4.2KiB'}
    ];

    const checks = [
        {name: '配置目录', status: 'ok', detail: '/Users/demo/Library/Application Support/SSXNG', suggestion: ''},
        {name: '特权助手', status: 'ok', detail: '已安装，setuid 正常', suggestion: ''},
        {name: '本地监听', status: 'warn', detail: 'PAC 端口 1089 未监听', suggestion: '切换到自动代理模式后会启动'}
    ];

    const selfTest = {
        ok: true, elapsedMs: 812,
        steps: [
            {name: 'SOCKS5 转发', ok: true, detail: '收到 "ssxng-selftest"'},
            {name: 'HTTPS CONNECT 隧道', ok: true, detail: '收到 "ssxng-selftest"'}
        ]
    };

    const services = [
        {name: 'Wi-Fi', hardwarePort: 'Wi-Fi', device: 'en0', enabled: true, autoSelected: true},
        {name: 'Thunderbolt Bridge', hardwarePort: 'Thunderbolt Bridge', device: 'bridge0', enabled: true, autoSelected: false}
    ];

    let calls = 0;
    const respond = (value) => function () { calls++; return Promise.resolve(value); };

    window.__UI_FIXTURE = {state: state, logs: logs, checks: checks, selfTest: selfTest, services: services};

    window.go = {
        main: {
            App: {
                GetState: respond(state),
                Version: respond('1.0.0'),
                GetLogs: respond(logs),
                ClearLogs: respond(null),
                ListServers: respond(state.servers),
                ListNetworkServices: respond(services),
                HelperStatus: respond(state.helper),
                Diagnose: respond(checks),
                SelfTest: respond(selfTest),
                PACStatus: respond(state.pac),
                UpdatePACFromGFWList: respond(state.pac),
                TestServer: respond({ok: true, latencyMs: 128}),
                GetSettings: respond(state.settings),
                SaveSettings: respond(state.settings),
                ApplySettings: respond(state.settings),
                SetEnabled: respond(null),
                SetMode: respond(null),
                SetLaunchAtLogin: respond(null),
                SetHideDockIcon: respond(null),
                ImportSSURL: respond(state.servers[0]),
                ExportSSURL: respond('ss://demo'),
                AddServer: respond(state.servers[0]),
                UpdateServer: respond(state.servers[0]),
                DeleteServer: respond(null),
                SelectServer: respond(null),
                MoveServer: respond(null),
                InstallHelper: respond(null),
                UninstallHelper: respond(null),
                OpenUserRuleFile: respond(null),
                OpenConfigFolder: respond(null),
                Quit: respond(null)
            }
        }
    };

    const listeners = {};
    window.runtime = {
        EventsOn: function (name, cb) { (listeners[name] = listeners[name] || []).push(cb); },
        EventsOff: function (name) { delete listeners[name]; },
        EventsEmit: function () {},
        WindowSetBackgroundColour: function () { calls++; },
        WindowShow: function () {},
        WindowHide: function () {},
        WindowSetTitle: function () {},
        BrowserOpenURL: function () {},
        // 真实运行时会走这条（wails:// 不是安全上下文，navigator.clipboard 往往不存在），
        // 所以桩里必须一起记录，否则 ui-check 验的是一个真实环境用不到的路径。
        ClipboardSetText: function (t) {
            window.__CLIPBOARD.text = String(t);
            window.__CLIPBOARD.count++;
            return Promise.resolve(true);
        },
        ClipboardGetText: function () { return Promise.resolve(window.__CLIPBOARD.text); }
    };
    window.__UI_CALLS = function () { return calls; };

    // 剪贴板记录器：headless Chrome 里 navigator.clipboard.writeText 未必可用，
    // 会悄悄回退到 execCommand，那样"点复制到底写进去了什么"就没法断言了。
    // 这里拦下来存一份，供 ui-check 校验写进剪贴板的正是界面上显示的那条命令。
    window.__CLIPBOARD = {text: '', count: 0};
    try {
        Object.defineProperty(navigator, 'clipboard', {
            configurable: true,
            value: {
                writeText: function (t) {
                    window.__CLIPBOARD.text = String(t);
                    window.__CLIPBOARD.count++;
                    return Promise.resolve();
                },
                readText: function () { return Promise.resolve(window.__CLIPBOARD.text); }
            }
        });
    } catch (err) {
        // 定义失败就退回真实实现，后面的断言会自动跳过。
    }

    // 走真实的 ssxng:state 事件路径推送一份状态（用于验证状态自适应界面）。
    // 不传 patch 时恢复成初始 fixture。
    const clone = (v) => JSON.parse(JSON.stringify(v));
    // 生成 n 个假节点：服务器页在节点变多 + 窗口变窄时的退化要靠它才测得出来
    window.__UI_FILL_SERVERS = function (n) {
        const many = [];
        for (let i = 0; i < n; i++) {
            many.push({
                id: 's' + i, host: '203.0.113.' + (10 + i), port: 54376, method: 'aes-256-gcm',
                password: 'secret', remark: '测试节点 ' + (i + 1), plugin: '', title: '测试节点 ' + (i + 1),
                hasPlugin: false, active: i === 0, connected: i === 0, lastError: ''
            });
        }
        state.servers = many;
        state.server = many[0];
        window.__UI_PUSH({servers: many, server: many[0]});
        return many.length;
    };

    window.__UI_PUSH = function (patch) {
        const next = clone(state);
        if (patch) Object.keys(patch).forEach(function (k) { next[k] = clone(patch[k]); });
        (listeners['ssxng:state'] || []).forEach(function (cb) { cb(next); });
        return true;
    };
})();
