/*!
 * SSXNG — 前端核心（ES2020 普通脚本，无模块、无框架、无外部请求）
 *
 * 前端拆成四个脚本，按下面的顺序加载，各自一个 IIFE，靠 window.SSXNG 互相引用：
 *   i18n.js → app-core.js → app-views.js → app-actions.js → app.js
 * 只有 app.js 会产生副作用（绑定事件、首次拉取状态），其余文件只定义东西。
 * 一个文件只允许解构"在它之前加载"的文件导出的名字；要引用后面文件里的东西
 * 一律写成 S.name(...)，加载顺序就不会变成隐式契约。
 *
 * 本文件：文案表入口、常量、应用状态 App、通用工具、状态规整、后端调用与剪贴板。
 *
 * 约定：
 *   · 调用后端一律走 window.go.main.App.<Method>()，事件一律走 window.runtime.EventsOn()。
 *   · 后端不存在时（window.go / window.runtime 缺失）不抛异常，只显示“后端未就绪”占位。
 *   · 所有后端调用都包在 call() 里：它永不 reject，返回 {ok, value}，错误统一进 toast。
 *   · ssxng:state 是唯一真相来源：收到就整页重渲染（表单正在输入时跳过，避免打断用户）。
 *   · 密码只存在于内存对象里，绝不写进日志文本或 data-* 属性。
 */
