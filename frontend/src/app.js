/*!
 * SSXNG — 启动与事件绑定（最后加载，只有这里产生副作用）
 *
 * 绑定 document 上的点击/输入/键盘委托，再跑 boot()。把"定义"与"启动"分开，
 * 改渲染逻辑就不必碰启动顺序。
 */
(function () {
    'use strict';

    // 核心脚本没跑起来（例如文案表缺失）时，这里什么都不做。
    const S = window.SSXNG;
    if (!S) return;

    // 从先加载的文件里取需要的名字。
    const {$, $$, ACTIONS, App, applyI18n, bindEvents, detach, errText, hasBackend, loadLogs,
        markBackendDown, pkey, refreshState, renderBackendBanner, renderSidebar,
        renderView, str, toast, updateDraftField, updateEditorField, updateServiceToggle} = S;

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

    // 复选框、单选框和下拉框在用户操作时会同时触发 input 与 change；文本框只在
    // input 时处理，否则失焦的那次 change 会把同一份输入再处理一遍。之前两个事件
    // 都绑到 onInput，等于每次勾选/切下拉都跑两遍——日志等级那次会连整页重绘两次。
    function fieldEventKind(el) {
        if (el.tagName === 'SELECT') return 'change';
        if (el.tagName === 'INPUT' && (el.type === 'checkbox' || el.type === 'radio')) return 'change';
        return 'input';
    }

    function onFieldEvent(e, kind) {
        const el = e.target;
        if (!el || el.nodeType !== 1 || !el.dataset) return;
        if (fieldEventKind(el) !== kind) return;
        const ds = el.dataset;
        if (ds.ui === 'importRaw') { App.importRaw = el.value; return; }
        if (ds.ui === 'logLevel') {
            App.logLevel = str(el.value) || 'all';
            renderView(true);
            // 换了过滤条件就顺便补一次增量，避免把"被过滤掉"和"没收到"混为一谈。
            detach(S.refreshLogs());
            return;
        }
        if (ds.ed) { updateEditorField(el); return; }
        if (ds.svc) { updateServiceToggle(el); return; }
        if (ds.key) updateDraftField(el);
    }

    function onInput(e) { onFieldEvent(e, 'input'); }
    function onChange(e) { onFieldEvent(e, 'change'); }

    // 行内"更多"菜单的键盘导航：菜单项可以点，也必须能用键盘走完。
    // 只在菜单打开时接管这几个键，其余情况一律放行。
    function menuItems() {
        return $$('.menu [data-act]:not([disabled])');
    }

    function focusMenuButton() {
        const btn = $('[data-act="row-more"][data-id="' + App.menuId + '"]');
        if (btn) btn.focus();
    }

    // 收起菜单并把焦点还给触发按钮。Tab 也走这里（不 preventDefault），
    // 否则焦点会穿到菜单背后的页面里。
    function closeMenuByKeyboard() {
        App.menu = null;
        renderView(true);
        focusMenuButton();
    }

    function onKeydown(e) {
        if (App.menu) {
            const items = menuItems();
            const idx = items.indexOf(document.activeElement);
            if (e.key === 'Escape') {
                e.preventDefault();
                closeMenuByKeyboard();
                return;
            }
            if (e.key === 'Tab') {
                closeMenuByKeyboard();
                return;
            }
            if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
                e.preventDefault();
                if (!items.length) return;
                // 焦点不在菜单里（例如仍停在触发按钮上）时，向下进入第一项。
                const step = e.key === 'ArrowDown' ? 1 : -1;
                const next = idx < 0
                    ? (step > 0 ? 0 : items.length - 1)
                    : (idx + step + items.length) % items.length;
                items[next].focus();
                return;
            }
            if (e.key === 'Home' || e.key === 'End') {
                e.preventDefault();
                if (!items.length) return;
                items[e.key === 'Home' ? 0 : items.length - 1].focus();
                return;
            }
        }
        if (e.key !== 'Enter') return;
        const el = e.target;
        if (el && el.id === 'import-raw') {
            e.preventDefault();
            const btn = $('[data-act="import-ss"]');
            if (btn && !btn.disabled) btn.click();
        }
    }

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

    // boot() is re-entered by the "重试连接" button on the backend-offline
    // screen, which sits *above* the early return in boot(), so this has to be
    // idempotent: without the flag every retry would add another matchMedia
    // listener that is never removed.
    let appearanceWatched = false;

    function watchAppearance() {
        syncWindowColour();
        if (appearanceWatched) return;
        appearanceWatched = true;
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
        document.addEventListener('change', onChange);
        document.addEventListener('keydown', onKeydown);
        boot();
    }

    if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', init);
    else init();

    /* 对后面加载的脚本公开的接口 */
    Object.assign(S, {
        boot
    });
})();
