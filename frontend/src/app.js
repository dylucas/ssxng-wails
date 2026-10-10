/*!
 * SSXNG — 前端逻辑（ES2020 普通脚本，无模块、无框架、无外部请求）
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

    /* =====================================================================
     * 0. 文案表
     * ===================================================================== */
    const T = window.T;
    if (!T) {
        const p = document.createElement('p');
        p.textContent = 'i18n.js missing / 文案表未加载';
        p.style.padding = '20px';
        document.body.appendChild(p);
        return;
    }

    /* =====================================================================
     * 1. 常量（与 Go 端保持一致）
     * ===================================================================== */
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

    /* =====================================================================
     * 2. 应用状态
     * ===================================================================== */
    const App = {
        state: null,          // normalizeState() 之后的 State
        view: 'status',
        backendReady: false,
        backendDetail: '',
        eventsBound: false,
        logs: [],             // LogEntry[]，最多 LOG_LIMIT 条（客户端镜像）
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

    /* =====================================================================
     * 3. 通用工具
     * ===================================================================== */
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

    /* =====================================================================
     * 4. 内联 SVG 图标
     * ===================================================================== */
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
        clock: '<circle cx="12" cy="12" r="8.5"/><path d="M12 7.5V12l3 2"/>',
        activity: '<path d="M3 12h4l2.5-7 4 14 2.5-7H21"/>'
    };

    function icon(name, cls) {
        const body = ICONS[name] || ICONS.info;
        return '<svg aria-hidden="true" class="ico' + (cls ? ' ' + cls : '') + '" fill="none" stroke="currentColor"' +
            ' stroke-linecap="round" stroke-linejoin="round" stroke-width="1.7" viewBox="0 0 24 24">' + body + '</svg>';
    }

    function spinner() { return '<span aria-hidden="true" class="spinner"></span>'; }

    /* =====================================================================
     * 5. Toast（后端错误、ssxng:toast 事件都走这里）
     * ===================================================================== */
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

    /* =====================================================================
     * 6. 状态规整（后端字段缺失/null 也不能让渲染崩）
     * ===================================================================== */
    function defaultSettings() {
        return {
            socksAddress: '127.0.0.1', socksPort: DEFAULT_SOCKS_PORT,
            httpEnabled: true, httpAddress: '127.0.0.1', httpPort: DEFAULT_HTTP_PORT, httpFollowGlobal: false,
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
            externalURL: str(r.externalURL), source: str(r.source), lastError: str(r.lastError)
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
            selected: bool(r.selected), hasOurProxy: bool(r.hasOurProxy),
            webProxy: str(r.webProxy), socksProxy: str(r.socksProxy), autoProxy: str(r.autoProxy)
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

    /* =====================================================================
     * 7. 后端访问：永不 reject，错误统一进 toast
     * ===================================================================== */
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
        if (r.ok && r.value) applyState(r.value);
        else if (!App.state) renderView(true);
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

    function bindEvents() {
        if (App.eventsBound) return 4;
        let n = 0;
        if (onEvent('ssxng:state', onStateEvent)) n++;
        if (onEvent('ssxng:log', onLogEvent)) n++;
        if (onEvent('ssxng:traffic', onTrafficEvent)) n++;
        if (onEvent('ssxng:toast', onToastEvent)) n++;
        App.eventsBound = n > 0;
        return n;
    }

    function onStateEvent(raw) {
        markBackendUp();
        applyState(raw);
    }

    function onTrafficEvent(raw) {
        // 每秒一次：只更新流量相关的 DOM，绝不重新拉取整个 state。
        if (!App.state) return;
        App.state.traffic = trafficView(raw);
        renderTraffic();
    }

    function onLogEvent(raw) {
        pushLog(logEntry(raw));
    }

    function onToastEvent(raw) {
        const t = obj(raw);
        toast(str(t.level) || 'info', str(t.message));
    }

    function applyState(raw) {
        const next = normalizeState(raw);
        App.state = next;
        if (!App.dirty) App.draft = cloneSettings(next.settings);
        renderBackendBanner();
        renderView(!viewHasFocus(App.view));
        renderSidebar();
    }

    /* =====================================================================
     * 8. 渲染
     * ===================================================================== */
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

    function renderSidebar() {
        const s = state();
        const ver = document.getElementById('app-version');
        if (ver) ver.textContent = s.version || '—';
        $$('#nav .nav-btn').forEach((b) => {
            const on = b.dataset.view === App.view;
            b.classList.toggle('is-active', on);
            b.setAttribute('aria-selected', on ? 'true' : 'false');
        });
        renderTraffic();
    }

    function setView(name) {
        App.menu = null;
        if (VIEWS.indexOf(name) < 0) return;
        App.view = name;
        renderSidebar();
        renderView(true);
        if (name === 'modes') detach(loadServices(false));
        if (name === 'logs') {
            App.follow = true;
            const list = document.getElementById('log-list');
            if (list) list.scrollTop = list.scrollHeight;
        }
    }

    function renderView(force) {
        const name = VIEWS.indexOf(App.view) >= 0 ? App.view : 'status';
        if (force === false) return; // 表单正在输入：保留 DOM，等下一次状态推送
        VIEWS.forEach((v) => {
            const sec = document.getElementById('view-' + v);
            if (sec) sec.hidden = (v !== name);
        });
        switch (name) {
            case 'servers': renderServers(); break;
            case 'modes': renderModes(); break;
            case 'pac': renderPAC(); break;
            case 'logs': renderLogs(); break;
            case 'diag': renderDiag(); break;
            default: renderStatus(); break;
        }
    }

    /* ---------------------------- 8.1 状态 ---------------------------- */
    function dotHtml(on) { return '<span class="dot ' + (on ? 'ok' : 'off') + '"></span>'; }

    function statHtml(id, label, value, cls) {
        return '<div class="stat' + (cls ? ' ' + esc(cls) : '') + '"><span class="stat-label">' + esc(label) +
            '</span><span class="stat-value" id="' + id + '">' + esc(value) + '</span></div>';
    }

    function kvHtml(label, valueHtml) {
        return '<div class="kv"><span>' + esc(label) + '</span><b>' + valueHtml + '</b></div>';
    }

    function modeLabel(m) {
        const v = T.mode ? T.mode[m] : '';
        return v || str(m);
    }

    function modeRadioHtml(s, m) {
        const available = s.modes.indexOf(m) >= 0;
        const checked = s.mode === m;
        return '<label class="radio' + (available ? '' : ' is-disabled') + (checked ? ' is-selected' : '') +
            '" data-act="set-mode" data-mode="' + esc(m) + '"' +
            ' title="' + esc(available ? '' : T.modesDisabledHint) + '">' +
            '<input type="radio" name="ssxng-mode" value="' + esc(m) + '"' + (checked ? ' checked' : '') +
            (available ? '' : ' disabled') + '/>' +
            '<span class="radio-dot"></span>' +
            '<span class="radio-text"><b>' + esc(modeLabel(m)) + '</b><i>' +
            esc((T.modeDesc && T.modeDesc[m]) || '') + '</i></span></label>';
    }

    // 顶部状态条：一眼看到"开没开、什么模式、走的哪个节点、出口是否通"，
    // 右侧只放一个跟当前状态相关的主动作（没节点 → 去添加；开着 → 关闭）。
    function heroHtml(s) {
        const on = s.enabled;
        const sv = s.server;
        const stateText = s.lastError ? T.status.stateError : (on ? T.status.running : T.status.stopped);
        const ico = s.lastError ? 'warn' : (on ? 'shield' : 'powerOff');

        const h = ['<section class="hero' + (on ? ' is-on' : ' is-off') + '">',
            '<div class="hero-ico">' + icon(ico) + '</div>',
            '<div class="hero-text">',
            '<div class="hero-state">' + dotHtml(on && !s.lastError) + '<span>' + esc(stateText) + '</span></div>',
            '<div class="hero-sub">' + esc(modeLabel(s.mode)) +
            '<span class="hero-sum">' + (sv
                ? esc(sv.title || (sv.host + ':' + sv.port)) + '<span class="sep">·</span>' + esc(sv.host + ':' + sv.port)
                : '<span class="is-none">' + esc(T.status.noServer) + '</span>') + '</span></div>',
            '</div>',
            '<div class="hero-actions">',
            '<button type="button" class="btn" data-act="goto-modes">' + icon('activity') + '<span>' +
            esc(T.status.changeMode) + '</span></button>'];

        if (!sv) {
            h.push('<button type="button" class="btn btn-primary" data-act="goto-servers">' + icon('plus') +
                '<span>' + esc(T.status.addServer) + '</span></button>');
        } else if (on) {
            const key = pkey('toggle-power');
            h.push('<button type="button" class="btn btn-danger btn-off" data-act="toggle-power"' + busyAttrs(key) + '>' +
                busyLabel(key, icon('powerOff') + '<span>' + esc(T.status.powerOff) + '</span>') + '</button>');
        } else {
            const key = pkey('toggle-power');
            h.push('<button type="button" class="btn btn-primary" data-act="toggle-power"' + busyAttrs(key) + '>' +
                busyLabel(key, icon('power') + '<span>' + esc(T.status.powerOn) + '</span>') + '</button>');
        }
        h.push('</div></section>');

        if (s.lastError) h.push('<div class="err-line hero-err">' + esc(s.lastError) + '</div>');
        return h.join('');
    }

    // 关键事实一行排列：省掉"监听端口"卡片里的逐行堆叠
    function factsHtml(s) {
        const facts = [
            {label: T.status.socks,
                value: str(s.socks.address || '—') + ':' + num(s.socks.port, 0),
                on: s.enabled && s.socks.active},
            {label: T.status.http,
                value: s.settings.httpEnabled ? str(s.http.address || '—') + ':' + num(s.http.port, 0) : T.common.disabled,
                on: s.enabled && s.settings.httpEnabled && s.http.active},
            {label: T.status.pac,
                value: str(s.pac.address || '—') + ':' + num(s.pac.port, 0),
                on: s.enabled && s.mode === 'auto' && s.pac.active},
            {label: T.status.uptime,
                value: s.startedAt ? fmtDuration(Date.now() / 1000 - s.startedAt) : '—',
                title: s.startedAt ? T.status.startedAt + ' ' + fmtDateTime(s.startedAt) : '',
                on: s.enabled}
        ];
        return '<div class="facts">' + facts.map((f) =>
            '<div class="fact' + (f.on ? ' is-on' : '') + '"' +
            (f.title ? ' title="' + esc(f.title) + '"' : '') + '>' +
            '<span class="fact-label">' + esc(f.label) + '</span>' +
            '<span class="fact-value mono">' + esc(f.value) + '</span></div>').join('') + '</div>';
    }

    function currentServerHtml(s, sv) {
        const h = [];
        if (!sv) {
            h.push('<p class="card-sub">' + esc(T.status.noServerHint) + '</p>');
            h.push('<div class="card-actions"><button type="button" class="btn btn-primary" data-act="goto-servers">' +
                icon('servers') + '<span>' + esc(T.status.pick) + '</span></button></div>');
            return h.join('');
        }
        const key = pkey('test-server', sv.id);
        const keyCopy = pkey('copy-current-url');
        const connected = s.enabled && sv.connected;
        h.push('<div class="server-line"><span class="title">' + esc(sv.title || (sv.host + ':' + sv.port)) + '</span>' +
            '<span class="chip mono">' + esc(sv.method || '?') + '</span>' +
            (sv.hasPlugin ? '<span class="chip chip-warn">' + esc(T.servers.pluginWarn) + '</span>' : '') +
            '<span class="chip ' + (connected ? 'chip-ok' : '') + '">' + dotHtml(connected) + ' ' +
            esc(connected ? T.common.active : T.common.inactive) + '</span></div>');
        h.push('<div class="kv"><span>' + esc(T.status.currentServer) + '</span><b class="mono">' +
            esc(sv.host + ':' + sv.port) + '</b></div>');
        if (sv.lastError) h.push('<div class="err-line">' + esc(T.status.lastError) + '：' + esc(sv.lastError) + '</div>');
        h.push('<div class="card-actions">');
        h.push('<button type="button" class="btn btn-sm" data-act="test-server" data-id="' + esc(sv.id) + '"' +
            busyAttrs(key) + '>' + busyLabel(key, icon('test') + '<span>' + esc(T.status.testConn) + '</span>') + '</button>');
        h.push('<button type="button" class="btn btn-sm" data-act="copy-current-url"' + busyAttrs(keyCopy) + '>' +
            busyLabel(keyCopy, icon('copy') + '<span>' + esc(T.status.copyURL) + '</span>') + '</button>');
        h.push('<button type="button" class="btn btn-sm" data-act="goto-servers">' + icon('pencil') + '<span>' +
            esc(T.common.edit) + '</span></button>');
        h.push('</div>');
        return h.join('');
    }

    function recentProxyHtml() {
        const rows = App.logs.filter((e) => e.scope === 'proxy').slice(-6).reverse();
        if (!rows.length) return '<p class="hint">' + esc(T.status.recentProxyEmpty) + '</p>';
        return '<div class="list is-tight">' + rows.map((e) =>
            '<div class="row"><div class="row-text">' +
            '<div class="row-title"><span class="mono muted">' + esc(fmtClock(e.time)) + '</span></div>' +
            '<div class="row-sub">' + esc(scrub(e.message)) + '</div>' +
            '</div></div>').join('') + '</div>';
    }

    function helperHtml(h, s) {
        const out = [];
        // is-wide：卡片数量是奇数，让它跨整行，右边不留半行空洞
        out.push('<section class="card is-wide"><h2 class="card-title">' + esc(T.status.helper) + '</h2>');
        out.push('<div class="kv-grid">');
        out.push('<div class="server-line"><span class="title">' +
            esc(h.installed ? T.status.helperInstalled : T.status.helperMissing) + '</span>' +
            (h.installed
                ? '<span class="chip ' + (h.versionOK ? 'chip-ok' : 'chip-warn') + '">' + esc(T.status.helperVersion) +
                ' ' + esc(h.version || '?') + '</span>'
                : '<span class="chip chip-warn">' + esc(T.status.helperMissing) + '</span>') + '</div>');

        if (!h.installed) out.push('<p class="hint">' + esc(T.status.helperHintMissing) + '</p>');
        else if (!h.versionOK) out.push('<p class="hint">' + esc(T.status.helperHintOutdated) + '</p>');
        else if (!h.setuid) out.push('<p class="hint">' + esc(T.status.helperHintBadPerm) + '</p>');

        if (h.path) out.push(kvHtml(T.status.helperPath, '<span class="mono">' + esc(h.path) + '</span>'));
        if (h.installed) {
            out.push(kvHtml(T.status.helperRequired, '<span class="mono">' + esc(h.requiredVersion || '?') + '</span>'));
            out.push(kvHtml(T.status.helperOwner, '<span class="mono">' + esc(h.owner || '?') + '</span>'));
            out.push(kvHtml(T.status.helperMode, '<span class="mono">' + esc(h.mode || '?') + '</span>'));
            out.push(kvHtml(T.status.helperSetuid, esc(h.setuid ? T.common.yes : T.common.no)));
        }
        if (h.applied && h.applied.length) out.push(kvHtml(T.status.helperApplied, esc(h.applied.join(' · '))));
        out.push('</div>');
        if (h.lastError) out.push('<div class="err-line">' + esc(h.lastError) + '</div>');
        // 启动时间/已运行由顶部"关键事实"承载，这里不再重复（s 仍用于下面的判断）
        out.push('<div class="card-actions">');
        if (!h.installed) {
            out.push('<button type="button" class="btn btn-sm btn-primary" data-act="install-helper"' +
                busyAttrs('install-helper') + '>' +
                busyLabel('install-helper', icon('shield') + '<span>' + esc(T.status.installHelper) + '</span>') + '</button>');
        }
        out.push('<button type="button" class="btn btn-sm" data-act="helper-refresh"' + busyAttrs('helper-refresh') + '>' +
            busyLabel('helper-refresh', icon('refresh') + '<span>' + esc(T.common.refresh) + '</span>') + '</button>');
        out.push('</div></section>');
        return out.join('');
    }

    function renderStatus() {
        const body = document.getElementById('body-status');
        if (!body) return;
        const s = state();
        const sv = s.server;
        const t = s.traffic;
        const h = [];

        // 1) 顶部状态条 + 关键事实
        h.push(heroHtml(s));
        h.push(factsHtml(s));

        // 2) 流量（当前速度放前面：这是用户最关心的数字）
        h.push('<section class="card"><h2 class="card-title">' + esc(T.status.traffic) + '</h2>');
        h.push('<div class="stats">');
        h.push(statHtml('t-upspeed', T.status.up + ' · ' + T.status.speed, fmtSpeed(t.upSpeed), 't-upspeed'));
        h.push(statHtml('t-downspeed', T.status.down + ' · ' + T.status.speed, fmtSpeed(t.downSpeed), 't-downspeed'));
        h.push(statHtml('t-up', T.status.up + ' · ' + T.status.total, fmtBytes(t.up)));
        h.push(statHtml('t-down', T.status.down + ' · ' + T.status.total, fmtBytes(t.down)));
        h.push(statHtml('t-active', T.status.activeConns, String(t.active)));
        h.push(statHtml('t-sessions', T.status.sessions, String(t.sessions)));
        h.push('</div></section>');

        // 3) 下面两栏
        h.push('<div class="grid grid-2">');
        h.push('<section class="card"><h2 class="card-title">' + esc(T.status.currentServer) + '</h2>' +
            currentServerHtml(s, sv) + '</section>');
        h.push('<section class="card"><h2 class="card-title">' + esc(T.status.recentProxy) + '</h2>' +
            '<div id="recent-proxy">' + recentProxyHtml() + '</div></section>');
        h.push(helperHtml(s.helper, s));
        h.push('</div>');

        body.innerHTML = h.join('');
        renderTraffic();
    }

    function renderTraffic() {
        const t = App.state ? App.state.traffic : trafficView(null);
        const set = (id, v) => {
            const el = document.getElementById(id);
            if (el) el.textContent = v;
        };
        set('t-up', fmtBytes(t.up));
        set('t-down', fmtBytes(t.down));
        set('t-upspeed', fmtSpeed(t.upSpeed));
        set('t-downspeed', fmtSpeed(t.downSpeed));
        set('t-active', String(t.active));
        set('t-sessions', String(t.sessions));
        set('mini-up', fmtSpeed(t.upSpeed));
        set('mini-down', fmtSpeed(t.downSpeed));
    }

    /* ---------------------------- 8.2 服务器 ---------------------------- */
    function serverRowHtml(v, index, total) {
        const keyTest = pkey('test-server', v.id);
        const h = [];
        h.push('<div class="row server-row' + (v.active ? ' is-active' : '') + '">');
        h.push('<span class="row-mark" title="' + esc(v.active ? T.common.selected : '') + '">' +
            (v.active ? icon('check') : '') + '</span>');
        h.push('<div class="row-text">');
        h.push('<div class="row-title"><b>' + esc(v.title || (v.host + ':' + v.port)) + '</b>' +
            '<span class="chip mono">' + esc(v.method || '?') + '</span>' +
            (v.hasPlugin ? '<span class="chip chip-warn">' + esc(T.servers.pluginWarn) + '</span>' : '') +
            (v.connected ? '<span class="chip chip-ok">' + esc(T.common.active) + '</span>' : '') + '</div>');
        h.push('<div class="row-sub mono">' + esc(v.host + ':' + v.port) + '<span class="sep">·</span>' +
            esc(T.servers.passwordSaved) + '</div>');
        if (v.lastError) h.push('<div class="row-err">' + esc(v.lastError) + '</div>');
        h.push('</div>');
        // 常驻只留最常用的 3 个 + 危险操作；上移/下移/复制 ss:// 收进"更多"。
        // 7 个按钮固定占 414px，窄窗下会把标题挤到 134px、行高撑到 103px。
        h.push('<div class="row-actions">');
        h.push('<button type="button" class="btn btn-sm" data-act="select-server" data-id="' + esc(v.id) + '"' +
            (v.active ? ' disabled' : '') + '>' + icon('check') + '<span>' +
            esc(v.active ? T.common.selected : T.common.select) + '</span></button>');
        h.push('<button type="button" class="btn btn-sm" data-act="test-server" data-id="' + esc(v.id) + '"' +
            busyAttrs(keyTest) + '>' + busyLabel(keyTest, icon('test') + '<span>' + esc(T.servers.testConn) + '</span>') + '</button>');
        h.push('<button type="button" class="btn btn-sm" data-act="edit-server" data-id="' + esc(v.id) + '">' +
            icon('pencil') + '<span>' + esc(T.common.edit) + '</span></button>');
        h.push('<button type="button" class="btn btn-sm btn-danger" data-act="delete-server" data-id="' + esc(v.id) +
            '">' + icon('trash') + '<span>' + esc(T.common.del) + '</span></button>');
        const open = !!App.menu && App.menu.id === v.id;
        h.push('<button type="button" class="btn btn-sm btn-icon" data-act="row-more" data-id="' + esc(v.id) +
            '" title="' + esc(T.servers.moreActions) + '" aria-haspopup="true" aria-expanded="' +
            (open ? 'true' : 'false') + '">' + icon('more') + '</button>');
        h.push('</div>');
        if (open) h.push(moreMenuHtml(v, index, total));
        h.push('</div>');
        return h.join('');
    }

    // 行内"更多"菜单：菜单项都是普通 data-act 按钮，复用既有动作，不新增后端调用
    function moreMenuHtml(v, index, total) {
        const item = (act, delta, label, ico, disabled) => {
            const busy = !!App.pending[pkey(act, v.id)];
            return '<button type="button" class="menu-item" role="menuitem" data-act="' + act +
                '" data-id="' + esc(v.id) + '"' + (delta ? ' data-delta="' + delta + '"' : '') +
                (disabled || busy ? ' disabled' : '') + '>' + icon(ico) + '<span>' + esc(label) + '</span></button>';
        };
        return '<div class="menu" role="menu" data-menu="' + esc(v.id) + '">' +
            item('move-server', '-1', T.common.moveUp, 'up', index === 0) +
            item('move-server', '1', T.common.moveDown, 'down', index === total - 1) +
            item('export-server', '', T.servers.copyURL, 'copy', false) +
            '</div>';
    }

    function editorHtml() {
        const ed = App.editor;
        if (!ed) return '';
        const v = ed.view;
        const isNew = ed.mode === 'new';
        // 与服务端 id 为空时 onClick 推导出的 key 保持一致
        const key = ed.mode === 'new' ? 'server-save' : pkey('server-save', v.id);
        const opts = METHODS.map((m) => '<option value="' + esc(m) + '"' + (m === v.method ? ' selected' : '') +
            '>' + esc(m) + '</option>').join('');
        const extra = (v.method && METHODS.indexOf(v.method) < 0)
            ? '<option value="' + esc(v.method) + '" selected>' + esc(v.method) + '</option>' : '';
        const h = [];
        h.push('<section class="card editor" id="server-editor">');
        h.push('<h2 class="card-title">' + esc(isNew ? T.servers.add : T.servers.edit) + '</h2>');
        h.push('<div class="form-grid">');
        h.push('<label class="field"><span class="field-label">' + esc(T.servers.remark) + '</span>' +
            '<input class="input" type="text" data-ed="remark" value="' + esc(v.remark) +
            '" placeholder="' + esc(T.servers.remarkPlaceholder) + '" autocomplete="off"/></label>');
        h.push('<label class="field"><span class="field-label">' + esc(T.servers.host) + '</span>' +
            '<input class="input mono" type="text" data-ed="host" value="' + esc(v.host) +
            '" placeholder="' + esc(T.servers.hostPlaceholder) + '" autocomplete="off" spellcheck="false"/></label>');
        h.push('<label class="field"><span class="field-label">' + esc(T.servers.port) + '</span>' +
            '<input class="input mono" type="number" data-ed="port" data-type="number" min="1" max="65535" value="' +
            esc(v.port || '') + '"/></label>');
        h.push('<label class="field"><span class="field-label">' + esc(T.servers.method) + '</span>' +
            '<select class="input mono" data-ed="method">' + opts + extra + '</select></label>');
        // 密码：值只用 DOM 属性写入，绝不进 HTML 属性 / data-* / 日志
        h.push('<label class="field"><span class="field-label">' + esc(T.servers.password) + '</span>' +
            '<span class="input-wrap"><input class="input mono" id="ed-password" type="password" data-ed="password"' +
            ' value="" placeholder="' + esc(T.servers.passwordPlaceholder) +
            '" autocomplete="new-password" spellcheck="false"/>' +
            '<button type="button" class="btn btn-icon" data-act="toggle-password" title="' +
            esc(T.servers.showPassword) + '">' + icon('eye') + '</button></span></label>');
        h.push('</div>');
        if (v.hasPlugin) h.push('<p class="hint">' + esc(T.servers.pluginNotice) + '</p>');
        h.push('<div class="form-errors" id="editor-errors"></div>');
        h.push('<div class="card-actions">');
        h.push('<button type="button" class="btn btn-primary" data-act="server-save" data-id="' + esc(v.id) + '"' +
            busyAttrs(key) + '>' + busyLabel(key, icon('check') + '<span>' + esc(T.common.save) + '</span>') + '</button>');
        h.push('<button type="button" class="btn" data-act="editor-cancel">' + '<span>' +
            esc(T.common.cancel) + '</span></button>');
        h.push('</div></section>');
        return h.join('');
    }

    function importHtml() {
        const key = 'import-ss';
        const busy = !!App.pending[key];
        return '<section class="card"><h2 class="card-title">' + esc(T.servers.importTitle) + '</h2>' +
            '<div class="toolbar">' +
            '<input class="input mono grow" id="import-raw" type="text" data-ui="importRaw" value=""' +
            ' placeholder="' + esc(T.servers.importPlaceholder) + '" autocomplete="off" spellcheck="false"/>' +
            '<button type="button" class="btn btn-primary" data-act="import-ss"' +
            (busy ? ' disabled aria-busy="true"' : '') + '>' +
            busyLabel(key, icon('download') + '<span>' + esc(T.servers.doImport) + '</span>') + '</button>' +
            '</div></section>';
    }

    function renderServers() {
        const body = document.getElementById('body-servers');
        if (!body) return;
        const s = state();
        const list = s.servers;
        const h = [];
        h.push('<div class="toolbar">');
        h.push('<button type="button" class="btn btn-primary" data-act="add-server">' + icon('plus') +
            '<span>' + esc(T.servers.add) + '</span></button>');
        h.push('<span class="grow"></span>');
        h.push('<span class="muted">' + esc(fill(T.servers.count, list.length)) + '</span>');
        h.push('</div>');
        if (!list.length) {
            h.push('<div class="card empty-card">' + esc(T.servers.listEmpty) + '</div>');
        } else {
            h.push('<div class="card list">' +
                list.map((v, i) => serverRowHtml(v, i, list.length)).join('') + '</div>');
        }
        h.push(editorHtml());
        h.push(importHtml());
        body.innerHTML = h.join('');

        // 密码/导入内容用 DOM 属性回填，避免出现在 HTML 属性里
        const pw = document.getElementById('ed-password');
        if (pw && App.editor) pw.value = str(App.editor.view.password);
        const imp = document.getElementById('import-raw');
        if (imp) imp.value = App.importRaw;
        refreshEditorFeedback();
    }

    function refreshEditorFeedback() {
        const box = document.getElementById('editor-errors');
        if (!box) return;
        const ed = App.editor;
        if (!ed) { box.innerHTML = ''; return; }
        const msgs = [];
        const err = validateServer(ed.view);
        if (err) msgs.push(err);
        else if (num(ed.view.port, 0) > 0 && num(ed.view.port, 0) < 1024) msgs.push(T.servers.errPortLow);
        box.innerHTML = msgs.map((m) => '<div class="form-error">' + esc(m) + '</div>').join('');
    }

    /* ---------------------------- 8.3 模式与代理 ---------------------------- */
    function fieldHtml(key, label, opts) {
        opts = opts || {};
        const d = ensureDraft();
        const v = (d[key] === undefined || d[key] === null) ? '' : d[key];
        const cls = 'input' + (opts.mono ? ' mono' : '');
        const attrs = [];
        if (opts.type === 'number') {
            attrs.push('type="number"');
            if (opts.min !== undefined) attrs.push('min="' + opts.min + '"');
            if (opts.max !== undefined) attrs.push('max="' + opts.max + '"');
        } else {
            attrs.push('type="' + (opts.type || 'text') + '"');
        }
        return '<label class="field"><span class="field-label">' + esc(label) + '</span>' +
            '<input class="' + cls + '" data-key="' + esc(key) + '" data-type="' +
            esc(opts.type || 'text') + '" value="' + esc(v) + '"' +
            (opts.placeholder ? ' placeholder="' + esc(opts.placeholder) + '"' : '') +
            ' autocomplete="off" spellcheck="false" ' + attrs.join(' ') + '/>' +
            (opts.hint ? '<span class="field-hint">' + esc(opts.hint) + '</span>' : '') + '</label>';
    }

    function checkFieldHtml(key, label, hint) {
        const d = ensureDraft();
        return '<div><label class="check"><input type="checkbox" data-key="' + esc(key) + '" data-type="bool"' +
            (d[key] ? ' checked' : '') + '/><span>' + esc(label) + '</span></label>' +
            (hint ? '<p class="field-hint">' + esc(hint) + '</p>' : '') + '</div>';
    }

    function servicesHtml() {
        const d = ensureDraft();
        const auto = !!d.autoConfigureNetworkServices;
        const h = [];
        // "自动选择"只有一个开关，单独占一张卡片太浪费：并进网络服务卡做分组
        h.push('<section class="card"><h2 class="card-title">' + esc(T.modes.network) + '</h2>');
        h.push('<div class="check-list">' +
            checkFieldHtml('autoConfigureNetworkServices', T.modes.autoConfig) + '</div>');
        if (auto) h.push('<p class="hint">' + esc(T.modes.autoConfigHint) + '</p>');
        h.push('<div class="toolbar"><span class="muted">' + esc(T.modes.services) + '</span>' +
            '<button type="button" class="btn btn-sm" data-act="services-refresh"' +
            busyAttrs('services-refresh') + '>' +
            busyLabel('services-refresh', icon('refresh') + '<span>' + esc(T.modes.servicesRefresh) + '</span>') +
            '</button></div>');
        if (App.servicesLoading) {
            h.push('<p class="hint">' + esc(T.modes.servicesLoading) + '</p>');
        } else if (!App.services.length) {
            h.push('<p class="hint">' + esc(T.modes.servicesEmpty) + '</p>');
        } else {
            h.push('<div class="check-list">' + App.services.map((svc) => {
                const on = (arr(d.networkServices).indexOf(svc.name) >= 0) || svc.selected;
                return '<label class="check' + (auto ? ' is-disabled' : '') + '">' +
                    '<input type="checkbox" data-svc="' + esc(svc.name) + '"' + (on ? ' checked' : '') +
                    (auto ? ' disabled' : '') + '/>' +
                    '<span>' + esc(svc.name) + ' <i class="muted mono">' +
                    esc(svc.hardwarePort || svc.device || '') + '</i></span>' +
                    (svc.hasOurProxy ? '<span class="chip chip-ok">' + esc(T.common.active) + '</span>' : '') +
                    '</label>';
            }).join('') + '</div>');
        }
        h.push('</section>');
        return h.join('');
    }

    /* ------------------ 8.3 命令行代理（export 命令） ------------------ */

    // 只复制"当前实际生效"的端口：这里读的是已保存的 settings，而不是草稿。
    // 用户改了端口还没保存时，监听并没有变，命令也就不该跟着变。
    function cliProxyCommands() {
        const saved = obj(state().settings);
        const host = (v, fallback) => str(v) || fallback;
        const rows = [];
        const pair = (scheme, h, p) => 'export http_proxy=' + scheme + '://' + h + ':' + p +
            ';export https_proxy=' + scheme + '://' + h + ':' + p + ';';

        // HTTP 放在前面：与 SSX-NG 的"Copy HTTP Proxy Shell Export Command"一致。
        if (saved.httpEnabled) {
            rows.push({
                id: 'http',
                label: T.modes.cliHTTP,
                command: pair('http', host(saved.httpAddress, '127.0.0.1'), num(saved.httpPort, 1087)),
            });
        }
        rows.push({
            id: 'socks',
            label: T.modes.cliSocks,
            command: pair('socks5', host(saved.socksAddress, '127.0.0.1'), num(saved.socksPort, 1086)),
        });
        return rows;
    }

    // 监听地址在草稿里被改过（还没保存）时，命令与界面上的端口会对不上，得说清楚。
    function cliProxyStale() {
        if (!App.dirty || !App.draft) return false;
        const saved = obj(state().settings);
        const d = App.draft;
        return ['socksAddress', 'socksPort', 'httpAddress', 'httpPort', 'httpEnabled'].some((k) => {
            const a = d[k];
            const b = saved[k];
            return typeof b === 'number' || typeof a === 'number' ? num(a) !== num(b) : str(a) !== str(b);
        });
    }

    function cliProxyHtml() {
        const rows = cliProxyCommands();
        const on = !!state().enabled;
        const h = ['<div class="cli-block">'];
        h.push('<h3 class="cli-title">' + esc(T.modes.cliProxy) + '</h3>');
        h.push('<p class="hint">' + esc(T.modes.cliProxyHint) + '</p>');
        rows.forEach((r) => {
            h.push('<div class="cli-row" data-cli="' + esc(r.id) + '">');
            h.push('<span class="cli-label">' + esc(r.label) + '</span>');
            h.push('<code class="cli-cmd" data-cli-cmd="' + esc(r.id) + '">' + esc(r.command) + '</code>');
            h.push('<button type="button" class="btn btn-sm" data-act="copy-cli-proxy" data-id="' + esc(r.id) + '"' +
                ' title="' + esc(r.command) + '">' + icon('copy') + '<span>' + esc(T.modes.cliCopy) + '</span></button>');
            h.push('</div>');
        });
        if (!rows.some((r) => r.id === 'http')) {
            h.push('<p class="hint">' + esc(T.modes.cliHTTPOff) + '</p>');
        }
        // 只有在"需要提醒"时才占一行：开启且没改端口时不必重复说"可以直接用"。
        const notes = [];
        if (!on) notes.push(T.modes.cliOff);
        if (cliProxyStale()) notes.push(T.modes.cliStale);
        if (notes.length) h.push('<p class="hint cli-note">' + esc(notes.join(' ')) + '</p>');
        h.push('</div>');
        return h.join('');
    }

    function renderModes() {
        const body = document.getElementById('body-modes');
        if (!body) return;
        const d = ensureDraft();
        const h = [];

        h.push('<div class="toolbar">');
        h.push('<button type="button" class="btn btn-primary" data-act="settings-save"' + busyAttrs('settings-save') +
            '>' + busyLabel('settings-save', icon('check') + '<span>' + esc(T.common.save) + '</span>') + '</button>');
        h.push('<button type="button" class="btn btn-accent" data-act="settings-apply"' + busyAttrs('settings-apply') +
            '>' + busyLabel('settings-apply', icon('zap') + '<span>' + esc(T.common.saveApply) + '</span>') + '</button>');
        h.push('<button type="button" class="btn" data-act="settings-reload">' + icon('refresh') + '<span>' +
            esc(T.common.reload) + '</span></button>');
        h.push('<span class="grow"></span>');
        h.push('<span class="muted" id="settings-dirty">' + (App.dirty ? esc(T.common.dirtyNotice) : '') + '</span>');
        h.push('</div>');
        h.push('<div class="form-errors" id="settings-errors"></div>');

        // 当前生效的模式。放在这里而不是状态页：状态页负责"看"，
        // 模式与代理页负责"配"，切换模式走这里或菜单栏图标。
        h.push('<section class="card"><h2 class="card-title">' + esc(T.status.mode) + '</h2>' +
            '<div class="mode-list">' + MODES.map((m) => modeRadioHtml(state(), m)).join('') + '</div>' +
            '<p class="hint">' + esc(T.modes.activeHint) + '</p></section>');

        h.push('<section class="card"><h2 class="card-title">' + esc(T.modes.listen) + '</h2><div class="form-grid">');
        h.push(fieldHtml('socksAddress', T.modes.socksAddress, {mono: true}));
        h.push(fieldHtml('socksPort', T.modes.socksPort, {mono: true, type: 'number', min: 1024, max: 65535}));
        h.push(checkFieldHtml('httpEnabled', T.modes.httpEnabled));
        h.push(fieldHtml('httpAddress', T.modes.httpAddress, {mono: true}));
        h.push(fieldHtml('httpPort', T.modes.httpPort, {mono: true, type: 'number', min: 1024, max: 65535}));
        h.push(checkFieldHtml('httpFollowGlobal', T.modes.httpFollowGlobal));
        h.push(fieldHtml('pacAddress', T.modes.pacAddress, {mono: true}));
        h.push(fieldHtml('pacPort', T.modes.pacPort, {mono: true, type: 'number', min: 1024, max: 65535}));
        h.push(checkFieldHtml('pacBindLocalhost', T.modes.pacBindLocalhost));
        h.push('</div>');
        h.push(cliProxyHtml());
        h.push('</section>');

        h.push('<section class="card"><h2 class="card-title">' + esc(T.modes.switchModes) + '</h2>' +
            '<div class="check-list">' +
            checkFieldHtml('enablePAC', T.modes.enablePAC) +
            checkFieldHtml('enableGlobal', T.modes.enableGlobal) +
            checkFieldHtml('enableManual', T.modes.enableManual) +
            checkFieldHtml('enableExternalPAC', T.modes.enableExternalPAC) +
            '</div></section>');

        h.push(servicesHtml());

        h.push('<section class="card"><h2 class="card-title">' + esc(T.modes.exceptions) + '</h2>' +
            '<div class="field"><textarea class="input" rows="4" data-key="exceptions" spellcheck="false">' +
            esc(d.exceptions) + '</textarea>' +
            '<span class="field-hint">' + esc(T.modes.exceptionsHint) + '</span></div></section>');

        h.push('<section class="card"><h2 class="card-title">' + esc(T.modes.quirks) + '</h2>' +
            '<div class="check-list">' +
            checkFieldHtml('launchAtLogin', T.modes.launchAtLogin) +
            checkFieldHtml('hideDockIcon', T.modes.hideDockIcon) +
            checkFieldHtml('clearProxyOnQuit', T.modes.clearProxyOnQuit) +
            checkFieldHtml('restoreProxyOnWake', T.modes.restoreOnWake) +
            '</div></section>');

        body.innerHTML = h.join('');
        refreshSettingsFeedback();
    }

    /* ---------------------------- 8.4 PAC ---------------------------- */
    function renderPAC() {
        const body = document.getElementById('body-pac');
        if (!body) return;
        const s = state();
        const p = s.pac;
        const h = [];

        h.push('<div class="toolbar">');
        h.push('<button type="button" class="btn btn-primary" data-act="pac-update"' + busyAttrs('pac-update') + '>' +
            busyLabel('pac-update', icon('download') + '<span>' + esc(T.pac.update) + '</span>') + '</button>');
        h.push('<button type="button" class="btn" data-act="pac-refresh"' + busyAttrs('pac-refresh') + '>' +
            busyLabel('pac-refresh', icon('refresh') + '<span>' + esc(T.pac.refreshStatus) + '</span>') + '</button>');
        h.push('<button type="button" class="btn" data-act="pac-userrule">' + icon('pencil') + '<span>' +
            esc(T.pac.editUserRule) + '</span></button>');
        h.push('<button type="button" class="btn" data-act="open-folder">' + icon('folder') + '<span>' +
            esc(T.pac.openFolder) + '</span></button>');
        h.push('</div>');

        h.push('<section class="card"><h2 class="card-title">' + esc(T.pac.server) + '</h2>');
        h.push(kvHtml(T.pac.status, dotHtml(p.active) + ' ' +
            esc(p.active ? T.common.active : T.common.inactive)));
        h.push(kvHtml(T.pac.url, p.url ? '<span class="mono">' + esc(p.url) + '</span>' : esc(T.common.none)));
        h.push(kvHtml(T.pac.ruleCount, '<span class="mono">' + esc(String(p.ruleCount)) + '</span>'));
        h.push(kvHtml(T.pac.generatedAt, '<span class="mono">' +
            esc(p.generatedAt ? fmtDateTime(p.generatedAt) : T.pac.never) + '</span>'));
        const src = p.source || T.common.unknown;
        h.push(kvHtml(T.pac.ruleSource, '<span title="' + esc(src) + '">' + esc(src) + '</span>'));
        if (p.lastError) h.push('<div class="err-line">' + esc(p.lastError) + '</div>');
        h.push('<div class="card-actions"><button type="button" class="btn btn-sm" data-act="pac-copy-url"' +
            (p.url ? '' : ' disabled') + '>' + icon('copy') + '<span>' + esc(T.pac.copyURL) + '</span></button></div>');
        h.push('</section>');

        h.push('<section class="card"><h2 class="card-title">' + esc(T.pac.source) + '</h2><div class="form-grid">');
        h.push(fieldHtml('gfwlistURL', T.pac.gfwlistURL, {
            mono: true, placeholder: T.pac.gfwlistPlaceholder, hint: T.pac.gfwlistHint
        }));
        h.push(fieldHtml('externalPACURL', T.pac.externalURL, {
            mono: true, placeholder: T.pac.externalURLPlaceholder, hint: T.pac.externalURLHint
        }));
        h.push('</div>');
        h.push('<div class="card-actions">');
        h.push('<button type="button" class="btn btn-sm" data-act="pac-default-url">' + esc(T.pac.restoreDefault) + '</button>');
        h.push('<button type="button" class="btn btn-sm" data-act="pac-mirror-url">' + esc(T.pac.useMirror) + '</button>');
        h.push('<span class="grow"></span>');
        h.push('<button type="button" class="btn btn-sm btn-accent" data-act="settings-apply"' +
            busyAttrs('settings-apply') + '>' +
            busyLabel('settings-apply', icon('zap') + '<span>' + esc(T.common.saveApply) + '</span>') + '</button>');
        h.push('</div>');
        h.push('<p class="hint">' + esc(T.pac.userRuleHint) + '</p>');
        h.push('</section>');

        body.innerHTML = h.join('');
        refreshSettingsFeedback();
    }

    /* ---------------------------- 8.5 日志 ---------------------------- */
    function visibleLogs() {
        return App.logs.filter((e) => App.logLevel === 'all' || e.level === App.logLevel);
    }

    function logRowHtml(e) {
        const lv = ['debug', 'info', 'warn', 'error'].indexOf(e.level) >= 0 ? e.level : 'info';
        return '<div class="log-row lv-' + lv + '">' +
            '<span class="log-time">' + esc(fmtClock(e.time)) + '</span>' +
            '<span class="log-level">' + esc(str(e.level).toUpperCase()) + '</span>' +
            '<span class="log-scope">' + esc(e.scope) + '</span>' +
            '<span class="log-msg">' + esc(scrub(e.message)) + '</span></div>';
    }

    function logFilterHtml() {
        const levels = ['all', 'debug', 'info', 'warn', 'error'];
        return '<label class="inline-field"><span>' + esc(T.logs.filter) + '</span>' +
            '<select class="input" data-ui="logLevel">' + levels.map((l) =>
                '<option value="' + l + '"' + (App.logLevel === l ? ' selected' : '') + '>' +
                esc(l === 'all' ? T.logs.all : (T.logLevel[l] || l)) + '</option>').join('') + '</select></label>';
    }

    function renderLogs() {
        const body = document.getElementById('body-logs');
        if (!body) return;
        const rows = visibleLogs();
        const h = [];
        h.push('<div class="toolbar">');
        h.push('<button type="button" class="btn" data-act="logs-clear">' + icon('trash') + '<span>' +
            esc(T.logs.clear) + '</span></button>');
        h.push('<button type="button" class="btn" data-act="open-folder">' + icon('folder') + '<span>' +
            esc(T.logs.openFolder) + '</span></button>');
        h.push(logFilterHtml());
        h.push('<span class="grow"></span>');
        h.push('<span class="muted">' + esc(fill(T.logs.count, App.logs.length)) + '</span>');
        h.push('</div>');
        h.push('<div class="card log-card"><div class="log-list mono" id="log-list">' +
            (rows.length ? rows.map(logRowHtml).join('') : '<p class="hint log-empty">' +
                esc(T.logs.empty) + '</p>') + '</div></div>');
        body.innerHTML = h.join('');

        const list = document.getElementById('log-list');
        if (list) {
            list.addEventListener('scroll', () => {
                App.follow = (list.scrollHeight - list.scrollTop - list.clientHeight) < 24;
            });
            list.scrollTop = list.scrollHeight;
            App.follow = true;
        }
    }

    function pushLog(e) {
        if (!e || (!e.message && !e.scope)) return;
        if (e.seq && App.logs.some((x) => x.seq === e.seq)) return;
        App.logs.push(e);
        if (App.logs.length > LOG_LIMIT) App.logs.splice(0, App.logs.length - LOG_LIMIT);

        if (App.view === 'logs' && !viewHasFocus('logs')) appendLogRow(e);
        if (App.view === 'status' && e.scope === 'proxy') {
            const box = document.getElementById('recent-proxy');
            if (box) box.innerHTML = recentProxyHtml();
        }
    }

    function appendLogRow(e) {
        const list = document.getElementById('log-list');
        if (!list) return;
        if (App.logLevel !== 'all' && e.level !== App.logLevel) return;
        const empty = list.querySelector('.hint');
        if (empty) list.innerHTML = '';
        const wrap = document.createElement('div');
        wrap.innerHTML = logRowHtml(e);
        const row = wrap.firstChild;
        if (row) list.appendChild(row);
        while (list.children.length > LOG_LIMIT) list.removeChild(list.firstChild);
        if (App.follow) list.scrollTop = list.scrollHeight;
    }

    /* ---------------------------- 8.6 诊断 ---------------------------- */
    function statusIcon(st) {
        if (st === 'ok') return {cls: 'status-ok', ico: 'check'};
        if (st === 'warn') return {cls: 'status-warn', ico: 'warn'};
        if (st === 'fail') return {cls: 'status-fail', ico: 'fail'};
        return {cls: 'status-info', ico: 'info'};
    }

    function renderDiag() {
        const body = document.getElementById('body-diag');
        if (!body) return;
        const h = [];
        h.push('<div class="toolbar">');
        h.push('<button type="button" class="btn btn-primary" data-act="diag-run"' + busyAttrs('diag-run') + '>' +
            busyLabel('diag-run', icon('test') + '<span>' + esc(T.diag.run) + '</span>') + '</button>');
        h.push('<button type="button" class="btn btn-accent" data-act="selftest-run"' + busyAttrs('selftest-run') + '>' +
            busyLabel('selftest-run', icon('shield') + '<span>' + esc(T.diag.selfTest) + '</span>') + '</button>');
        h.push('</div>');

        h.push('<section class="card"><h2 class="card-title">' + esc(T.diag.title) + '</h2>');
        if (!App.checks || !App.checks.length) {
            h.push('<p class="hint">' + esc(T.diag.empty) + '</p>');
        } else {
            h.push('<div class="table-wrap"><table class="table"><thead><tr>' +
                '<th>' + esc(T.diag.colName) + '</th><th>' + esc(T.diag.colStatus) + '</th>' +
                '<th>' + esc(T.diag.colDetail) + '</th><th>' + esc(T.diag.colSuggestion) + '</th>' +
                '</tr></thead><tbody>' + App.checks.map((c) => {
                    const s = statusIcon(c.status);
                    return '<tr><td>' + esc(c.name) + '</td>' +
                        '<td><span class="status-cell ' + s.cls + '">' + icon(s.ico) + esc(T.common[c.status] || c.status) + '</span></td>' +
                        '<td>' + esc(c.detail) + '</td>' +
                        '<td class="suggestion">' + esc(c.suggestion) + '</td></tr>';
                }).join('') + '</tbody></table></div>');
        }
        h.push('</section>');

        h.push('<section class="card"><h2 class="card-title">' + esc(T.diag.steps) + '</h2>');
        const r = App.selfTest;
        if (!r) {
            h.push('<p class="hint">' + esc(T.diag.selfTestEmpty) + '</p>');
        } else {
            const st = statusIcon(r.ok ? 'ok' : 'fail');
            h.push('<div class="server-line"><span class="status-cell ' + st.cls + '">' +
                icon(st.ico) + esc(r.ok ? T.diag.selfTestOK : T.diag.selfTestFail) + '</span>' +
                '<span class="chip">' + esc(fill(T.diag.elapsed, r.elapsedMs)) + '</span></div>');
            h.push('<div class="steps">' + arr(r.steps).map((s) => {
                const si = statusIcon(s.ok ? 'ok' : 'fail');
                return '<div class="step ' + si.cls + '">' + icon(si.ico) + '<div><div class="step-name">' +
                    esc(s.name) + '</div><div class="step-detail">' + esc(s.detail) + '</div></div></div>';
            }).join('') + '</div>');
        }
        h.push('</section>');

        body.innerHTML = h.join('');
    }

    /* =====================================================================
     * 9. 交互
     * ===================================================================== */
    async function runAction(key, text, fn) {
        if (App.pending[key]) return undefined;
        App.pending[key] = {text: text || T.common.loading};
        renderView(true);
        try {
            return await fn();
        } catch (err) {
            toast('error', errText(err));
            return undefined;
        } finally {
            delete App.pending[key];
            renderView(true);
        }
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
            ta.select();
            const ok = document.execCommand && document.execCommand('copy');
            document.body.removeChild(ta);
            return !!ok;
        } catch (err) {
            return false;
        }
    }

    function validateServer(v) {
        const host = str(v.host).trim();
        if (!host || /[\s\/]/.test(host)) return T.servers.errHost;
        const port = num(v.port, NaN);
        if (!Number.isFinite(port) || Math.floor(port) !== port || port < 1 || port > 65535) return T.servers.errPort;
        if (METHODS.indexOf(str(v.method)) < 0) return T.servers.errMethod;
        if (!str(v.password)) return T.servers.errPassword;
        return '';
    }

    function validateSettings(d) {
        const errs = [];
        const portOk = (p) => {
            const n = Number(p);
            return Number.isFinite(n) && Math.floor(n) === n && n >= 1024 && n <= 65535;
        };
        if (!isLoopback(d.socksAddress)) errs.push({key: 'socksAddress', message: fill(T.modes.errLoopback, T.modes.socksAddress)});
        if (!portOk(d.socksPort)) errs.push({key: 'socksPort', message: fill(T.modes.errPort, T.modes.socksPort)});
        if (d.httpEnabled) {
            if (!isLoopback(d.httpAddress)) errs.push({key: 'httpAddress', message: fill(T.modes.errLoopback, T.modes.httpAddress)});
            if (!portOk(d.httpPort)) errs.push({key: 'httpPort', message: fill(T.modes.errPort, T.modes.httpPort)});
        }
        if (!isLoopback(d.pacAddress)) errs.push({key: 'pacAddress', message: fill(T.modes.errLoopback, T.modes.pacAddress)});
        if (!portOk(d.pacPort)) errs.push({key: 'pacPort', message: fill(T.modes.errPort, T.modes.pacPort)});
        if (d.httpEnabled && num(d.socksPort, 0) === num(d.httpPort, 0)) {
            errs.push({key: 'httpPort', message: fill(T.modes.errDup, 'SOCKS / HTTP')});
        }
        if (num(d.pacPort, 0) === num(d.socksPort, 0) ||
            (d.httpEnabled && num(d.pacPort, 0) === num(d.httpPort, 0))) {
            errs.push({key: 'pacPort', message: fill(T.modes.errDup, 'PAC')});
        }
        if (state().mode === 'externalPAC' && !str(d.externalPACURL).trim()) {
            errs.push({key: 'externalPACURL', message: T.modes.errExternalPAC});
        }
        return errs;
    }

    function settingsPayload(d) {
        const p = Object.assign({}, d);
        ['socksPort', 'httpPort', 'pacPort'].forEach((k) => { p[k] = num(p[k], 0); });
        p.networkServices = arr(p.networkServices).map(str);
        return p;
    }

    function refreshSettingsFeedback() {
        const d = ensureDraft();
        const errs = validateSettings(d);
        const box = document.getElementById('settings-errors');
        if (box) box.innerHTML = errs.map((e) => '<div class="form-error">' + esc(e.message) + '</div>').join('');
        const bad = {};
        errs.forEach((e) => { bad[e.key] = true; });
        $$('#body-modes [data-key], #body-pac [data-key]').forEach((inp) => {
            inp.classList.toggle('is-invalid', !!bad[inp.dataset.key]);
        });
        const dirtyEl = document.getElementById('settings-dirty');
        if (dirtyEl) dirtyEl.textContent = App.dirty ? T.common.dirtyNotice : '';
    }

    function updateDraftField(el) {
        const key = str(el.dataset.key);
        if (!key) return;
        const d = ensureDraft();
        let val;
        if (el.type === 'checkbox') val = !!el.checked;
        else if (el.dataset.type === 'number') {
            val = el.value === '' ? 0 : Number(el.value);
            if (!Number.isFinite(val)) val = 0;
        } else val = el.value;
        d[key] = val;

        if (key === 'launchAtLogin' || key === 'hideDockIcon') {
            detach(applyImmediateToggle(key, !!val));
            return;
        }
        App.dirty = true;
        refreshSettingsFeedback();
    }

    function updateEditorField(el) {
        const ed = App.editor;
        if (!ed) return;
        const key = str(el.dataset.ed);
        if (!key) return;
        let val;
        if (el.dataset.type === 'number') {
            val = el.value === '' ? 0 : Number(el.value);
            if (!Number.isFinite(val)) val = 0;
        } else val = el.value;
        ed.view[key] = val;
        refreshEditorFeedback();
    }

    function updateServiceToggle(el) {
        const d = ensureDraft();
        const name = str(el.dataset.svc);
        if (!name) return;
        const list = arr(d.networkServices).slice();
        const i = list.indexOf(name);
        if (el.checked && i < 0) list.push(name);
        if (!el.checked && i >= 0) list.splice(i, 1);
        $$('#body-modes input[data-svc]').forEach((cb) => {
            const n = cb.dataset.svc;
            if (cb.checked && list.indexOf(n) < 0) list.push(n);
        });
        d.networkServices = list;
        App.dirty = true;
        refreshSettingsFeedback();
    }

    async function applyImmediateToggle(key, on) {
        const busyKey = 'toggle:' + key;
        if (App.pending[busyKey]) return;
        App.pending[busyKey] = {text: T.common.loading};
        const d = ensureDraft();
        const r = key === 'launchAtLogin'
            ? await call('SetLaunchAtLogin', on)
            : await call('SetHideDockIcon', on, true);
        const good = r.ok && r.value !== false;
        if (!good) {
            d[key] = !on;
            toast('error', key === 'launchAtLogin' ? T.modes.launchAtLoginFailed : T.modes.hideDockFailed);
        }
        delete App.pending[busyKey];
        await refreshState();
        renderView(true);
    }

    async function loadServices(force) {
        if (App.servicesLoading) return;
        if (App.servicesLoaded && !force) return;
        App.servicesLoading = true;
        renderView(true);
        const r = await call('ListNetworkServices');
        App.servicesLoading = false;
        if (r.ok) {
            App.services = arr(r.value).map(networkServiceView);
            App.servicesLoaded = true;
        }
        if (App.view === 'modes') renderView(true);
    }

    async function loadLogs() {
        const r = await call('GetLogs', 0);
        if (!r.ok) return;
        if (App.logs.length) return; // 期间已经有实时日志了，不要覆盖
        App.logs = arr(r.value).map(logEntry).slice(-LOG_LIMIT);
        if (App.view === 'logs') renderView(true);
    }

    function saveSettings(apply) {
        const d = ensureDraft();
        const errs = validateSettings(d);
        if (errs.length) {
            refreshSettingsFeedback();
            toast('warn', errs[0].message);
            return;
        }
        const key = apply ? 'settings-apply' : 'settings-save';
        runAction(key, apply ? T.common.applying : T.common.saving, async () => {
            const {ok} = await call(apply ? 'ApplySettings' : 'SaveSettings', settingsPayload(d));
            if (!ok) return;
            App.dirty = false;
            toast('success', apply ? T.modes.applied : T.modes.saved);
            await refreshState();
            renderView(true);
        });
    }

    const ACTIONS = {
        nav: (el) => setView(el.dataset.view),

        'retry-backend': () => boot(),

        'toggle-power': (el, id, key) => runAction(key, T.common.loading, async () => {
            const r = await call('SetEnabled', !state().enabled);
            if (r.ok) await refreshState(); // 状态事件之外的兜底刷新
        }),

        'set-mode': (el) => {
            if (el.classList.contains('is-disabled')) { toast('warn', T.modesDisabledHint); return; }
            const m = str(el.dataset.mode);
            if (!m || m === state().mode) return;
            detach(call('SetMode', m).then((r) => {
                if (!r.ok) return undefined;
                // 点单选框后焦点仍在该 INPUT 上，applyState 会跳过重绘，
                // 这里补一次强制重绘，保证整行高亮跟着切换。
                return refreshState().then(() => renderView(true));
            }));
        },

        'goto-servers': () => setView('servers'),

        'row-more': (el, id) => {
            App.menu = (App.menu && App.menu.id === id) ? null : {id: id};
            renderView(true);
        },

        'goto-modes': () => setView('modes'),

        'install-helper': (el, id, key) => runAction(key, T.status.installing, async () => {
            await call('InstallHelper');
            await refreshState();
        }),

        'helper-refresh': (el, id, key) => runAction(key, T.common.loading, async () => {
            const r = await call('HelperStatus');
            if (r.ok && r.value) { App.state.helper = helperView(r.value); renderView(true); }
        }),

        'add-server': () => {
            App.editor = {mode: 'new', view: emptyServer()};
            renderView(true);
            const f = $('[data-ed="remark"]');
            if (f) f.focus();
        },

        'edit-server': (el, id) => {
            const v = findServer(id);
            if (!v) return;
            App.editor = {mode: 'edit', view: Object.assign({}, v)};
            renderView(true);
            const f = $('[data-ed="remark"]');
            if (f) f.focus();
        },

        'editor-cancel': () => { App.editor = null; renderView(true); },

        'toggle-password': (el) => {
            const inp = document.getElementById('ed-password');
            if (!inp) return;
            const show = inp.type === 'password';
            inp.type = show ? 'text' : 'password';
            el.innerHTML = icon(show ? 'eyeOff' : 'eye');
            el.title = show ? T.servers.hidePassword : T.servers.showPassword;
        },

        'server-save': (el, id, key) => {
            const ed = App.editor;
            if (!ed) return;
            const err = validateServer(ed.view);
            if (err) { refreshEditorFeedback(); toast('warn', err); return; }
            runAction(key, T.common.saving, async () => {
                const payload = Object.assign({}, ed.view, {port: num(ed.view.port, 0)});
                const r = ed.mode === 'new'
                    ? await call('AddServer', payload)
                    : await call('UpdateServer', payload);
                if (!r.ok) return;
                App.editor = null;
                toast('success', T.servers.saved);
                await refreshState();
                renderView(true);
            });
        },

        'delete-server': (el, id) => {
            const v = findServer(id);
            if (!v) return;
            const question = fill(T.common.confirmDelete, v.title || (v.host + ':' + v.port));
            // 危险操作必须确认；宿主没有 confirm 时宁可不执行。
            if (typeof window.confirm !== 'function') { toast('warn', question); return; }
            if (!window.confirm(question)) return;
            if (App.editor && App.editor.view.id === id) App.editor = null;
            detach(call('DeleteServer', id).then(() => refreshState()).then(() => renderView(true)));
        },

        'select-server': (el, id) => {
            detach(call('SelectServer', id).then((r) => {
                if (r.ok) toast('info', T.servers.selected);
                return refreshState();
            }).then(() => renderView(true)));
        },

        'move-server': (el, id) => {
            const delta = num(el.dataset.delta, 0);
            if (!delta) return;
            detach(call('MoveServer', id, delta).then(() => refreshState()).then(() => renderView(true)));
        },

        'test-server': (el, id, key) => runAction(key, T.servers.testing, async () => {
            await call('TestServer', id);
        }),

        'export-server': (el, id, key) => runAction(key, T.common.loading, async () => {
            const r = await call('ExportSSURL', id);
            if (!r.ok || !r.value) return;
            const ok = await copyText(r.value);
            toast(ok ? 'success' : 'warn', ok ? T.common.copied : T.common.copyFailed);
        }),

        'copy-current-url': (el, id, key) => runAction(key, T.common.loading, async () => {
            const sv = state().server;
            if (!sv) return;
            const r = await call('ExportSSURL', sv.id);
            if (!r.ok || !r.value) return;
            const ok = await copyText(r.value);
            toast(ok ? 'success' : 'warn', ok ? T.common.copied : T.common.copyFailed);
        }),

        'import-ss': (el, id, key) => {
            const raw = str(App.importRaw).trim();
            if (!raw) { toast('warn', T.servers.importEmpty); return; }
            runAction(key, T.servers.importing, async () => {
                const r = await call('ImportSSURL', raw);
                if (!r.ok || !r.value) return;
                const v = serverView(r.value);
                App.importRaw = '';
                toast('success', fill(T.servers.importOK, v.title || (v.host + ':' + v.port)));
                await refreshState();
                renderView(true);
            });
        },

        'settings-save': () => saveSettings(false),
        'settings-apply': () => saveSettings(true),

        'settings-reload': () => {
            App.dirty = false;
            App.draft = cloneSettings(state().settings);
            renderView(true);
            toast('info', T.common.reloaded);
        },

        'services-refresh': (el, id, key) => runAction(key, T.common.loading, async () => {
            App.servicesLoaded = false;
            await loadServices(true);
        }),

        'pac-update': (el, id, key) => runAction(key, T.pac.updating, async () => {
            const r = await call('UpdatePACFromGFWList');
            if (r.ok && r.value) {
                App.state.pac = pacView(r.value);
                toast('success', fill(T.pac.updated, num(obj(r.value).ruleCount, 0)));
            }
            await refreshState();
            renderView(true);
        }),

        'pac-refresh': (el, id, key) => runAction(key, T.common.loading, async () => {
            const r = await call('PACStatus');
            if (r.ok && r.value) { App.state.pac = pacView(r.value); renderView(true); }
        }),

        'pac-userrule': () => { detach(call('OpenUserRuleFile')); },
        'open-folder': () => { detach(call('OpenConfigFolder')); },

        // 命令行代理：按 data-id 重新生成，不读 DOM 里的文字，避免剪贴板被界面文本污染。
        'copy-cli-proxy': (el, id, key) => {
            const row = cliProxyCommands().filter((r) => r.id === str(id))[0];
            if (!row) return;
            copyText(row.command).then((ok) =>
                toast(ok ? 'success' : 'warn', ok ? T.common.copied : T.common.copyFailed));
        },

        'pac-copy-url': () => {
            const url = state().pac.url;
            if (!url) return;
            copyText(url).then((ok) => toast(ok ? 'success' : 'warn', ok ? T.common.copied : T.common.copyFailed));
        },

        'pac-default-url': () => {
            ensureDraft().gfwlistURL = DEFAULT_GFWLIST_URL;
            App.dirty = true;
            renderView(true);
            toast('info', T.pac.restoreDefaultDone);
        },

        'pac-mirror-url': () => {
            ensureDraft().gfwlistURL = MIRROR_GFWLIST_URL;
            App.dirty = true;
            renderView(true);
            toast('info', T.pac.useMirrorDone);
        },

        'logs-clear': () => {
            App.logs = [];
            detach(call('ClearLogs'));
            renderView(true);
            toast('info', T.logs.cleared);
        },

        'diag-run': (el, id, key) => runAction(key, T.diag.running, async () => {
            const r = await call('Diagnose');
            if (!r.ok) return;
            App.checks = arr(r.value).map(checkView);
            renderView(true);
            toast('info', fill(T.diag.diagDone, App.checks.length));
        }),

        'selftest-run': (el, id, key) => runAction(key, T.diag.selfTesting, async () => {
            const r = await call('SelfTest');
            if (!r.ok || !r.value) return;
            App.selfTest = selfTestView(r.value);
            renderView(true);
            toast(App.selfTest.ok ? 'success' : 'warn',
                App.selfTest.ok ? T.diag.selfTestOK : T.diag.selfTestFail);
        })
    };

    // 只重绘受影响的视图，避免关菜单时把整个页面重建（会丢焦点/滚动位置）
    function closeMenu() {
        if (!App.menu) return;
        App.menuId = App.menu.id;
        App.menu = null;
        renderView(true);
    }

    function onClick(e) {
        const target = e.target;
        if (!target || target.nodeType !== 1 || typeof target.closest !== 'function') return;

        // 行内"更多"菜单：点到菜单或触发按钮以外的地方就收起。
        // 菜单项本身也是 data-act，所以这里只负责关，动作交给下面的委托。
        if (App.menu) {
            const inside = target.closest('.menu') || target.closest('[data-act="row-more"]');
            if (!inside) closeMenu();
        }

        const el = target.closest('[data-act]');
        if (!el || el.disabled) return;
        const act = str(el.dataset.act);
        const handler = ACTIONS[act];
        if (!handler) return;
        const key = pkey(act, el.dataset.id);
        if (App.pending[key]) { e.preventDefault(); return; }
        e.preventDefault();
        Promise.resolve()
            .then(() => handler(el, el.dataset.id, key))
            .catch((err) => toast('error', errText(err)));
    }

    function onInput(e) {
        const el = e.target;
        if (!el || el.nodeType !== 1 || !el.dataset) return;
        const ds = el.dataset;
        if (ds.ui === 'importRaw') { App.importRaw = el.value; return; }
        if (ds.ui === 'logLevel') { App.logLevel = str(el.value) || 'all'; renderView(true); return; }
        if (ds.ed) { updateEditorField(el); return; }
        if (ds.svc) { updateServiceToggle(el); return; }
        if (ds.key) updateDraftField(el);
    }

    function onKeydown(e) {
        if (e.key === 'Escape' && App.menu) {
            e.preventDefault();
            closeMenu();
            const btn = $('[data-act="row-more"][data-id="' + App.menuId + '"]');
            if (btn) btn.focus();
            return;
        }
        if (e.key !== 'Enter') return;
        const el = e.target;
        if (el && el.id === 'import-raw') {
            e.preventDefault();
            const btn = $('[data-act="import-ss"]');
            if (btn && !btn.disabled) btn.click();
        }
    }

    /* =====================================================================
     * 10. 启动
     * ===================================================================== */
    // 让原生窗口底色跟当前主题一致，避免浅色模式下窗口先闪一下深色背景。
    function syncWindowColour() {
        try {
            const rt = window.runtime;
            if (!rt || typeof rt.WindowSetBackgroundColour !== 'function') return;
            const css = getComputedStyle(document.documentElement).getPropertyValue('--bg').trim();
            const m = /^#([0-9a-fA-F]{6})$/.exec(css);
            if (!m) return;
            const v = parseInt(m[1], 16);
            rt.WindowSetBackgroundColour((v >> 16) & 255, (v >> 8) & 255, v & 255, 255);
        } catch (err) { /* 只是窗口底色，失败无所谓 */ }
    }

    function watchAppearance() {
        syncWindowColour();
        try {
            const mq = window.matchMedia('(prefers-color-scheme: dark)');
            if (mq && typeof mq.addEventListener === 'function') mq.addEventListener('change', syncWindowColour);
        } catch (err) { /* 忽略 */ }
    }

    function boot() {
        applyI18n(document);
        watchAppearance();
        renderSidebar();
        renderBackendBanner();
        if (!hasBackend()) {
            markBackendDown('');
            renderView(true);
            return;
        }
        bindEvents();
        detach(refreshState());
        detach(loadLogs());
    }

    let booted = false;

    function init() {
        if (booted) return;
        booted = true;
        document.addEventListener('click', onClick);
        document.addEventListener('input', onInput);
        document.addEventListener('change', onInput);
        document.addEventListener('keydown', onKeydown);
        boot();
    }

    if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', init);
    else init();
})();
