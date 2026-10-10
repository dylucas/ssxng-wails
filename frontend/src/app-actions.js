/*!
 * SSXNG — 动作处理（依赖 app-core.js、app-views.js）
 *
 *   · 表单绑定：草稿 → 输入框、输入框 → 草稿；
 *   · 设置校验与保存/应用，服务器增删改与导入导出；
 *   · ACTIONS 表：data-act 属性 → 处理函数，由 app.js 的点击委托查找。
 */
(function () {
    'use strict';

    // 核心脚本没跑起来（例如文案表缺失）时，这里什么都不做。
    const S = window.SSXNG;
    if (!S) return;

    // 从先加载的文件里取需要的名字。
    const {$, $$, App, DEFAULT_GFWLIST_URL, LOG_LIMIT, METHODS, MIRROR_GFWLIST_URL, T, arr,
        call, checkView, cliProxyCommands, cloneSettings, copyText, detach, emptyServer,
        ensureDraft, errText, esc, fill, findServer, helperView, icon, isLoopback,
        logEntry, networkServiceView, num, obj, pacView, refreshEditorFeedback,
        refreshState, renderView, selfTestView, serverView, setView, state, str, toast} = S;

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

    // runAction 的轻量版：这些动作对应的按钮没有忙碌文案（不显示 spinner），
    // 但也必须在途只发一次 —— 连点"删除/上移/选中/打开配置目录"原本会重复
    // 发 RPC（删除还会重复弹一次确认框）。onClick 在点击时已经查过
    // App.pending[key]，所以这里同步占位就能挡住紧随其后的第二次点击。
    async function guardAction(key, fn) {
        if (App.pending[key]) return undefined;
        App.pending[key] = {text: ''};
        try {
            return await fn();
        } catch (err) {
            toast('error', errText(err));
            return undefined;
        } finally {
            delete App.pending[key];
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
        // 编辑中的密码也算"已知密码"，抹除缓存要跟着失效。
        if (key === 'password') S.invalidateSecretCache();
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

    // 拉取日志并与本地镜像合并。
    //
    // afterSeq 走水位线：启动时 App.logSeq 为 0，后端返回最近 500 条；切到日志页
    // 或手动刷新时带上当前水位线，只取真正缺的那些。日志事件是成批推送的，批次
    // 在缓冲满时可能被丢弃，这一步是界面补齐的唯一手段，所以不能只跑一次。
    async function refreshLogs() {
        const r = await call('GetLogs', num(App.logSeq, 0));
        if (!r.ok) return 0;
        const fetched = arr(r.value).map(logEntry).slice(-LOG_LIMIT);
        if (!fetched.length) return 0;
        // A live batch can land before this response does. Merging by seq keeps
        // both instead of dropping the whole backlog.
        const seen = new Set(App.logs.map((e) => e.seq));
        const missing = fetched.filter((e) => !seen.has(e.seq));
        if (!missing.length) return 0;
        App.logs = missing.concat(App.logs).sort((a, b) => a.seq - b.seq).slice(-LOG_LIMIT);
        const maxSeq = App.logs.reduce((m, e) => (e.seq > m ? e.seq : m), 0);
        if (maxSeq > App.logSeq) App.logSeq = maxSeq;
        if (App.view === 'logs') renderView(true);
        return missing.length;
    }

    // 启动时拉一次完整镜像（afterSeq=0 时后端返回最近 500 条）。
    function loadLogs() { return refreshLogs(); }

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

        'retry-backend': () => S.boot(),

        'toggle-power': (el, id, key) => runAction(key, T.common.loading, async () => {
            const r = await call('SetEnabled', !state().enabled);
            if (r.ok) await refreshState(); // 状态事件之外的兜底刷新
        }),

        'set-mode': (el) => guardAction('set-mode', async () => {
            if (el.classList.contains('is-disabled')) { toast('warn', T.modesDisabledHint); return; }
            const m = str(el.dataset.mode);
            if (!m || m === state().mode) return;
            const r = await call('SetMode', m);
            if (!r.ok) return;
            // 点单选框后焦点仍在该 INPUT 上，applyState 会跳过重绘，
            // 这里补一次强制重绘，保证整行高亮跟着切换。
            await refreshState();
            renderView(true);
        }),

        'goto-servers': () => setView('servers'),

        'row-more': (el, id) => {
            const open = App.menu && App.menu.id === id;
            if (open) {
                // 收起：焦点回到刚才的触发按钮，键盘用户不会"掉"在页面某处。
                App.menu = null;
                App.menuId = id;
                renderView(true);
                const btn = S.$('[data-act="row-more"][data-id="' + id + '"]');
                if (btn) btn.focus();
                return;
            }
            App.menu = {id: id};
            App.menuId = id;
            renderView(true);
            // 打开后把焦点放进第一个可用菜单项：否则方向键无从谈起。
            const first = S.$('.menu [data-act]:not([disabled])');
            if (first) first.focus();
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

        'delete-server': (el, id, key) => guardAction(key, async () => {
            const v = findServer(id);
            if (!v) return;
            const question = fill(T.common.confirmDelete, v.title || (v.host + ':' + v.port));
            // 危险操作必须确认；宿主没有 confirm 时宁可不执行。
            if (typeof window.confirm !== 'function') { toast('warn', question); return; }
            if (!window.confirm(question)) return;
            if (App.editor && App.editor.view.id === id) App.editor = null;
            await call('DeleteServer', id);
            await refreshState();
            renderView(true);
        }),

        'select-server': (el, id, key) => guardAction(key, async () => {
            const r = await call('SelectServer', id);
            if (r.ok) toast('info', T.servers.selected);
            await refreshState();
            renderView(true);
        }),

        'move-server': (el, id, key) => guardAction(key, async () => {
            const delta = num(el.dataset.delta, 0);
            if (!delta) return;
            await call('MoveServer', id, delta);
            await refreshState();
            renderView(true);
        }),

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

        'pac-userrule': (el, id, key) => guardAction(key, () => call('OpenUserRuleFile')),
        'open-folder': (el, id, key) => guardAction(key, () => call('OpenConfigFolder')),

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

        'logs-clear': (el, id, key) => guardAction(key, () => {
            App.logs = [];
            App.logScroll = 0;
            detach(call('ClearLogs'));
            renderView(true);
            toast('info', T.logs.cleared);
        }),

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

    /* 对后面加载的脚本公开的接口 */
    Object.assign(S, {
        ACTIONS, loadLogs, loadServices, refreshLogs, refreshSettingsFeedback, updateDraftField,
        updateEditorField, updateServiceToggle, validateServer
    });
})();
