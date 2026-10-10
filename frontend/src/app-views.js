/*!
 * SSXNG — 视图渲染（依赖 app-core.js）
 *
 * 每个视图一个 render* 函数，把状态拼成 HTML 字符串写进对应的 <section>。
 * 事件订阅（ssxng:state / :log / :traffic / :toast）也在这里：它们唯一的作用
 * 就是触发重绘。renderView 是唯一的绘制入口。
 */
(function () {
    'use strict';

    // 核心脚本没跑起来（例如文案表缺失）时，这里什么都不做。
    const S = window.SSXNG;
    if (!S) return;

    // 从先加载的文件里取需要的名字。
    const {$$, App, LOG_LIMIT, METHODS, MODES, T, VIEWS, arr, busyAttrs, busyLabel,
        cloneSettings, detach, ensureDraft, esc, fill, fmtBytes, fmtClock, fmtDateTime,
        fmtDuration, fmtSpeed, icon, logEntry, markBackendUp, normalizeState, num, obj,
        onEvent, pkey, renderBackendBanner, scrub, state, str, toast, trafficView,
        viewHasFocus} = S;

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
        if (name === 'modes') detach(S.loadServices(false));
        if (name === 'logs') {
            App.follow = true;
            const list = document.getElementById('log-list');
            if (list) list.scrollTop = list.scrollHeight;
        }
    }

    function renderView(force) {
        const name = VIEWS.indexOf(App.view) >= 0 ? App.view : 'status';
        // 分区显示状态与"是否重绘内容"是两件事：把前者提到提前 return 之前，
        // 否则 force === false 时连该显示哪个视图都不会更新。
        VIEWS.forEach((v) => {
            const sec = document.getElementById('view-' + v);
            if (sec) sec.hidden = (v !== name);
        });
        if (force === false) return; // 表单正在输入：保留 DOM，等下一次状态推送
        switch (name) {
            case 'servers': renderServers(); break;
            case 'modes': renderModes(); break;
            case 'pac': renderPAC(); break;
            case 'logs': renderLogs(); break;
            case 'diag': renderDiag(); break;
            default: renderStatus(); break;
        }
    }

/* ----------------------------  状态 ---------------------------- */
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

    // UDP 与 SOCKS 共用一个端口，所以不占独立的一项（状态页一行有 4 项，
    // 加一项会挤到第二行）。有它时在 SOCKS 那项后面点一下即可。
    function udpSuffix(s) {
        return (s.settings.udpEnabled && s.udp.active) ? ' · ' + T.status.udp : '';
    }

    // 关键事实一行排列：省掉"监听端口"卡片里的逐行堆叠
    function factsHtml(s) {
        const facts = [
            {label: T.status.socks,
                value: str(s.socks.address || '—') + ':' + num(s.socks.port, 0) + udpSuffix(s),
                title: udpSuffix(s) ? T.status.udp + ' ' + T.common.active : '',
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

/* ----------------------------  服务器 ---------------------------- */
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
        return '<div class="menu" role="menu">' +
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
        h.push('<section class="card editor">');
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
        const err = S.validateServer(ed.view);
        if (err) msgs.push(err);
        else if (num(ed.view.port, 0) > 0 && num(ed.view.port, 0) < 1024) msgs.push(T.servers.errPortLow);
        box.innerHTML = msgs.map((m) => '<div class="form-error">' + esc(m) + '</div>').join('');
    }

/* ----------------------------  模式与代理 ---------------------------- */
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

    // hint 会占一行高度；页面密度有硬门槛的地方（如本地监听网格）用 title 传说明，
    // 鼠标悬停同样能看到，但不增加页高。
    function checkFieldHtml(key, label, hint, title) {
        const d = ensureDraft();
        return '<div><label class="check"' + (title ? ' title="' + esc(title) + '"' : '') +
            '><input type="checkbox" data-key="' + esc(key) + '" data-type="bool"' +
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

/* ----------------------------  命令行代理（export 命令） ---------------------------- */

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
            h.push('<code class="cli-cmd">' + esc(r.command) + '</code>');
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
        // UDP 与"跟随全局"一样是附加开关，两者叠在同一格里：网格是
        // auto-fit(210px)，多加一格就会多出一整行（页高 +38px）。
        h.push('<div class="check-stack">' +
            checkFieldHtml('httpFollowGlobal', T.modes.httpFollowGlobal) +
            checkFieldHtml('udpEnabled', T.modes.udpEnabled, '', T.modes.udpEnabledHint) +
            '</div>');
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
        S.refreshSettingsFeedback();
    }

/* ----------------------------  PAC ---------------------------- */
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
        S.refreshSettingsFeedback();
    }

/* ----------------------------  日志 ---------------------------- */
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
            // A re-render replaces the DOM, which resets scrollTop to 0. Put the
            // user back where they were: a state push must not yank someone who
            // is reading history down to the newest line.
            const follow = App.follow;
            const prevTop = App.logScroll;
            list.addEventListener('scroll', () => {
                App.logScroll = list.scrollTop;
                App.follow = (list.scrollHeight - list.scrollTop - list.clientHeight) < 24;
            });
            list.scrollTop = follow ? list.scrollHeight : prevTop;
            App.logScroll = list.scrollTop;
        }
    }

    function pushLog(e) {
        if (!e || (!e.message && !e.scope)) return;
        // Dedup on the sequence number. The backend numbers every line, so a
        // watermark answers "have I seen this?" in O(1); the old linear scan was
        // O(n) per line, i.e. O(n²) over a session's worth of logs.
        if (e.seq > 0) {
            if (e.seq <= App.logSeq) return;
            App.logSeq = e.seq;
        }
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

/* ----------------------------  诊断 ---------------------------- */
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

    /* 对后面加载的脚本公开的接口 */
    Object.assign(S, {
        applyState, bindEvents, cliProxyCommands, refreshEditorFeedback, renderSidebar,
        renderView, setView
    });
})();
