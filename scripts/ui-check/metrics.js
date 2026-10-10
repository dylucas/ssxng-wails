// 页面内的度量代码：把"看不见的界面"变成可断言的数字。
// 由 scripts/ui-check.sh 注入到 harness 页面，在 app.js 之后执行（都是 defer）。
// measure.js 通过 Runtime.evaluate 读取 window.__ui.metrics()。
(function () {
    'use strict';

    function css(el, prop) { return window.getComputedStyle(el).getPropertyValue(prop).trim(); }

    function parseColour(v) {
        const m = /^rgba?\(([^)]+)\)$/i.exec(v || '');
        if (!m) return null;
        const p = m[1].split(/[,/\s]+/).filter(Boolean).map(parseFloat);
        if (p.length < 3) return null;
        return {r: p[0], g: p[1], b: p[2], a: p.length > 3 ? p[3] : 1};
    }

    function blend(top, bottom) {
        const a = top.a + bottom.a * (1 - top.a);
        if (a <= 0) return {r: 0, g: 0, b: 0, a: 0};
        return {
            r: (top.r * top.a + bottom.r * bottom.a * (1 - top.a)) / a,
            g: (top.g * top.a + bottom.g * bottom.a * (1 - top.a)) / a,
            b: (top.b * top.a + bottom.b * bottom.a * (1 - top.a)) / a,
            a: a
        };
    }

    // 元素真实背景：从最外层往里合成所有带 alpha 的背景色。
    function effectiveBg(el) {
        const chain = [];
        for (let n = el; n; n = n.parentElement) chain.push(n);
        chain.reverse();
        let acc = {r: 255, g: 255, b: 255, a: 1};
        chain.forEach(function (n) {
            const c = parseColour(css(n, 'background-color'));
            if (c && c.a > 0) acc = blend(c, acc);
        });
        return acc;
    }

    function luminance(c) {
        function f(v) {
            v = v / 255;
            return v <= 0.03928 ? v / 12.92 : Math.pow((v + 0.055) / 1.055, 2.4);
        }
        return 0.2126 * f(c.r) + 0.7152 * f(c.g) + 0.0722 * f(c.b);
    }

    function contrast(fg, bg) {
        const a = luminance(fg), b = luminance(bg);
        return (Math.max(a, b) + 0.05) / (Math.min(a, b) + 0.05);
    }

    function visible(el) {
        if (!el) return false;
        if (el.closest('[hidden]')) return false;
        const cs = window.getComputedStyle(el);
        if (cs.display === 'none' || cs.visibility === 'hidden') return false;
        const r = el.getBoundingClientRect();
        return r.width > 0 && r.height > 0;
    }

    function label(el) {
        let s = el.tagName.toLowerCase();
        if (el.id) s += '#' + el.id;
        if (el.classList.length) s += '.' + Array.prototype.join.call(el.classList, '.');
        const act = el.dataset ? el.dataset.act : '';
        if (act) s += '[data-act=' + act + ']';
        return s;
    }

    // 在指定根节点里取一个元素的文本/属性
    function text(root, sel, pick) {
        const el = (root || document).querySelector(sel);
        if (!el) return null;
        if (pick) return pick(el);
        return (el.innerText || '').trim();
    }

    function rect(sel) {
        const el = document.querySelector(sel);
        if (!el) return null;
        const r = el.getBoundingClientRect();
        return {
            top: Math.round(r.top), left: Math.round(r.left),
            width: Math.round(r.width), height: Math.round(r.height),
            bottom: Math.round(r.bottom), right: Math.round(r.right)
        };
    }

    // 需要检查对比度的代表性文字元素（当前视图里存在且可见的才算）。
    const CONTRAST_TARGETS = [
        '.card-title', '.card-sub', '.hint', '.view-head h1', '.view-head p',
        '.stat-label', '.stat-value', '.kv > span', '.kv > b', '.muted',
        '.radio-text b', '.radio-text i', '.chip', '.field-label', '.field-hint',
        '.btn', '.btn-primary', '.btn-sm', '.status-cell', '.step-name', '.step-detail',
        '.err-line', '.suggestion', '.table th', '.table td', '.empty-card', '.offline-text p'
    ];

    function textRatios() {
        const out = [];
        const seen = {};
        CONTRAST_TARGETS.forEach(function (sel) {
            Array.prototype.slice.call(document.querySelectorAll(sel)).forEach(function (el) {
                if (!visible(el)) return;
                const text = (el.innerText || '').trim();
                if (!text) return;
                const key = sel + '|' + css(el, 'color') + '|' + css(el, 'font-size');
                if (seen[key]) return;
                seen[key] = true;
                const cs = window.getComputedStyle(el);
                const fg = parseColour(cs.color);
                const bg = effectiveBg(el);
                if (!fg) return;
                const size = parseFloat(cs.fontSize);
                const weight = parseInt(cs.fontWeight, 10) || 400;
                out.push({
                    sel: sel,
                    sample: text.slice(0, 24),
                    fg: cs.color,
                    bg: 'rgb(' + Math.round(bg.r) + ', ' + Math.round(bg.g) + ', ' + Math.round(bg.b) + ')',
                    size: size,
                    weight: weight,
                    large: size >= 24 || (size >= 18.66 && weight >= 700),
                    ratio: Math.round(contrast(fg, bg) * 100) / 100
                });
            });
        });
        return out;
    }

    // 侧栏在下、主区在上：拖拽、导航等固定元素也要检查。
    // 这里的类名必须真的会出现在 DOM 上——选择器写错时 querySelectorAll 返回空，
    // 对比度检查会静默少测几处，界面看起来仍然是"全部通过"。
    const KEY_SELECTORS = ['.nav-btn', '.nav-btn.is-active', '.log-time', '.log-msg',
        '.log-scope', '.hero-state', '.hero-sub', '.fact-label', '.fact-value',
        '.stat-label', '.stat-value'];

    function keyRatios() {
        const out = [];
        KEY_SELECTORS.forEach(function (sel) {
            Array.prototype.slice.call(document.querySelectorAll(sel)).forEach(function (el) {
                if (!visible(el)) return;
                const text = (el.innerText || '').trim();
                const cs = window.getComputedStyle(el);
                const fg = parseColour(cs.color);
                const bg = effectiveBg(el);
                if (!text || !fg) return;
                const size = parseFloat(cs.fontSize);
                const weight = parseInt(cs.fontWeight, 10) || 400;
                out.push({
                    sel: sel,
                    sample: text.slice(0, 24),
                    fg: cs.color,
                    bg: 'rgb(' + Math.round(bg.r) + ', ' + Math.round(bg.g) + ', ' + Math.round(bg.b) + ')',
                    size: size,
                    weight: weight,
                    large: size >= 24 || (size >= 18.66 && weight >= 700),
                    ratio: Math.round(contrast(fg, bg) * 100) / 100
                });
            });
        });
        return out;
    }

    function brokenText() {
        const view = document.querySelector('.view:not([hidden])');
        const text = view ? view.innerText : document.body.innerText;
        const out = [];
        ['undefined', 'NaN', '[object Object]'].forEach(function (bad) {
            if (text.indexOf(bad) >= 0) out.push(bad);
        });
        return out;
    }

    window.__ui = {
        selectView: function (name) {
            const btn = document.querySelector('.nav-btn[data-view="' + name + '"]');
            if (!btn) throw new Error('找不到导航按钮: ' + name);
            btn.click();
            return true;
        },
        metrics: function () {
            const view = document.querySelector('.view:not([hidden])');
            const tb = document.querySelector('.titlebar');
            const main = document.querySelector('.main');
            const doc = document.documentElement;
            return {
                theme: window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light',
                viewport: {w: window.innerWidth, h: window.innerHeight},
                view: view ? view.dataset.view : null,
                vars: {
                    titlebarH: css(doc, '--titlebar-h'),
                    radius: css(doc, '--radius'),
                    radiusCard: css(doc, '--radius-card'),
                    controlH: css(doc, '--control-h'),
                    bg: css(doc, '--bg'),
                    text: css(doc, '--text'),
                    text2: css(doc, '--text-2'),
                    text3: css(doc, '--text-3')
                },
                titlebar: rect('.titlebar'),
                titlebarDrag: tb ? css(tb, '--wails-draggable') : null,
                brand: rect('.brand'),
                firstHead: rect('.view:not([hidden]) .view-head'),
                overflow: {
                    docX: doc.scrollWidth - doc.clientWidth,
                    mainX: main ? main.scrollWidth - main.clientWidth : 0,
                    cardOverflow: Array.prototype.slice.call(document.querySelectorAll('.view:not([hidden]) .card'))
                        .filter(function (c) { return c.scrollWidth - c.clientWidth > 1; }).length
                },
                // 视图根节点的直接子块，用来断言版面顺序与密度
                // 注意：视图的块在 .view-body 里，不在 .view 的直接子节点上
                blocks: (function () {
                    const root = view ? view.querySelector('.view-body') : null;
                    if (!root) return [];
                    return Array.prototype.slice.call(root.children).map(function (el) {
                        const r = el.getBoundingClientRect();
                        return {cls: String(el.className).split(' ')[0], top: Math.round(r.top),
                            left: Math.round(r.left), width: Math.round(r.width), height: Math.round(r.height)};
                    });
                })(),
                // 服务器行：每行高度、每行可见按钮数、行内标题可用宽度
                rowHeights: (function () {
                    if (!view) return [];
                    return Array.prototype.slice.call(view.querySelectorAll('.server-row')).map(function (el) {
                        return Math.round(el.getBoundingClientRect().height);
                    });
                })(),
                rowButtons: (function () {
                    const r = view ? view.querySelector('.server-row .row-actions') : null;
                    return r ? r.querySelectorAll('.btn').length : 0;
                })(),
                rowTextWidth: (function () {
                    const t = view ? view.querySelector('.server-row .row-text') : null;
                    return t ? Math.round(t.getBoundingClientRect().width) : 0;
                })(),
                rowTextAlign: (function () {
                    const t = view ? view.querySelector('.server-row .row-text') : null;
                    return t ? getComputedStyle(t).textAlign : null;
                })(),
                // "更多"菜单
                menuOpen: !!(view && view.querySelector('.menu')),
                menuItems: view ? view.querySelectorAll('.menu .menu-item').length : 0,
                menuExpanded: (function () {
                    const b = view ? view.querySelector('[data-act="row-more"]') : null;
                    return b ? b.getAttribute('aria-expanded') : null;
                })(),
                menuInsideRow: (function () {
                    const m = view ? view.querySelector('.menu') : null;
                    return m ? !!m.closest('.server-row') : false;
                })(),
                // 命令行代理：命令文本、复制按钮，以及"命令是否与已保存端口一致"
                cliRows: (function () {
                    if (!view) return [];
                    return Array.prototype.map.call(view.querySelectorAll('.cli-row'), function (r) {
                        const cmd = r.querySelector('.cli-cmd');
                        const btn = r.querySelector('[data-act="copy-cli-proxy"]');
                        const rect = cmd ? cmd.getBoundingClientRect() : null;
                        return {
                            id: r.dataset.cli,
                            command: cmd ? cmd.textContent : null,
                            // 命令是否被容器裁掉（换行后不应再溢出）
                            clipped: cmd ? (cmd.scrollWidth - cmd.clientWidth) : 0,
                            hasButton: !!btn,
                            buttonId: btn ? btn.dataset.id : null,
                            // 命令必须能选中，否则用户没法手动复制
                            selectable: cmd ? (getComputedStyle(cmd).webkitUserSelect || '') !== 'none' : false,
                            width: rect ? Math.round(rect.width) : 0
                        };
                    });
                })(),
                cliNote: text(view, '.cli-note'),
                cliBlockHeight: (function () {
                    const b = view ? view.querySelector('.cli-block') : null;
                    return b ? Math.round(b.getBoundingClientRect().height) : 0;
                })(),
                cliHTTPOff: !!text(view, '.cli-block .hint'),
                // "模式"卡：4 个"菜单栏可切换的模式"复选框是否与该卡的模式单选同处一张卡，
                // 以及它们排成几行几列（宽窗要求 2×2，窄窗回退单列）。
                modeCardHasSwitch: (function () {
                    if (!view) return false;
                    const cards = view.querySelectorAll('.card');
                    for (let i = 0; i < cards.length; i++) {
                        const radios = cards[i].querySelectorAll('.radio').length;
                        const switchBox = cards[i].querySelector('input[data-key="enablePAC"]');
                        if (radios > 0 && switchBox) return true;
                    }
                    return false;
                })(),
                switchCheckRows: (function () {
                    const list = view ? view.querySelector('.check-list.is-2col') : null;
                    if (!list) return 0;
                    const tops = {};
                    Array.prototype.forEach.call(list.children, function (el) {
                        tops[Math.round(el.getBoundingClientRect().top)] = 1;
                    });
                    return Object.keys(tops).length;
                })(),
                switchSwitchCols: (function () {
                    const list = view ? view.querySelector('.check-list.is-2col') : null;
                    if (!list) return 0;
                    const raw = String(getComputedStyle(list).gridTemplateColumns || '').trim();
                    if (!raw || raw === 'none') return 1;
                    return raw.split(/\s+/).length;
                })(),
                clipboard: window.__CLIPBOARD ? window.__CLIPBOARD.text : null,
                clipboardCount: window.__CLIPBOARD ? window.__CLIPBOARD.count : 0,
                // 诊断表格
                tableRows: view ? view.querySelectorAll('table.table tbody tr').length : 0,
                tableOverflow: (function () {
                    const w = view ? view.querySelector('.table-wrap') : null;
                    return w ? w.scrollWidth - w.clientWidth : 0;
                })(),
                // PAC：第一张卡里显示规则来源的那一行
                pacSourceLabel: (function () {
                    const rows = view ? view.querySelectorAll('.card .kv') : [];
                    for (let i = 0; i < rows.length; i++) {
                        const b = rows[i].querySelector('b');
                        if (b && b.querySelector('span[title]')) return rows[i].querySelector('span').innerText.trim();
                    }
                    return null;
                })(),
                // 模式单选：行数，以及"说明与标题同行"（单行）的个数
                radioRows: view ? view.querySelectorAll('.radio').length : 0,
                radioSingleLine: (function () {
                    if (!view) return 0;
                    let n = 0;
                    Array.prototype.forEach.call(view.querySelectorAll('.radio'), function (el) {
                        const t = el.querySelector('.radio-text');
                        if (!t) return;
                        const b = t.querySelector('b');
                        const i = t.querySelector('i');
                        if (b && i && Math.abs(b.getBoundingClientRect().top - i.getBoundingClientRect().top) <= 2) n++;
                    });
                    return n;
                })(),
                // 特权助手卡：kv 栏数 + 是否重复显示"已运行"
                kvGridCols: (function () {
                    const g = view ? view.querySelector('.kv-grid') : null;
                    if (!g) return 0;
                    const raw = String(getComputedStyle(g).gridTemplateColumns || '').trim();
                    if (!raw || raw === 'none') return 1;
                    return raw.split(/\s+/).length;
                })(),
                dupUptime: (function () {
                    const c = view ? view.querySelector('.card.is-wide') : null;
                    if (!c) return -1;
                    let n = 0;
                    Array.prototype.forEach.call(c.querySelectorAll('.kv > span'), function (el) {
                        if (String(el.textContent).indexOf('已运行') >= 0) n++;
                    });
                    return n;
                })(),
                // .grid-2 里的卡片：用来检查奇数张卡片有没有留下半行空洞
                gridCards: (function () {
                    const g = view ? view.querySelector('.grid-2') : null;
                    if (!g) return [];
                    return Array.prototype.slice.call(g.children).map(function (el) {
                        const r = el.getBoundingClientRect();
                        return {cls: String(el.className), left: Math.round(r.left),
                            width: Math.round(r.width), top: Math.round(r.top), height: Math.round(r.height)};
                    });
                })(),
                mainScroll: main ? {scroll: main.scrollHeight, client: main.clientHeight} : null,
                contrast: textRatios().concat(keyRatios()),
                textLen: view ? (view.innerText || '').trim().length : 0,
                broken: brokenText(),
                navActive: (document.querySelector('.nav-btn.is-active') || {}).dataset
                    ? document.querySelector('.nav-btn.is-active').dataset.view : null,
                // 以下都只在"当前可见的视图"里找：隐藏视图的 DOM 还在，不限定范围会误判
                modeSelected: text(view, '.radio.is-selected', function (el) { return el.dataset.mode; }),
                radioCount: view ? view.querySelectorAll('.radio').length : 0,
                factCount: view ? view.querySelectorAll('.fact').length : 0,
                heroState: text(view, '.hero-state'),
                heroSub: text(view, '.hero-sub'),
                heroPrimary: text(view, '.hero-actions .btn-primary, .hero-actions .btn-off',
                    function (el) { return el.dataset.act; }),
                heroPrimaryText: text(view, '.hero-actions .btn-primary, .hero-actions .btn-off'),
                callCount: window.__UI_CALLS ? window.__UI_CALLS() : 0
            };
        }
    };
})();