(function () {
    'use strict';

    // 命名空间：后面三个脚本都从这里取东西。
    const S = {};
    window.SSXNG = S;

    'use strict';

    const T = window.T;
    if (!T) {
        const p = document.createElement('p');
        p.textContent = 'i18n.js missing / 文案表未加载';
        p.style.padding = '20px';
        document.body.appendChild(p);
        return;
    }

    const VIEWS = ['status', 'servers', 'modes', 'pac', 'logs', 'diag'];
    const MODES = ['auto', 'global', 'manual', 'externalPAC', 'off'];

    // internal/config/servers.go: SupportedMethods
    const METHODS = [
        'aes-128-gcm',
        'aes-192-gcm',
        'aes-256-gcm',
        'chacha20-ietf-poly1305'
    ];

    // internal/config/config.go
    const DEFAULT_GFWLIST_URL = 'https://raw.githubusercontent.com/gfwlist/gfwlist/master/gfwlist.txt';
    const MIRROR_GFWLIST_URL = 'https://cdn.jsdelivr.net/gh/gfwlist/gfwlist@master/gfwlist.txt';
    const DEFAULT_EXCEPTIONS = '127.0.0.1, localhost, 192.168.0.0/16, 10.0.0.0/8, FE80::/64, ::1, FD00::/8';

    const LOG_LIMIT = 500;
    const TOAST_MS = 3000;
    const TOAST_MAX = 5;
    const DEFAULT_SOCKS_PORT = 1086;
    const DEFAULT_HTTP_PORT = 1087;
    const DEFAULT_PAC_PORT = 1089;

    const App = {
        state: null,          // normalizeState() 之后的 State
        view: 'status',
        backendReady: false,
        backendDetail: '',
        eventsBound: false,
        logs: [],             // LogEntry[]，最多 LOG_LIMIT 条（客户端镜像）
        logSeq: 0,            // 已见过的最大 seq：日志去重的水位线
        logScroll: 0,         // 日志列表的滚动位置，重渲染后按需还原
        logLevel: 'all',
        follow: true,
        services: [],         // NetworkService[]
        servicesLoaded: false,
        servicesLoading: false,
        checks: null,         // Check[]
        selfTest: null,       // SelfTestReport
        draft: null,          // SettingsView 草稿
        dirty: false,
        editor: null,         // { mode: 'new'|'edit', view: ServerView }
        importRaw: '',
        menu: null,           // 行内"更多"菜单：{ id } | null
        menuId: '',           // 最近一次打开菜单的行 id（Escape 后把焦点还回去）
        pending: {}           // 长任务禁用态：key -> {text}
    };

    const $ = (sel, root) => (root || document).querySelector(sel);
    const $$ = (sel, root) => Array.prototype.slice.call((root || document).querySelectorAll(sel));

    const ESC_MAP = {'&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;'};

    function esc(v) {
        if (v === null || v === undefined) return '';
        return String(v).replace(/[&<>"']/g, (c) => ESC_MAP[c]);
    }

    function fill(tpl) {
        const args = Array.prototype.slice.call(arguments, 1);
        return String(tpl === null || tpl === undefined ? '' : tpl)
            .replace(/\{(\d+)\}/g, (m, i) => (args[+i] === undefined || args[+i] === null) ? m : String(args[+i]));
    }

    function obj(v) { return (v && typeof v === 'object' && !Array.isArray(v)) ? v : {}; }
    function arr(v) { return Array.isArray(v) ? v : []; }
    function str(v) { return (v === null || v === undefined) ? '' : String(v); }
    function num(v, d) { const n = Number(v); return Number.isFinite(n) ? n : (d || 0); }
    function bool(v) { return v === true; }

    function pad2(n) { return n < 10 ? '0' + n : String(n); }

    const BYTE_UNITS = ['B', 'KB', 'MB', 'GB', 'TB', 'PB'];

    function fmtBytes(v) {
        let n = num(v, 0);
        const neg = n < 0;
        n = Math.abs(n);
        let i = 0;
        while (n >= 1024 && i < BYTE_UNITS.length - 1) { n /= 1024; i++; }
        const s = i === 0 ? String(Math.round(n)) : n.toFixed(n < 10 ? 2 : 1);
        return (neg ? '-' : '') + s + ' ' + BYTE_UNITS[i];
    }

    function fmtSpeed(v) { return fmtBytes(v) + str(T.unit && T.unit.perSec); }

    // 后端的时间戳既可能是 Unix 秒，也可能是毫秒/微秒/纳秒，这里做个容错。
    function toDate(v) {
        const n = num(v, 0);
        if (!n) return null;
        let ms;
        if (n >= 1e16) ms = n / 1e6;
        else if (n >= 1e13) ms = n / 1e3;
        else if (n >= 1e11) ms = n;
        else ms = n * 1000;
        const d = new Date(ms);
        return isNaN(d.getTime()) ? null : d;
    }

    function fmtClock(v) {
        const d = toDate(v);
        return d ? pad2(d.getHours()) + ':' + pad2(d.getMinutes()) + ':' + pad2(d.getSeconds()) : '—';
    }

    function fmtDateTime(v) {
        const d = toDate(v);
        if (!d) return '—';
        return d.getFullYear() + '-' + pad2(d.getMonth() + 1) + '-' + pad2(d.getDate()) +
            ' ' + pad2(d.getHours()) + ':' + pad2(d.getMinutes()) + ':' + pad2(d.getSeconds());
    }

    function fmtDuration(sec) {
        const s = Math.max(0, Math.floor(num(sec, 0)));
        const parts = [
            [Math.floor(s / 86400), T.time.days],
            [Math.floor((s % 86400) / 3600), T.time.hours],
            [Math.floor((s % 3600) / 60), T.time.minutes],
            [s % 60, T.time.seconds]
        ].filter((p) => p[0] > 0);
        if (!parts.length) return '0' + T.time.seconds;
        return parts.slice(0, 2).map((p) => p[0] + p[1]).join(' ');
    }

    function isLoopback(addr) {
        const a = str(addr).trim().replace(/^\[/, '').replace(/\]$/, '');
        if (!a) return false;
        if (a === 'localhost' || a === '::1') return true;
        const m = /^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/.exec(a);
        if (!m) return false;
        const p = [m[1], m[2], m[3], m[4]].map(Number);
        if (p.some((x) => x > 255)) return false;
        return p[0] === 127;
    }

    function pkey(act, id) { return id ? act + ':' + id : act; }

    function busyText(key) { const p = App.pending[key]; return p ? p.text : ''; }
    function busyAttrs(key) { return App.pending[key] ? ' disabled aria-busy="true"' : ''; }
    function busyLabel(key, normalHtml) {
        const t = busyText(key);
        return t ? spinner() + '<span>' + esc(t) + '</span>' : normalHtml;
    }

    const ICONS = {
        power: '<path d="M12 3v9"/><path d="M6.6 6.6a8 8 0 1 0 10.8 0"/>',
        plus: '<path d="M12 5v14M5 12h14"/>',
        trash: '<path d="M4 7h16"/><path d="M9.5 7V4.8h5V7"/><path d="M6.5 7l1 13h9l1-13"/>',
        pencil: '<path d="M4 20h4L19 9a2.8 2.8 0 0 0-4-4L4 16z"/>',
        up: '<path d="M12 19V5"/><path d="M6 11l6-6 6 6"/>',
        down: '<path d="M12 5v14"/><path d="M6 13l6 6 6-6"/>',
        copy: '<rect x="9" y="9" width="11" height="11" rx="2"/><path d="M5 15V5a2 2 0 0 1 2-2h8"/>',
        test: '<path d="M13 2 4 14h6l-1 8 9-12h-6z"/>',
        zap: '<path d="M13 2 4 14h6l-1 8 9-12h-6z"/>',
        check: '<path d="M20 6 9 17l-5-5"/>',
        warn: '<path d="M12 4 2.5 20h19z"/><path d="M12 10v4M12 17h.01"/>',
        fail: '<circle cx="12" cy="12" r="9"/><path d="M15 9l-6 6M9 9l6 6"/>',
        info: '<circle cx="12" cy="12" r="9"/><path d="M12 11v5M12 8h.01"/>',
        refresh: '<path d="M20 11a8 8 0 1 0-2.3 5.7"/><path d="M20 5.5V11h-5.5"/>',
        folder: '<path d="M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z"/>',
        download: '<path d="M12 3v12"/><path d="M7 10l5 5 5-5"/><path d="M4 21h16"/>',
        servers: '<rect x="3" y="4" width="18" height="6.5" rx="2"/><rect x="3" y="13.5" width="18" height="6.5" rx="2"/>',
        eye: '<path d="M2.5 12S6 5.5 12 5.5 21.5 12 21.5 12 18 18.5 12 18.5 2.5 12 2.5 12z"/><circle cx="12" cy="12" r="3"/>',
        eyeOff: '<path d="M4 4l16 16"/><path d="M9.9 5.9A9.7 9.7 0 0 1 12 5.5c6 0 9.5 6.5 9.5 6.5a17.6 17.6 0 0 1-2.8 3.7"/><path d="M6.7 7.8A17.4 17.4 0 0 0 2.5 12S6 18.5 12 18.5c1.1 0 2.1-.2 3-.5"/>',
        shield: '<path d="M12 3l8 3v6c0 5-3.4 8.2-8 9.9C7.4 20.2 4 17 4 12V6z"/>',
        powerOff: '<path d="M12 3v9"/><path d="M6.6 6.6a8 8 0 1 0 10.8 0"/>',
        more: '<circle cx="5" cy="12" r="1.5"/><circle cx="12" cy="12" r="1.5"/><circle cx="19" cy="12" r="1.5"/>',
        activity: '<path d="M3 12h4l2.5-7 4 14 2.5-7H21"/>'
    };

    function icon(name, cls) {
        const body = ICONS[name] || ICONS.info;
        return '<svg aria-hidden="true" class="ico' + (cls ? ' ' + cls : '') + '" fill="none" stroke="currentColor"' +
            ' stroke-linecap="round" stroke-linejoin="round" stroke-width="1.7" viewBox="0 0 24 24">' + body + '</svg>';
    }

    function spinner() { return '<span aria-hidden="true" class="spinner"></span>'; }

    function secrets() {
        const out = [];
        const s = App.state;
        if (s) {
            if (s.server && s.server.password) out.push(s.server.password);
            arr(s.servers).forEach((v) => { if (v && v.password) out.push(v.password); });
        }
        if (App.editor && App.editor.view && App.editor.view.password) out.push(App.editor.view.password);
        return out.filter((x) => typeof x === 'string' && x.length >= 3);
    }

    // 纵深防御：任何进入界面/日志的文本都先抹掉已知密码。
    function scrub(text) {
        let s = str(text);
        secrets().forEach((sec) => { if (s.indexOf(sec) >= 0) s = s.split(sec).join('••••••'); });
        return s;
    }

    function toast(level, message) {
        const box = document.getElementById('toasts');
        const text = str(message);
        if (!box || !text) return;
        const lv = ['info', 'success', 'warn', 'error'].indexOf(str(level)) >= 0 ? level : 'info';
        const el = document.createElement('div');
        el.className = 'toast toast-' + lv;
        el.setAttribute('role', 'status');
        el.innerHTML = icon(lv === 'success' ? 'check' : (lv === 'info' ? 'info' : lv)) +
            '<span class="toast-msg">' + esc(scrub(text)) + '</span>';
        box.appendChild(el);
        while (box.children.length > TOAST_MAX) box.removeChild(box.firstChild);
        let dead = false;
        const kill = () => {
            if (dead) return;
            dead = true;
            el.classList.add('is-gone');
            window.setTimeout(() => { if (el.parentNode) el.parentNode.removeChild(el); }, 320);
        };
        el.addEventListener('click', kill);
        window.setTimeout(kill, TOAST_MS);
    }

    function defaultSettings() {
        return {
            socksAddress: '127.0.0.1', socksPort: DEFAULT_SOCKS_PORT,
            httpEnabled: true, httpAddress: '127.0.0.1', httpPort: DEFAULT_HTTP_PORT, httpFollowGlobal: false,
            udpEnabled: true,
            pacAddress: '127.0.0.1', pacPort: DEFAULT_PAC_PORT, pacBindLocalhost: true,
            gfwlistURL: DEFAULT_GFWLIST_URL, externalPACURL: '',
            exceptions: DEFAULT_EXCEPTIONS,
            autoConfigureNetworkServices: true, networkServices: [],
            enablePAC: true, enableGlobal: true, enableManual: false, enableExternalPAC: false,
            launchAtLogin: false, hideDockIcon: false, clearProxyOnQuit: true, restoreProxyOnWake: true
        };
    }

    function settingsView(raw) {
        const d = defaultSettings();
        const r = obj(raw);
        Object.keys(d).forEach((k) => {
            const v = r[k];
            if (v === undefined || v === null) return;
            if (Array.isArray(d[k])) d[k] = arr(v).map(str);
            else if (typeof d[k] === 'number') d[k] = num(v, d[k]);
            else if (typeof d[k] === 'boolean') d[k] = bool(v);
            else d[k] = str(v);
        });
        return d;
    }

    function listenerView(raw) {
        const r = obj(raw);
        return {address: str(r.address), port: num(r.port, 0), active: bool(r.active)};
    }

    function pacView(raw) {
        const r = obj(raw);
        return {
            address: str(r.address), port: num(r.port, 0), active: bool(r.active),
            url: str(r.url), ruleCount: num(r.ruleCount, 0), generatedAt: num(r.generatedAt, 0),
            source: str(r.source), lastError: str(r.lastError)
        };
    }

    function helperView(raw) {
        const r = obj(raw);
        return {
            path: str(r.path), installed: bool(r.installed), version: str(r.version),
            requiredVersion: str(r.requiredVersion), versionOK: bool(r.versionOK), setuid: bool(r.setuid),
            owner: str(r.owner), mode: str(r.mode), applied: arr(r.applied).map(str), lastError: str(r.lastError)
        };
    }

    function serverView(raw) {
        const r = obj(raw);
        const host = str(r.host);
        const port = num(r.port, 0);
        const remark = str(r.remark);
        return {
            id: str(r.id), host: host, port: port, method: str(r.method), password: str(r.password),
            remark: remark, plugin: str(r.plugin),
            title: str(r.title) || (remark.trim() !== '' ? remark.trim() : (host ? host + ':' + port : '')),
            hasPlugin: bool(r.hasPlugin) || str(r.plugin) !== '',
            active: bool(r.active), connected: bool(r.connected), lastError: str(r.lastError)
        };
    }

    function trafficView(raw) {
        const r = obj(raw);
        return {
            up: num(r.up, 0), down: num(r.down, 0),
            upSpeed: num(r.upSpeed, 0), downSpeed: num(r.downSpeed, 0),
            active: num(r.active, 0), sessions: num(r.sessions, 0)
        };
    }

    function logEntry(raw) {
        const r = obj(raw);
        return {
            seq: num(r.seq, 0), time: num(r.time, 0),
            level: str(r.level) || 'info', scope: str(r.scope), message: str(r.message)
        };
    }

    function checkView(raw) {
        const r = obj(raw);
        const st = str(r.status);
        return {
            name: str(r.name),
            status: ['ok', 'warn', 'fail', 'info'].indexOf(st) >= 0 ? st : 'info',
            detail: str(r.detail), suggestion: str(r.suggestion)
        };
    }

    function selfTestView(raw) {
        const r = obj(raw);
        return {
            ok: bool(r.ok), elapsedMs: num(r.elapsedMs, 0),
            steps: arr(r.steps).map((s) => {
                const x = obj(s);
                return {name: str(x.name), ok: bool(x.ok), detail: str(x.detail)};
            })
        };
    }

    function networkServiceView(raw) {
        const r = obj(raw);
        return {
            name: str(r.name), hardwarePort: str(r.hardwarePort), device: str(r.device),
            selected: bool(r.selected), hasOurProxy: bool(r.hasOurProxy)
        };
    }

    function normalizeState(raw) {
        const s = obj(raw);
        const prev = App.state;
        return {
            version: str(s.version) || (prev ? prev.version : ''),
            enabled: bool(s.enabled),
            mode: str(s.mode) || 'off',
            modes: arr(s.modes).map(str),
            socks: listenerView(s.socks),
            http: listenerView(s.http),
            udp: listenerView(s.udp),
            pac: pacView(s.pac),
            server: s.server ? serverView(s.server) : null,
            servers: arr(s.servers).map(serverView),
            traffic: trafficView(s.traffic),
            helper: helperView(s.helper),
            settings: settingsView(s.settings),
            lastError: str(s.lastError),
            startedAt: num(s.startedAt, 0)
        };
    }

    function state() { return App.state || normalizeState(null); }

    function cloneSettings(v) { return JSON.parse(JSON.stringify(v || defaultSettings())); }

    function ensureDraft() {
        if (!App.draft) App.draft = cloneSettings(state().settings);
        return App.draft;
    }

    function findServer(id) {
        return arr(state().servers).filter((v) => v.id === id)[0] || null;
    }

    function emptyServer() {
        return {
            id: '', host: '', port: 1080, method: METHODS[2] || 'aes-256-gcm',
            password: '', remark: '', plugin: '', title: '',
            hasPlugin: false, active: false, connected: false, lastError: ''
        };
    }

    function api() {
        return (window.go && window.go.main && window.go.main.App) || null;
    }

    function hasBackend() { return !!api(); }

    function errText(err, name) {
        let msg = '';
        if (typeof err === 'string') msg = err;
        else if (err && typeof err.message === 'string') msg = err.message;
        else if (err && typeof err.error === 'string') msg = err.error;
        if (!msg) msg = str(T.backend.unknownError);
        return name ? fill(T.backend.callFailed, name, msg) : msg;
    }

    function markBackendUp() {
        if (App.backendReady) return;
        App.backendReady = true;
        App.backendDetail = '';
        renderBackendBanner();
    }

    function markBackendDown(detail) {
        const d = str(detail);
        if (!App.backendReady && App.backendDetail === d) return;
        App.backendReady = false;
        App.backendDetail = d;
        renderBackendBanner();
    }

    function renderBackendBanner() {
        const box = document.getElementById('offline');
        if (box) box.hidden = App.backendReady;
        const det = document.getElementById('offline-detail');
        if (det) det.textContent = App.backendDetail ? fill(T.backend.missingMethod, App.backendDetail) : '';
    }

    let lastOfflineToast = 0;

    // 唯一的后端调用入口。返回 {ok, value}，永远 resolve。
    function call(name) {
        const args = Array.prototype.slice.call(arguments, 1);
        const a = api();
        if (!a || typeof a[name] !== 'function') {
            markBackendDown(name);
            const now = Date.now();
            if (now - lastOfflineToast > 2500) {
                lastOfflineToast = now;
                toast('warn', T.backend.notReadyShort);
            }
            return Promise.resolve({ok: false, value: undefined});
        }
        let p;
        try {
            p = a[name].apply(a, args);
        } catch (err) {
            toast('error', errText(err, name));
            return Promise.resolve({ok: false, value: undefined});
        }
        return Promise.resolve(p).then(
            (v) => { markBackendUp(); return {ok: true, value: v}; },
            (err) => { toast('error', errText(err, name)); return {ok: false, value: undefined}; }
        );
    }

    async function refreshState() {
        const r = await call('GetState');
        if (r.ok && r.value) S.applyState(r.value);
        else if (!App.state) S.renderView(true);
    }

    // 浮动 promise 统一兜底，保证任何情况下都不会出现 unhandled rejection
    function detach(p) {
        if (p && typeof p.catch === 'function') p.catch((err) => toast('error', errText(err)));
        return p;
    }

    function onEvent(name, cb) {
        try {
            const rt = window.runtime;
            if (rt && typeof rt.EventsOn === 'function') { rt.EventsOn(name, cb); return true; }
        } catch (err) { /* 忽略：当作后端不可用 */ }
        return false;
    }

    function deepGet(o, path) {
        return String(path).split('.').reduce((a, k) => (a === null || a === undefined ? undefined : a[k]), o);
    }

    function applyI18n(root) {
        $$('[data-i18n]', root).forEach((el) => {
            const v = deepGet(T, el.dataset.i18n);
            if (typeof v === 'string') el.textContent = v;
        });
        $$('[data-i18n-placeholder]', root).forEach((el) => {
            const v = deepGet(T, el.dataset.i18nPlaceholder);
            if (typeof v === 'string') el.setAttribute('placeholder', v);
        });
        $$('[data-i18n-title]', root).forEach((el) => {
            const v = deepGet(T, el.dataset.i18nTitle);
            if (typeof v === 'string') el.setAttribute('title', v);
        });
    }

    function viewHasFocus(name) {
        const sec = document.getElementById('view-' + name);
        const ae = document.activeElement;
        if (!sec || !ae) return false;
        if (!sec.contains(ae)) return false;
        return /^(INPUT|TEXTAREA|SELECT)$/.test(ae.tagName);
    }

    // 复制到剪贴板。三条路依次尝试，第一条是 Wails 原生剪贴板：
    // 界面是从 wails://wails/ 加载的，那不是"安全上下文"，因此 navigator.clipboard
    // 很可能压根不存在，只能落到 execCommand；而 execCommand 依赖用户手势、也已被
    // 标记为废弃。原生接口没有这两个问题。
    async function copyText(text) {
        const value = str(text);
        if (!value) return false;
        const rt = window.runtime;
        if (rt && typeof rt.ClipboardSetText === 'function') {
            try {
                await rt.ClipboardSetText(value);
                return true;
            } catch (err) { /* 回退到浏览器接口 */ }
        }
        try {
            if (navigator.clipboard && navigator.clipboard.writeText) {
                await navigator.clipboard.writeText(value);
                return true;
            }
        } catch (err) { /* 回退到 execCommand */ }
        try {
            const ta = document.createElement('textarea');
            ta.className = 'copy-buffer';
            ta.value = value;
            ta.setAttribute('readonly', 'readonly');
            document.body.appendChild(ta);
            // The buffer has to leave the DOM no matter how copying goes: an
            // early return or a throwing select()/execCommand() would otherwise
            // leave an offscreen textarea behind on the page.
            try {
                ta.select();
                return !!(document.execCommand && document.execCommand('copy'));
            } finally {
                ta.remove();
            }
        } catch (err) {
            return false;
        }
    }

    /* 对后面加载的脚本公开的接口 */
    Object.assign(S, {
        $, $$, App, DEFAULT_GFWLIST_URL, LOG_LIMIT, METHODS, MIRROR_GFWLIST_URL, MODES, T, VIEWS,
        applyI18n, arr, busyAttrs, busyLabel, call, checkView, cloneSettings, copyText, detach,
        emptyServer, ensureDraft, errText, esc, fill, findServer, fmtBytes, fmtClock,
        fmtDateTime, fmtDuration, fmtSpeed, hasBackend, helperView, icon, isLoopback, logEntry,
        markBackendDown, markBackendUp, networkServiceView, normalizeState, num, obj, onEvent,
        pacView, pkey, refreshState, renderBackendBanner, scrub, selfTestView, serverView, state,
        str, toast, trafficView, viewHasFocus
    });
})();
