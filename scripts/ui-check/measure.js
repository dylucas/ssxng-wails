#!/usr/bin/env node
// 用本机缓存的 chrome-headless-shell（CDP 驱动，零依赖）对 SSXNG 界面做
// 可断言的检查：标题栏/拖动区几何、横向溢出、文字对比度、变量完整性。
//
//   node scripts/ui-check/measure.js <harness 目录> <chrome-headless-shell 路径>
//
// 退出码：0 = 全部通过；1 = 有断言失败。
'use strict';

const fs = require('node:fs');
const path = require('node:path');
const {spawn} = require('node:child_process');

const dir = process.argv[2];
const chromePath = process.argv[3];
// 仓库根目录：静态检查需要读 internal/api/types.go，harness 目录里没有它。
const repoRoot = process.argv[4] || path.resolve(__dirname, '..', '..');
if (!dir || !chromePath) {
    console.error('用法: measure.js <harness 目录> <chrome 路径> [仓库根目录]');
    process.exit(2);
}

const failures = [];
const passes = [];
const density = [];

function check(ok, msg) {
    if (ok) passes.push(msg);
    else failures.push(msg);
}

// ---------------------------------------------------------------- 静态检查
// main.css 里出现的颜色字面量必须只出现在自定义属性声明里，
// 否则浅色模式就会拿到深色模式写死的颜色。
function staticCssCheck(cssPath) {
    const lines = fs.readFileSync(cssPath, 'utf8').split('\n');
    const literal = /(#[0-9a-fA-F]{3,8}\b|rgba?\()/;
    const decl = /^\s*--[A-Za-z0-9-]+\s*:/;
    const used = new Set();
    const defined = new Set();
    lines.forEach(function (line, i) {
        const code = line.replace(/\/\*.*?\*\//g, '');
        if (literal.test(code) && !decl.test(code)) {
            check(false, 'main.css:' + (i + 1) + ' 硬编码颜色（应改成变量）: ' + line.trim());
        }
        let m;
        const useRe = /var\((--[A-Za-z0-9-]+)/g;
        while ((m = useRe.exec(code)) !== null) used.add(m[1]);
        const defRe = /(--[A-Za-z0-9-]+)\s*:/g;
        while ((m = defRe.exec(code)) !== null) defined.add(m[1]);
    });
    const missing = Array.from(used).filter(function (n) { return !defined.has(n); }).sort();
    check(missing.length === 0, 'main.css 里 var() 引用的变量都有定义' + (missing.length ? '，缺失: ' + missing.join(', ') : ''));
    return {used: used.size, defined: defined.size};
}

function staticHtmlCheck(htmlPath) {
    const html = fs.readFileSync(htmlPath, 'utf8');
    check(/class="titlebar"/.test(html), 'index.html 里有 .titlebar 拖动区元素');
    // CSS 里定义的类名与 app.js/index.html 里用到的类名对不上时，样式会静默失效。
    check(!/main\.js/.test(html), 'index.html 不再引用已删除的 main.js');
}

// 前端拆成四个脚本后，"加载顺序"和"各自一个 IIFE"都成了隐性约定：顺序错则某些
// 名字解构出来是 undefined，漏掉 IIFE 则会把所有内部名字泄漏成全局。
function staticScriptOrderCheck(htmlPath) {
    const html = fs.readFileSync(htmlPath, 'utf8');
    const srcs = [];
    const re = /<script[^>]*\ssrc="\.\/([\w.-]+\.js)"[^>]*>/g;
    let m;
    while ((m = re.exec(html)) !== null) srcs.push(m[1]);
    const app = srcs.filter(function (s) { return s !== 'stub.js' && s !== 'metrics.js'; });
    const want = ['i18n.js', 'app-core.js', 'app-views.js', 'app-actions.js', 'app.js'];
    check(app.join(',') === want.join(','),
        'app 脚本按 i18n → core → views → actions → app 的顺序加载 (' + app.join(' → ') + ')');
    check(srcs[srcs.length - 1] === 'metrics.js',
        '度量脚本挂在最后一个 app 脚本之后 (' + srcs[srcs.length - 1] + ')');

    // i18n.js 只是一个文案表（window.T = {...}），不参与 IIFE 约定；
    // app-core.js 是第一个 app 脚本，它自己创建命名空间，不需要"安全退出"。
    const wrapped = want.slice(1);
    wrapped.forEach(function (name) {
        let text;
        try {
            text = fs.readFileSync(path.join(dir, name), 'utf8');
        } catch (e) {
            check(false, '脚本 ' + name + ' 可读');
            return;
        }
        const lines = text.split('\n');
        const opens = lines.filter(function (l) { return l.trim() === '(function () {'; }).length;
        const closes = lines.filter(function (l) { return l.trim() === '})();'; }).length;
        check(opens === 1 && closes === 1,
            name + ' 用一个 IIFE 包住（开 ' + opens + ' / 闭 ' + closes + '）');
        check(!/^\s*(import|export)\s/m.test(text), name + ' 不是 ES 模块（无 import/export）');
        const topLevel = lines.filter(function (l) {
            return /^(function|const|let|var|class)\s/.test(l);
        });
        check(topLevel.length === 0,
            name + ' 没有顶层声明 (' + topLevel.length + ' 处)');
    });

    // core 之后的每个脚本都要先确认命名空间存在再解构。
    wrapped.slice(1).forEach(function (name) {
        const text = fs.readFileSync(path.join(dir, name), 'utf8');
        check(/const S = window\.SSXNG;\s*\n\s*if \(!S\) return;/.test(text),
            name + ' 在核心脚本缺失时安全退出');
    });
}

// 完整校验四个脚本之间的名字接线，而不是抽查几个名字：
//   · 每个 `const {a, b} = S;` 里的名字，必须由"更早加载"的文件导出；
//   · 每处 `S.name` 引用的名字，必须由某个文件导出。
// 拆分后新增跨文件调用却忘了导出、或把解构写到了赋值之前，都会在这里失败。
function staticNamespaceCheck() {
    const order = ['app-core.js', 'app-views.js', 'app-actions.js', 'app.js'];
    const src = {};
    order.forEach(function (f) {
        src[f] = fs.readFileSync(path.join(dir, f), 'utf8');
    });

    function code(text) {
        // 去掉注释与字符串，避免把文案里的 S.xxx 当成引用。
        return text.replace(/\/\*[\s\S]*?\*\//g, ' ')
            .split('\n').map(function (l) { return l.replace(/\/\/.*$/, ''); }).join('\n')
            .replace(/'(?:[^'\\]|\\.)*'/g, "''")
            .replace(/"(?:[^"\\]|\\.)*"/g, '""');
    }

    const exported = {};      // file -> [names]
    const destructured = {};  // file -> [names]
    const used = {};          // file -> Set(S.name targets)

    order.forEach(function (f) {
        const text = code(src[f]);
        const ex = [];
        const asg = /Object\.assign\(S,\s*\{([\s\S]*?)\}\)/.exec(text);
        if (asg) {
            asg[1].split(/[,\n]/).forEach(function (p) {
                p = p.trim();
                if (p) ex.push(p);
            });
        }
        exported[f] = ex;

        const ds = [];
        const dasg = /const\s*\{([\s\S]*?)\}\s*=\s*S;/.exec(text);
        if (dasg) {
            dasg[1].split(/[,\n]/).forEach(function (p) {
                p = p.trim();
                if (p) ds.push(p);
            });
        }
        destructured[f] = ds;

        const set = new Set();
        const re = /(?<![\w$.])S\.([A-Za-z_$][\w$]*)/g;
        let m;
        while ((m = re.exec(text)) !== null) set.add(m[1]);
        used[f] = set;
    });

    check(order.every(function (f) { return exported[f].length > 0; }),
        '四个脚本都往 window.SSXNG 上导出名字 (' +
        order.map(function (f) { return f + ':' + exported[f].length; }).join(' ') + ')');

    order.forEach(function (f, idx) {
        const earlier = new Set();
        order.slice(0, idx).forEach(function (g) {
            exported[g].forEach(function (n) { earlier.add(n); });
        });
        const missing = destructured[f].filter(function (n) { return !earlier.has(n); });
        check(missing.length === 0,
            f + ' 解构的名字都由更早的脚本导出' +
            (missing.length ? '，缺失: ' + missing.join(', ') : ''));
    });

    const all = new Set();
    order.forEach(function (f) { exported[f].forEach(function (n) { all.add(n); }); });
    order.forEach(function (f) {
        const missing = Array.from(used[f]).filter(function (n) { return !all.has(n); });
        check(missing.length === 0,
            f + ' 里每处 S.name 都能在某个脚本的导出里找到' +
            (missing.length ? '，未导出: ' + missing.join(', ') : ''));
    });
}

// stub.js 的 fixture 必须覆盖 internal/api/types.go 里的字段：少字段时前端
// normalizer 会走默认值，视图静默变空，而 ui-check 只看"非空"就会放过。
function staticAPIShapeCheck(repoRoot) {
    const types = fs.readFileSync(path.join(repoRoot, 'internal/api/types.go'), 'utf8');
    const stub = fs.readFileSync(path.join(dir, 'stub.js'), 'utf8');

    function jsonTags(typeName) {
        const m = new RegExp('type ' + typeName + ' struct \\{([\\s\\S]*?)\\n\\}').exec(types);
        if (!m) return null;
        const tags = [];
        const re = /`json:"([^",]+)"/g;
        let t;
        while ((t = re.exec(m[1])) !== null) tags.push(t[1]);
        return tags;
    }

    const settings = jsonTags('SettingsView');
    check(settings !== null, 'types.go 里能找到 SettingsView');
    if (settings) {
        const missing = settings.filter(function (k) {
            return !new RegExp('(^|[\\s{,])' + k + '\\s*:').test(stub);
        });
        check(missing.length === 0,
            'stub.js 的 settings fixture 覆盖 SettingsView 全部字段' +
            (missing.length ? '，缺失: ' + missing.join(', ') : ''));
    }

    const stateFields = jsonTags('State');
    check(stateFields !== null, 'types.go 里能找到 State');
    if (stateFields) {
        const missing = stateFields.filter(function (k) {
            return !new RegExp('(^|\\n)\\s*' + k + '\\s*:').test(stub);
        });
        check(missing.length === 0,
            'stub.js 的 state fixture 覆盖 State 全部字段' +
            (missing.length ? '，缺失: ' + missing.join(', ') : ''));
    }
}

// ---------------------------------------------------------------- CDP 客户端
function rpc(url) {
    return new Promise(function (resolve, reject) {
        const ws = new WebSocket(url);
        let nextId = 1;
        const pending = new Map();
        const eventWaiters = [];
        const eventListeners = {};
        ws.addEventListener('message', function (ev) {
            let msg;
            try { msg = JSON.parse(ev.data); } catch (e) { return; }
            if (msg.id && pending.has(msg.id)) {
                const p = pending.get(msg.id);
                pending.delete(msg.id);
                if (msg.error) p.reject(new Error(msg.error.message));
                else p.resolve(msg.result);
                return;
            }
            if (msg.method) {
                (eventListeners[msg.method] || []).forEach(function (cb) {
                    try { cb(msg.params); } catch (e) { /* 监听器自己的错不该拖垮检查 */ }
                });
                for (let i = eventWaiters.length - 1; i >= 0; i--) {
                    if (eventWaiters[i].method === msg.method) {
                        const w = eventWaiters.splice(i, 1)[0];
                        w.resolve(msg.params);
                    }
                }
            }
        });
        ws.addEventListener('error', function () { reject(new Error('DevTools WebSocket 连接失败: ' + url)); });
        ws.addEventListener('open', function () {
            resolve({
                send: function (method, params, sessionId) {
                    const id = nextId++;
                    const payload = {id: id, method: method, params: params || {}};
                    if (sessionId) payload.sessionId = sessionId;
                    return new Promise(function (res, rej) {
                        pending.set(id, {resolve: res, reject: rej});
                        ws.send(JSON.stringify(payload));
                    });
                },
                waitFor: function (method, timeoutMs) {
                    return new Promise(function (res, rej) {
                        const w = {method: method, resolve: res};
                        eventWaiters.push(w);
                        setTimeout(function () {
                            const i = eventWaiters.indexOf(w);
                            if (i >= 0) { eventWaiters.splice(i, 1); rej(new Error('等待事件超时: ' + method)); }
                        }, timeoutMs || 20000);
                    });
                },
                on: function (method, cb) {
                    (eventListeners[method] = eventListeners[method] || []).push(cb);
                },
                close: function () { try { ws.close(); } catch (e) { /* 忽略 */ } }
            });
        });
    });
}

function launch(chrome, profileDir) {
    const proc = spawn(chrome, [
        '--headless',
        '--no-sandbox',
        '--disable-gpu',
        '--hide-scrollbars',
        '--disable-lcd-text',
        '--force-device-scale-factor=1',
        '--remote-debugging-port=0',
        '--user-data-dir=' + profileDir,
        'about:blank'
    ], {stdio: ['ignore', 'ignore', 'pipe']});

    return new Promise(function (resolve, reject) {
        let buf = '';
        const timer = setTimeout(function () {
            try { proc.kill('SIGKILL'); } catch (e) { /* 忽略 */ }
            reject(new Error('等待 DevTools 端口超时'));
        }, 25000);
        proc.stderr.on('data', function (d) {
            buf += d.toString();
            const m = /ws:\/\/[^\s]+/.exec(buf);
            if (m) {
                clearTimeout(timer);
                resolve({proc: proc, wsURL: m[0]});
            }
        });
        proc.on('exit', function (code) {
            clearTimeout(timer);
            reject(new Error('chrome-headless-shell 提前退出（code=' + code + '）\n' + buf));
        });
    });
}

// 滚动主区域后，标题栏那条带状区域必须是"平的"：如果内容能滑到红绿灯下面，
// 截图里就会出现文字的明暗像素。用页面里的 canvas 直接读截图像素来判断。
async function bandFlatness(s, evaluate, scrollPx) {
    await evaluate('(function(){var m=document.querySelector(".main");if(m){m.scrollTop=' + scrollPx + ';}return true;})()');
    await new Promise(function (r) { setTimeout(r, 100); });
    const shot = await s('Page.captureScreenshot', {format: 'png'});
    const expr = '(function (b64) {' +
        'return new Promise(function (resolve) {' +
        'var img = new Image();' +
        'img.onload = function () {' +
        'var c = document.createElement("canvas");' +
        'c.width = img.width; c.height = img.height;' +
        'var ctx = c.getContext("2d");' +
        'ctx.drawImage(img, 0, 0);' +
        'var tbh = parseFloat(getComputedStyle(document.documentElement).getPropertyValue("--titlebar-h"));' +
        'var scale = img.width / window.innerWidth;' +
        'var h = Math.max(1, Math.round(tbh * scale) - 6);' +
        'var data = ctx.getImageData(0, 2, img.width, h).data;' +
        'var seen = {}, maxL = -1, minL = 2;' +
        'for (var i = 0; i < data.length; i += 4) {' +
        'var key = data[i] + "," + data[i+1] + "," + data[i+2];' +
        'seen[key] = (seen[key] || 0) + 1;' +
        'var l = (0.2126*data[i] + 0.7152*data[i+1] + 0.0722*data[i+2]) / 255;' +
        'if (l > maxL) maxL = l; if (l < minL) minL = l;' +
        '}' +
        'var keys = Object.keys(seen).sort(function (a, b) { return seen[b] - seen[a]; });' +
        'resolve({colours: keys.length, top: keys.slice(0, 3), spread: Math.round((maxL - minL) * 1000) / 1000});' +
        '};' +
        'img.onerror = function () { resolve({error: "截图解码失败"}); };' +
        'img.src = "data:image/png;base64," + b64;' +
        '});' +
        '})(' + JSON.stringify(shot.data) + ')';
    const r = await s('Runtime.evaluate', {expression: expr, returnByValue: true, awaitPromise: true});
    if (r.exceptionDetails) throw new Error('带状区域分析失败: ' + JSON.stringify(r.exceptionDetails));
    return r.result.value;
}

function parseMetrics(value) {
    if (!value || typeof value !== 'object') throw new Error('页面没有返回度量结果: ' + JSON.stringify(value));
    return value;
}

const WIDTHS = [980, 880, 700];
const THEMES = ['dark', 'light'];
const VIEWS = ['status', 'servers', 'modes', 'pac', 'logs', 'diag'];

function assertGeometry(m, label) {
    const tbH = parseFloat(m.vars.titlebarH);
    check(m.titlebarDrag === 'drag', label + ' · .titlebar 声明 --wails-draggable: drag');
    check(!!m.titlebar && Math.abs(m.titlebar.height - tbH) <= 1,
        label + ' · .titlebar 高度 = ' + (m.titlebar ? m.titlebar.height : '—') + ' 与 --titlebar-h(' + tbH + ') 一致');
    check(!!m.brand && m.brand.top >= tbH - 1,
        label + ' · 侧栏品牌在标题栏之下 (brand.top=' + (m.brand ? m.brand.top : '—') + ')');
    check(!!m.firstHead && m.firstHead.top >= tbH - 1,
        label + ' · 视图标题在标题栏之下 (top=' + (m.firstHead ? m.firstHead.top : '—') + ')');
    check(m.overflow.docX <= 0, label + ' · 文档无横向溢出 (docX=' + m.overflow.docX + ')');
    check(m.overflow.mainX <= 0, label + ' · 主区域无横向溢出 (mainX=' + m.overflow.mainX + ')');
    check(m.overflow.cardOverflow === 0, label + ' · 卡片内无内容溢出 (' + m.overflow.cardOverflow + ')');
    check(m.broken.length === 0, label + ' · 没有 undefined/NaN/[object Object]');
}

function assertContrast(m, label) {
    m.contrast.forEach(function (c) {
        const need = c.large ? 3 : 4.5;
        check(c.ratio >= need,
            label + ' · 对比度 ' + c.sel + ' "' + c.sample + '" ' + c.ratio + ':1 ≥ ' + need +
            '  [' + c.fg + ' on ' + c.bg + ', ' + c.size + 'px/' + c.weight + ']');
    });
}

async function main() {
    staticCssCheck(path.join(dir, 'main.css'));
    staticHtmlCheck(path.join(dir, 'harness.html'));
    staticScriptOrderCheck(path.join(dir, 'harness.html'));
    staticNamespaceCheck();
    staticAPIShapeCheck(repoRoot);

    const run = await launch(chromePath, path.join(dir, 'profile'));
    const client = await rpc(run.wsURL);
    // 页面未捕获异常一律当成失败：脚本抛错时界面可能只是静默变空，后面的断言
    // 会以各种奇怪的方式失败（例如读 null.indexOf），看不出真正原因。
    const pageErrors = [];
    let exitCode = 0;
    try {
        const {targetId} = await client.send('Target.createTarget', {url: 'about:blank'});
        const {sessionId} = await client.send('Target.attachToTarget', {targetId: targetId, flatten: true});
        const s = function (method, params) { return client.send(method, params, sessionId); };

        await s('Page.enable');
        await s('Runtime.enable');
        client.on('Runtime.exceptionThrown', function (p) {
            const d = (p && p.exceptionDetails) || {};
            const desc = (d.exception && (d.exception.description || d.exception.value)) || d.text;
            pageErrors.push(String(desc).split('\n')[0]);
        });
        const loaded = client.waitFor('Page.loadEventFired', 30000);
        await s('Page.navigate', {url: 'file://' + path.join(dir, 'harness.html')});
        await loaded;

        async function evaluate(expr) {
            const r = await s('Runtime.evaluate', {expression: expr, returnByValue: true, awaitPromise: true});
            if (r.exceptionDetails) {
                throw new Error('页面脚本出错: ' + JSON.stringify(r.exceptionDetails.exception || r.exceptionDetails));
            }
            return r.result.value;
        }

        // 0) 命名空间：四个脚本都跑到了，而且各自把该公开的名字放上了 window.SSXNG。
        // 静态检查只能证明"顺序写对了"，这里证明"顺序真的能跑通"——某个文件漏了
        // Object.assign，或解构发生在赋值之前，都会在这里立刻暴露成 undefined。
        // 只列真正跨文件用到的名字（各文件的内部函数不导出，故意不在这里断言）。
        const ns = await evaluate('(function(){' +
            'var S=window.SSXNG;' +
            'if(!S)return {ok:false,why:"window.SSXNG 缺失"};' +
            'var need={ "app-core.js":["T","App","esc","state","call","refreshState",' +
            '"normalizeState","copyText","fmtBytes","pkey","toast"],' +
            '"app-views.js":["renderView","renderSidebar","applyState","bindEvents","setView"],' +
            '"app-actions.js":["ACTIONS","loadLogs","loadServices","updateDraftField",' +
            '"updateEditorField","updateServiceToggle","validateServer","refreshSettingsFeedback"],' +
            '"app.js":["boot"] };' +
            'var bad=[];' +
            'Object.keys(need).forEach(function(f){need[f].forEach(function(k){' +
            'if(S[k]===undefined)bad.push(f+":"+k);});});' +
            'return {ok:bad.length===0, why:bad.join(","), keys:Object.keys(S).length,' +
            'bootRan:!!(S.App&&S.App.state)};})()');
        check(ns && ns.ok, '四个脚本都执行了且 window.SSXNG 暴露了预期接口' +
            (ns && ns.why ? '，缺失: ' + ns.why : ''));
        check(ns && ns.keys > 30, 'window.SSXNG 导出的名字数量合理 (' + (ns && ns.keys) + ')');
        check(ns && ns.bootRan, 'app.js 跑完了启动流程（状态已进 App.state）');
        // 拆分不能让内部名字泄漏成全局：抽查几个只应存在于闭包里的名字。
        const leaked = await evaluate('["esc","pkey","normalizeState","ACTIONS","renderView","closeMenu"]' +
            '.filter(function(n){return n in window;})');
        check(leaked.length === 0,
            '拆分后没有把内部名字泄漏成全局变量' + (leaked.length ? ': ' + leaked.join(', ') : ''));
        check(pageErrors.length === 0,
            '页面加载与启动期间没有未捕获异常' +
            (pageErrors.length ? ': ' + pageErrors.slice(0, 3).join(' | ') : ''));

        // 不改视图、只改视口与主题的度量：用于"刚点开菜单"这类不能被重绘打断的场景
        async function snap(width, height, theme) {
            await s('Emulation.setDeviceMetricsOverride', {
                width: width, height: height, deviceScaleFactor: 1, mobile: false
            });
            await s('Emulation.setEmulatedMedia', {
                media: 'screen',
                features: [{name: 'prefers-color-scheme', value: theme}]
            });
            await new Promise(function (r) { setTimeout(r, 60); });
            return parseMetrics(await evaluate('window.__ui.metrics()'));
        }

        async function measure(width, height, theme, view) {
            await s('Emulation.setDeviceMetricsOverride', {
                width: width, height: height, deviceScaleFactor: 1, mobile: false
            });
            await s('Emulation.setEmulatedMedia', {
                media: 'screen',
                features: [{name: 'prefers-color-scheme', value: theme}]
            });
            await evaluate('window.__ui.selectView(' + JSON.stringify(view) + ')');
            await new Promise(function (r) { setTimeout(r, 60); });
            return parseMetrics(await evaluate('window.__ui.metrics()'));
        }

        // 1) 主题 × 宽度：状态页几何 + 对比度 + "滚动不钻到标题栏下面"
        for (const theme of THEMES) {
            for (const width of WIDTHS) {
                const label = theme + '/' + width;
                const m = await measure(width, 680, theme, 'status');
                assertGeometry(m, label);
                assertContrast(m, label);
                check(m.theme === theme, label + ' · 主题媒体查询生效 (' + m.theme + ')');
                const flat = await bandFlatness(s, evaluate, 120);
                check(!!flat && !flat.error && flat.spread <= 0.15 && flat.colours <= 32,
                    label + ' · 滚动 120px 后标题栏区域仍是平面（无内容钻到红绿灯下）' +
                    (flat ? ' [色数=' + flat.colours + ' 明暗差=' + flat.spread + ']' : ''));
                await evaluate('(function(){var m=document.querySelector(".main");if(m){m.scrollTop=0;}return true;})()');
            }
        }

        // 2) 每个视图都要有内容，并且深浅两套主题下对比度都达标
        for (const view of VIEWS) {
            for (const theme of THEMES) {
                const label = theme + '/980/' + view;
                const m = await measure(980, 680, theme, view);
                check(m.view === view, label + ' · 视图已切换 (' + m.view + ')');
                check(m.navActive === view, label + ' · 侧栏高亮跟随 (' + m.navActive + ')');
                check(m.textLen > 40, label + ' · 视图渲染出内容 (' + m.textLen + ' 字符)');
                check(m.broken.length === 0, label + ' · 没有 undefined/NaN');
                check(m.overflow.mainX <= 0, label + ' · 无横向溢出');
                assertContrast(m, label);
            }
        }

        // 2b) 窄窗（侧栏折叠）下同样不允许溢出/裁切：
        //     模式单选的说明改成与标题同行后，窄窗最容易在这里出问题
        for (const view of VIEWS) {
            const label = 'narrow/700/' + view;
            const m = await measure(700, 680, 'dark', view);
            check(m.textLen > 40, label + ' · 视图渲染出内容');
            assertGeometry(m, label);
            assertContrast(m, label);
        }
        const radio = await measure(700, 680, 'dark', 'modes');
        check(radio.radioRows > 0 && radio.radioSingleLine === radio.radioRows,
            '窄窗下每个模式单选的说明仍与标题同行 (' + radio.radioSingleLine + '/' + radio.radioRows + ')');

        // 3) 状态页：顶部状态条 + 关键事实 + 状态自适应主按钮
        const status = await measure(980, 680, 'dark', 'status');
        check(!!status.heroState, '状态页有顶部状态条 (' + status.heroState + ')');
        check(status.heroState.indexOf('代理运行中') >= 0, '开启时状态条显示"代理运行中" (' + status.heroState + ')');
        check((status.heroSub || '').indexOf('全局代理') >= 0, '状态条显示当前模式 (' + status.heroSub + ')');
        check((status.heroSub || '').indexOf('演示节点') >= 0, '状态条显示当前节点 (' + status.heroSub + ')');
        check(status.heroPrimary === 'toggle-power' && (status.heroPrimaryText || '').indexOf('关闭') >= 0,
            '开启时主按钮是"关闭代理" (' + status.heroPrimary + ': ' + status.heroPrimaryText + ')');
        check(status.factCount === 4, '关键事实一行显示 4 项 (' + status.factCount + ')');
        const order = status.blocks.map(function (b) { return b.cls; });
        check(order[0] === 'hero' && order[1] === 'facts' && order[2] === 'card',
            '状态页版面顺序：状态条 → 关键事实 → 流量 (' + order.join(' → ') + ')');
        // 4) 版面密度护栏：页高、单列、跨列不留空洞
        //    基线：模式与代理 1498px、状态 970px（视口 680px）。数值变了就说明版面退化。
        //    阈值 = 实测值 + 约 25px 余量；改动版面时请连同这里的注释一起更新。
        check(status.mainScroll.scroll <= 820, '状态页内容高 ≤ 820px（基线 970，实测 ' + status.mainScroll.scroll + '）');
        check(status.kvGridCols === 2, '特权助手卡在宽窗下 kv 排两栏 (' + status.kvGridCols + ')');
        check(status.dupUptime === 0, '特权助手卡不再重复显示"已运行"（已由顶部关键事实承载）(' + status.dupUptime + ')');

        const wide = status.gridCards.filter(function (c) { return c.cls.indexOf('is-wide') >= 0; });
        check(wide.length === 1, '状态页只有一张跨整行的卡片 (' + wide.length + ')');
        const colW = status.gridCards.length ? status.gridCards[0].width : 0;
        check(wide.length === 1 && wide[0].width > colW * 1.8,
            '跨行卡片占满两栏，右边不再留半行空洞 (' + (wide[0] ? wide[0].width : 0) + ' vs ' + colW + ')');
        check(status.gridCards.every(function (c) {
            return c.width >= colW - 1 && c.width <= wide[0].width + 1;
        }), '状态页没有宽度异常的卡片');

        const modesPage = await measure(980, 680, 'dark', 'modes');
        // 本轮的基线：把"菜单栏可切换的模式"并进"模式"卡（少一张卡），实测 1409px。
        // 历史门槛：1275 → 1450（新增"命令行代理"块）→ 本轮合并后收回到 1435。
        // 阈值 = 实测 + 约 26px 余量。新增块自身仍单独设上界（见下），
        // 免得以后悄悄膨胀还被这条总高度断言吸收掉。
        check(modesPage.mainScroll.scroll <= 1435,
            '模式与代理页内容高 ≤ 1435px（合并"菜单栏可切换的模式"后基线 1409，实测 ' + modesPage.mainScroll.scroll + '）');
        check(modesPage.cliBlockHeight > 0 && modesPage.cliBlockHeight <= 200,
            '"命令行代理"块高度受控 ≤ 200px（实测 ' + modesPage.cliBlockHeight + 'px）');
        const cols = modesPage.blocks.filter(function (b) { return b.cls === 'card'; });
        check(cols.length >= 2 && cols.every(function (c) {
            return c.left === cols[0].left && c.width === cols[0].width;
        }), '模式与代理页保持单列，没有改成分栏 (' + cols.length + ' 张卡)');
        check(modesPage.blocks.every(function (b) { return b.top >= 0; }), '模式与代理页没有元素被挤出视口上方');
        // 合并后的"模式"卡：5 个模式单选 + 4 个切换模式复选框同处一张卡，
        // 后者在宽窗固定两列（2+2），不能排成 3+1 留一个孤格。
        check(modesPage.modeCardHasSwitch === true,
            '"菜单栏可切换的模式"与模式单选合并进同一张卡 (' + modesPage.modeCardHasSwitch + ')');
        check(modesPage.switchCheckRows === 2 && modesPage.switchSwitchCols === 2,
            '切换模式 4 项在宽窗排成 2×2 (' + modesPage.switchCheckRows + ' 行 × ' +
            modesPage.switchSwitchCols + ' 列)');
        density.push('模式与代理页内容高 ' + modesPage.mainScroll.scroll + 'px / 视口 ' + modesPage.mainScroll.client + 'px，' +
            cols.length + ' 张卡片');
        density.push('状态页内容高 ' + status.mainScroll.scroll + 'px / 视口 ' + status.mainScroll.client + 'px');

        // 4b) 服务器页：节点变多 + 窗口变窄时的退化
        //     基线：8 个节点 @700px 内容高 1495px、行高 103px、每行 7 个按钮、标题只剩 134px
        const filled = await evaluate('window.__UI_FILL_SERVERS(8)');
        check(filled === 8, '注入 8 个节点成功 (' + filled + ')');
        const many = await measure(700, 680, 'dark', 'servers');
        check(many.rowHeights.length === 8, '渲染出 8 行节点 (' + many.rowHeights.length + ')');
        check(many.rowButtons === 5, '每行 4 个动作 + 1 个"更多"（原为 7 个）(' + many.rowButtons + ')');
        check(Math.max.apply(null, many.rowHeights) <= 80,
            '窄窗单行高 ≤ 80px（基线 103，实测 ' + Math.max.apply(null, many.rowHeights) + '）');
        check(many.mainScroll.scroll <= 1200,
            '8 节点窄窗内容高 ≤ 1200px（基线 1495，实测 ' + many.mainScroll.scroll + '）');
        check(many.rowTextWidth >= 190,
            '窄窗行内标题可用宽度 ≥ 190px（基线 134，实测 ' + many.rowTextWidth + '）');
        check(many.rowTextAlign === 'left', '行内文字保持左对齐 (' + many.rowTextAlign + ')');
        assertGeometry(many, 'servers/700/8 节点');
        assertContrast(many, 'servers/700/8 节点');

        const manyWide = await measure(980, 680, 'dark', 'servers');
        check(manyWide.mainScroll.scroll <= 760,
            '8 节点宽窗内容高 ≤ 760px（基线 810，实测 ' + manyWide.mainScroll.scroll + '）');
        density.push('服务器页 8 节点：700px 高 ' + many.mainScroll.scroll + 'px（行高 ' +
            Math.max.apply(null, many.rowHeights) + '）、980px 高 ' + manyWide.mainScroll.scroll + 'px');

        // 4c) "更多"菜单：打开 / 点空白收起 / Escape 收起 / 菜单项完整
        await evaluate('window.__ui.selectView("servers")');
        await evaluate('document.querySelector(\'[data-act="row-more"]\').click()');
        await new Promise(function (r) { setTimeout(r, 60); });
        const opened = await snap(980, 680, 'dark');
        check(opened.menuOpen && opened.menuItems === 3, '点"更多"后菜单出现且含 3 项 (' +
            opened.menuItems + ')');
        check(opened.menuExpanded === 'true', '触发按钮 aria-expanded=true (' + opened.menuExpanded + ')');
        check(opened.menuInsideRow, '菜单挂在对应的行里（可随之滚动）');

        // 点空白处收起
        await evaluate('document.querySelector(".view-head").click()');
        await new Promise(function (r) { setTimeout(r, 60); });
        const closed = await snap(980, 680, 'dark');
        check(!closed.menuOpen && closed.menuItems === 0, '点空白处菜单收起 (' + closed.menuItems + ')');
        check(closed.menuExpanded === 'false', '收起后 aria-expanded=false (' + closed.menuExpanded + ')');

        // Escape 收起
        await evaluate('document.querySelector(\'[data-act="row-more"]\').click()');
        await new Promise(function (r) { setTimeout(r, 60); });
        await evaluate('document.dispatchEvent(new KeyboardEvent("keydown", {key: "Escape", bubbles: true}))');
        await new Promise(function (r) { setTimeout(r, 60); });
        const esc = await snap(980, 680, 'dark');
        check(!esc.menuOpen, 'Escape 收起菜单 (' + esc.menuOpen + ')');

        // 菜单里的次要操作仍然可用（上移第一行在菜单里是禁用的）
        await evaluate('document.querySelector(\'[data-act="row-more"]\').click()');
        await new Promise(function (r) { setTimeout(r, 60); });
        const menuActs = await evaluate('JSON.stringify([].map.call(document.querySelectorAll(".menu .menu-item"),' +
            'function(b){return b.dataset.act + (b.dataset.delta || "") + (b.disabled ? ":off" : ":on");}))');
        check(menuActs.indexOf('move-server-1:off') >= 0 && menuActs.indexOf('move-server1:on') >= 0 &&
            menuActs.indexOf('export-server:on') >= 0,
            '菜单项复用既有动作且首行上移禁用 (' + menuActs + ')');

        // 4c2) 菜单的键盘可达性：打开后焦点进菜单，方向键在可用项间循环，
        //      Home/End 到首尾，Escape 收起并把焦点还给触发按钮。
        //      首行的"上移"是禁用的，所以可用项只有 2 个（下移、复制链接）。
        const activeAct = 'document.activeElement && document.activeElement.dataset ? ' +
            '(document.activeElement.dataset.act + (document.activeElement.dataset.delta || "")) : ""';
        const focus0 = await evaluate(activeAct);
        check(focus0 === 'move-server1',
            '打开菜单后焦点落在第一个可用项 (' + focus0 + ')');

        const arrow = 'document.dispatchEvent(new KeyboardEvent("keydown", {key: "%K", bubbles: true}));';
        await evaluate(arrow.replace('%K', 'ArrowDown'));
        await new Promise(function (r) { setTimeout(r, 30); });
        check(await evaluate(activeAct) === 'export-server', '↓ 移到下一项 (' + await evaluate(activeAct) + ')');
        await evaluate(arrow.replace('%K', 'ArrowDown'));
        await new Promise(function (r) { setTimeout(r, 30); });
        check(await evaluate(activeAct) === 'move-server1', '↓ 在末尾后循环回第一项 (' + await evaluate(activeAct) + ')');
        await evaluate(arrow.replace('%K', 'ArrowUp'));
        await new Promise(function (r) { setTimeout(r, 30); });
        check(await evaluate(activeAct) === 'export-server', '↑ 从第一项回到末尾 (' + await evaluate(activeAct) + ')');
        await evaluate(arrow.replace('%K', 'Home'));
        await new Promise(function (r) { setTimeout(r, 30); });
        check(await evaluate(activeAct) === 'move-server1', 'Home 回到第一项 (' + await evaluate(activeAct) + ')');
        await evaluate(arrow.replace('%K', 'End'));
        await new Promise(function (r) { setTimeout(r, 30); });
        check(await evaluate(activeAct) === 'export-server', 'End 到最后一个可用项 (' + await evaluate(activeAct) + ')');

        // 禁止项永远不该拿到焦点，方向键也不能停在它上面。
        check(await evaluate('document.activeElement.disabled === true') === false,
            '方向键不会把焦点停在禁用项上');
        await evaluate(arrow.replace('%K', 'Escape'));
        await new Promise(function (r) { setTimeout(r, 60); });
        const escFocus = await evaluate('JSON.stringify({' +
            'open: !!document.querySelector(".menu"),' +
            'act: document.activeElement && document.activeElement.dataset ? document.activeElement.dataset.act : "",' +
            'expanded: (document.querySelector(\'[data-act="row-more"]\') || {}).ariaExpanded || ""})');
        const escFocusObj = JSON.parse(escFocus);
        check(!escFocusObj.open && escFocusObj.act === 'row-more',
            'Escape 收起菜单并把焦点还给"更多"按钮 (' + escFocus + ')');
        check(escFocusObj.expanded === 'false', '键盘收起后 aria-expanded=false (' + escFocusObj.expanded + ')');

        // 4d) 诊断页：桩修好后表格才真的渲染，这里把它纳入检查
        await evaluate('window.__ui.selectView("diag")');
        await evaluate('document.querySelector(\'[data-act="diag-run"]\').click()');
        await new Promise(function (r) { setTimeout(r, 120); });
        const diag = await measure(980, 680, 'dark', 'diag');
        check(diag.tableRows >= 1, '诊断结果表格渲染出行 (' + diag.tableRows + ' 行)');
        check(diag.tableOverflow <= 0, '诊断表格无横向溢出 (' + diag.tableOverflow + ')');
        assertContrast(diag, 'diag/980/表格');
        const diagNarrow = await measure(700, 680, 'dark', 'diag');
        check(diagNarrow.tableRows >= 1 && diagNarrow.tableOverflow <= 0,
            '窄窗下诊断表格仍无横向溢出 (' + diagNarrow.tableOverflow + ')');
        assertContrast(diagNarrow, 'diag/700/表格');
        density.push('诊断表格 ' + diag.tableRows + ' 行，980/700 均无横向溢出');

        // 4e) PAC 页：显示规则来源的那一行标签不能和卡片标题同名
        const pac = await measure(980, 680, 'dark', 'pac');
        check(pac.pacSourceLabel === '规则来源',
            'PAC 页规则来源行改名为"规则来源"，与卡片标题"数据源"区分 (' + pac.pacSourceLabel + ')');

        // 4f) 命令行代理：命令文本必须与"已保存的监听端口"一致，且复制到剪贴板的
        //     必须是同一串文字（不能用界面文本拼）。
        await evaluate('window.__UI_PUSH({enabled: true})');
        const cli = await measure(980, 680, 'dark', 'modes');
        check(cli.cliRows.length === 2, '命令行代理给出 HTTP 与 SOCKS5 两条命令 (' + cli.cliRows.length + ')');
        const cliHTTP = cli.cliRows.filter(function (r) { return r.id === 'http'; })[0];
        const cliSocks = cli.cliRows.filter(function (r) { return r.id === 'socks'; })[0];
        const wantHTTP = 'export http_proxy=http://127.0.0.1:1087;export https_proxy=http://127.0.0.1:1087;';
        const wantSocks = 'export http_proxy=socks5://127.0.0.1:1086;export https_proxy=socks5://127.0.0.1:1086;';
        // 与 SSX-NG 的 "Copy HTTP Proxy Shell Export Command" 同形
        check(cliHTTP && cliHTTP.command === wantHTTP,
            'HTTP 命令与 SSX-NG 同形 (' + (cliHTTP && cliHTTP.command) + ')');
        check(cliSocks && cliSocks.command === wantSocks,
            'SOCKS5 命令用同一套 export 形式 (' + (cliSocks && cliSocks.command) + ')');
        check(cli.cliRows.every(function (r) { return r.hasButton && r.id === r.buttonId; }),
            '每条命令都有对应的复制按钮');
        check(cli.cliRows.every(function (r) { return r.selectable && r.clipped <= 0; }),
            '命令文本可选中且未被裁掉');
        check(!cli.cliNote, '代理开启且端口未改时不加多余提示 (' + cli.cliNote + ')');
        await evaluate('window.__UI_PUSH({enabled: false})');
        const cliOff = await measure(980, 680, 'dark', 'modes');
        check((cliOff.cliNote || '').indexOf('未开启') >= 0, '代理关闭时提示命令尚不可用 (' + cliOff.cliNote + ')');
        // 关掉 HTTP 代理后，只应剩下 SOCKS5 一条，并说明原因
        await evaluate('window.__UI_PUSH({enabled: true, settings: Object.assign({}, window.__UI_FIXTURE.state.settings, {httpEnabled: false})})');
        const cliNoHTTP = await measure(980, 680, 'dark', 'modes');
        check(cliNoHTTP.cliRows.length === 1 && cliNoHTTP.cliRows[0].id === 'socks',
            'HTTP 关闭后只给 SOCKS5 命令 (' + cliNoHTTP.cliRows.map(function (r) { return r.id; }).join(',') + ')');
        check(cliNoHTTP.cliHTTPOff, 'HTTP 关闭时说明如何启用');

        // 复制按钮写进剪贴板的，必须与界面显示的命令逐字一致
        await evaluate('window.__UI_PUSH()');
        await evaluate('window.__CLIPBOARD.text = ""; window.__CLIPBOARD.count = 0;');
        await evaluate('window.__ui.selectView("modes")');
        await evaluate('document.querySelector(\'[data-act="copy-cli-proxy"][data-id="socks"]\').click()');
        await new Promise(function (r) { setTimeout(r, 120); });
        const clip = await snap(980, 680, 'dark');
        check(clip.clipboard === wantSocks,
            '点"复制命令"写进剪贴板的正是那条命令 (' + clip.clipboard + ')');
        check(clip.clipboardCount === 1, '复制只写一次剪贴板 (' + clip.clipboardCount + ')');
        density.push('命令行代理 2 条命令，复制内容与显示逐字一致');

        // 4g) 日志：去重、水位线、滚动位置。这一组对应三个静默的回归：
        //     同一条日志被记两次（seq=0 时旧代码漏过）、实时日志先到时整段历史
        //     被丢弃、以及状态推送把正在往上翻的用户顶回最新一行。
        await evaluate('window.__ui.selectView("logs")');
        await new Promise(function (r) { setTimeout(r, 80); });
        const beforeLogs = await evaluate('window.__UI_LOG_STATE().count');
        check(beforeLogs > 0, '日志页渲染出日志行 (' + beforeLogs + ' 行)');

        // 同一个 seq 推两次只能出现一次
        await evaluate('window.__UI_PUSH_LOG({seq: 900001, message: "去重测试"})');
        await new Promise(function (r) { setTimeout(r, 40); });
        const onceLogs = await evaluate('window.__UI_LOG_STATE().count');
        await evaluate('window.__UI_PUSH_LOG({seq: 900001, message: "去重测试"})');
        await new Promise(function (r) { setTimeout(r, 40); });
        const twiceLogs = await evaluate('window.__UI_LOG_STATE().count');
        check(onceLogs === beforeLogs + 1 && twiceLogs === onceLogs,
            '相同 seq 的日志只记一次 (' + beforeLogs + ' -> ' + onceLogs + ' -> ' + twiceLogs + ')');

        // seq 更小的（迟到的重放）也必须被水位线挡住
        await evaluate('window.__UI_PUSH_LOG({seq: 5, message: "迟到的旧日志"})');
        await new Promise(function (r) { setTimeout(r, 40); });
        check(await evaluate('window.__UI_LOG_STATE().count') === twiceLogs,
            '迟到的低 seq 日志被水位线挡掉');

        // 后端现在按 ~100ms 成批推送（ssxng:logs）。批次内部的去重必须与逐条一致：
        // 一批里既有新的、重复的、也有迟到的旧行。
        await evaluate('window.__UI_PUSH_LOGS([' +
            '{seq: 920001, message: "批次新行 A"},' +
            '{seq: 920002, message: "批次新行 B"},' +
            '{seq: 900001, message: "批次里的重复行"},' +
            '{seq: 5, message: "批次里迟到的旧行"}])');
        await new Promise(function (r) { setTimeout(r, 60); });
        const afterBatch = await evaluate('window.__UI_LOG_STATE().count');
        check(afterBatch === twiceLogs + 2,
            '批量推送只在同一批里记下新的 2 行 (期望 ' + (twiceLogs + 2) + '，实际 ' + afterBatch + ')');
        // 批次里靠后的行不能覆盖靠后水位线：再来一批"序号更低"的整批必须全部被挡掉。
        await evaluate('window.__UI_PUSH_LOGS([{seq: 920000, message: "整批迟到"}])');
        await new Promise(function (r) { setTimeout(r, 40); });
        check(await evaluate('window.__UI_LOG_STATE().count') === afterBatch,
            '整批序号都低于水位线时一条都不记');

        // 状态页"最近连接"：proxy 日志成批到达时不能每来一条就重建一次 DOM，
        // 但也必须在 ~250ms 内落到最新内容。用 MutationObserver 数重建次数。
        await evaluate('window.__ui.selectView("status")');
        await new Promise(function (r) { setTimeout(r, 80); });
        await evaluate('(function(){var b=document.getElementById("recent-proxy");' +
            'window.__UI_RECENT_MUT=0;' +
            'if(window.__UI_RECENT_OBS){window.__UI_RECENT_OBS.disconnect();}' +
            'window.__UI_RECENT_OBS=new MutationObserver(function(ms){window.__UI_RECENT_MUT+=ms.length;});' +
            'window.__UI_RECENT_OBS.observe(b,{childList:true,subtree:true});' +
            'for(var i=0;i<40;i++){' +
            'window.__UI_PUSH_LOG({seq: 930000+i, scope: "proxy", message: "连接测试 " + i});}' +
            'return true;})()');
        // 节流窗口是 250ms：稍等一下只允许落到一次（最多两次时钟误差）。
        await new Promise(function (r) { setTimeout(r, 400); });
        const muts = await evaluate('(function(){if(window.__UI_RECENT_OBS){window.__UI_RECENT_OBS.disconnect();window.__UI_RECENT_OBS=null;}return window.__UI_RECENT_MUT;})()');
        check(muts > 0 && muts <= 2,
            '状态页"最近连接"按节流重建，40 条 proxy 日志只重建 ' + muts + ' 次（≤2）');
        const recentTop = await evaluate('(function(){var b=document.getElementById("recent-proxy");' +
            'var t=b&&b.querySelector(".row-sub");return t?t.textContent:"";})()');
        check(recentTop.indexOf('连接测试 39') >= 0,
            '"最近连接"最终显示最新一条 (' + recentTop + ')');

        // 视图滚动位置的记忆：切走再切回服务器页要回到原来的位置，而不是停在别处。
        // 目标值取"实际可滚动范围"内的一半，否则内容不够高时会被浏览器钳到最大值，
        // 断言就变成了在测量钳位而不是在测量记忆。
        await evaluate('window.__ui.selectView("servers")');
        await new Promise(function (r) { setTimeout(r, 80); });
        const wantScroll = await evaluate('(function(){var m=document.querySelector(".main");' +
            'var max=m.scrollHeight-m.clientHeight;var top=Math.floor(max/2);m.scrollTop=top;return [top,max];})()');
        const targetScroll = wantScroll[0];
        check(targetScroll > 0, '服务器页可以滚动出足够的高度 (最大 ' + wantScroll[1] + 'px)');
        await evaluate('window.__ui.selectView("logs")');
        await new Promise(function (r) { setTimeout(r, 80); });
        await evaluate('window.__ui.selectView("servers")');
        await new Promise(function (r) { setTimeout(r, 80); });
        const backScroll = await evaluate('document.querySelector(".main").scrollTop');
        check(backScroll === targetScroll,
            '切走再切回视图会恢复原来的滚动位置 (' + backScroll + '/' + targetScroll + ')');
        // 切到日志页仍然按既有语义直接置底（follow），不能被"恢复位置"覆盖。
        await evaluate('window.__ui.selectView("logs")');
        await new Promise(function (r) { setTimeout(r, 80); });
        const logFollow = await evaluate('(function(){var m=document.querySelector(".main");' +
            'return m.scrollHeight-m.clientHeight-m.scrollTop;})()');
        check(logFollow <= 2, '切到日志页仍然自动置底 (距底部 ' + logFollow + 'px)');
        await evaluate('(function(){document.querySelector(".main").scrollTop=0;return true;})()');

        // 往上翻之后，一次状态推送不能把滚动位置重置到底部。
        // 必须先塞满日志让列表真的可滚动：内容不足时 scrollTop 恒为 0，
        // 断言会在"没有回归"和"有回归"两种情况下都通过，等于白测。
        await evaluate('window.__ui.selectView("logs")');
        await new Promise(function (r) { setTimeout(r, 80); });
        await evaluate('(function(){for(var i=0;i<120;i++){' +
            'window.__UI_PUSH_LOG({seq: 910000+i, message: "滚动测试 " + i});}return true;})()');
        await new Promise(function (r) { setTimeout(r, 120); });
        const scrollable = await evaluate('(function(){var l=document.getElementById("log-list");' +
            'return l.scrollHeight - l.clientHeight;})()');
        check(scrollable > 40, '日志列表已塞满到可滚动（可滚动高度 ' + scrollable + 'px）');

        await evaluate('(function(){var l=document.getElementById("log-list");l.scrollTop=0;' +
            'l.dispatchEvent(new Event("scroll"));return l.scrollTop;})()');
        await evaluate('window.__UI_PUSH({enabled: true})');
        await new Promise(function (r) { setTimeout(r, 100); });
        const keptTop = await evaluate('window.__UI_LOG_STATE().scrolled');
        check(keptTop <= 1, '状态推送不会把翻到顶部的日志列表顶回底部 (scrollTop=' + keptTop + ')');
        density.push('日志去重按 seq 水位线，重渲染保留滚动位置');

        // 4g1b) 批次被丢弃后的补齐：一条日志只写进后端、不推事件（订阅缓冲满时
        //       后端就是这个行为）。切到日志页会带水位线调一次 GetLogs，
        //       这条必须补出来，且不能重复。
        const beforeCatchUp = await evaluate('window.__UI_LOG_STATE().count');
        await evaluate('window.__UI_BACKEND_LOG({seq: 940001, level: "error", scope: "pac", message: "被丢弃的批次里的行"})');
        await evaluate('window.__ui.selectView("status")');
        await new Promise(function (r) { setTimeout(r, 80); });
        const notYet = await evaluate('window.__UI_LOG_STATE().count');
        check(notYet === beforeCatchUp, '未推送的日志不会凭空出现在界面 (' + notYet + ')');
        await evaluate('window.__ui.selectView("logs")');
        await new Promise(function (r) { setTimeout(r, 150); });
        const caught = await evaluate('window.__UI_LOG_STATE().count');
        check(caught === beforeCatchUp + 1,
            '切到日志页按水位线补齐被丢弃的日志 (' + beforeCatchUp + ' -> ' + caught + ')');
        // 再切一次不能把同一行补第二遍。
        await evaluate('window.__ui.selectView("status")');
        await new Promise(function (r) { setTimeout(r, 60); });
        await evaluate('window.__ui.selectView("logs")');
        await new Promise(function (r) { setTimeout(r, 150); });
        check(await evaluate('window.__UI_LOG_STATE().count') === caught,
            '补齐按 seq 去重，反复切页不会重复计入');

        // 4g2) 复选框一次交互只处理一次。浏览器对复选框会同时派发 input 与 change；
        //      两个事件都绑到同一个处理器时，每次勾选都会跑两遍（下拉框那条路更明显，
        //      但下拉框在第一次处理时就会被整页重绘换掉，第二个事件落在已脱离文档的
        //      节点上、根本冒泡不到 document，所以真正能观察到重复的是复选框）。
        //      updateDraftField 会走 refreshSettingsFeedback，它每次都重写
        //      #settings-errors —— 一次勾选只能写一次。
        await evaluate('window.__ui.selectView("modes")');
        await new Promise(function (r) { setTimeout(r, 80); });
        const checkPasses = await evaluate('(function(){' +
            'var box=document.getElementById("settings-errors");' +
            'if(!box)return -1;' +
            'var d=Object.getOwnPropertyDescriptor(Element.prototype,"innerHTML");' +
            'window.__FEEDBACK=0;' +
            'Object.defineProperty(box,"innerHTML",{configurable:true,' +
            'get:function(){return d.get.call(this);},' +
            'set:function(v){window.__FEEDBACK++;return d.set.call(this,v);}});' +
            'var cb=document.querySelector(\'#body-modes [data-key="clearProxyOnQuit"]\');' +
            'if(!cb)return -2;' +
            'cb.checked=!cb.checked;' +
            'cb.dispatchEvent(new Event("input",{bubbles:true}));' +
            'cb.dispatchEvent(new Event("change",{bubbles:true}));' +
            'return window.__FEEDBACK;})()');
        check(checkPasses === 1, '复选框一次交互只处理一次（input+change 不重复，实际 ' + checkPasses + ' 次）');

        // 文本框同理：敲键（input）与失焦（change）各一次，但失焦那次不能重复处理。
        await evaluate('window.__ui.selectView("modes")');
        await new Promise(function (r) { setTimeout(r, 80); });
        const textPasses = await evaluate('(function(){' +
            'var box=document.getElementById("settings-errors");' +
            'if(!box)return -1;' +
            'var d=Object.getOwnPropertyDescriptor(Element.prototype,"innerHTML");' +
            'window.__FEEDBACK2=0;' +
            'Object.defineProperty(box,"innerHTML",{configurable:true,' +
            'get:function(){return d.get.call(this);},' +
            'set:function(v){window.__FEEDBACK2++;return d.set.call(this,v);}});' +
            'var inp=document.querySelector(\'#body-modes [data-key="socksPort"]\');' +
            'if(!inp)return -2;' +
            'inp.value="1088";' +
            'inp.dispatchEvent(new Event("input",{bubbles:true}));' +
            'inp.dispatchEvent(new Event("change",{bubbles:true}));' +
            'return window.__FEEDBACK2;})()');
        check(textPasses === 1 || textPasses === 2,
            '文本框的 input 与 change 不会叠加处理（实际 ' + textPasses + ' 次）');
        // 上面这条是宽松的（文本框本来就是 input 每次敲键都处理、change 只该在
        // 值真的变了时补一次），真正钉死重复的是复选框那条；这里只保证不会失控。

        // 4g2b) UDP 开关必须真的在设置页里、并且真的进了保存载荷。
        //   只断言"有个 data-key=udpEnabled 的复选框"是不够的：一个画出来但没接线的
        //   开关，勾了也不会进 SaveSettings 的参数，用户看到勾选却在重启后失效。
        await evaluate('window.__ui.selectView("modes")');
        await new Promise(function (r) { setTimeout(r, 80); });
        const udpBox = await evaluate('(function(){' +
            'var cb=document.querySelector(\'#body-modes [data-key="udpEnabled"]\');' +
            'if(!cb)return null;' +
            'return {type: cb.type, checked: cb.checked, inGrid: !!cb.closest(".form-grid"),' +
            'stacked: !!cb.closest(".check-stack")};})()');
        check(udpBox !== null, '设置页有"启用 UDP 转发"复选框');
        check(udpBox && udpBox.type === 'checkbox', 'UDP 开关是复选框 (type=' + (udpBox && udpBox.type) + ')');
        check(udpBox && udpBox.checked === true, 'UDP 开关默认勾选（与 config 默认值一致）');
        check(udpBox && udpBox.inGrid && udpBox.stacked,
            'UDP 开关在监听网格里且与"跟随全局"叠在同一格（不为它多占一整行）');

        // 勾掉 -> 保存，载荷里必须是 false。stub 把入参记在 __UI_LAST_ARGS 里。
        await evaluate('window.__UI_RESET_CALLS()');
        await evaluate('(function(){' +
            'var cb=document.querySelector(\'#body-modes [data-key="udpEnabled"]\');' +
            'cb.checked=false;' +
            'cb.dispatchEvent(new Event("change",{bubbles:true}));return true;})()');
        await new Promise(function (r) { setTimeout(r, 60); });
        await evaluate('document.querySelector(\'[data-act="settings-save"]\').click()');
        await new Promise(function (r) { setTimeout(r, 120); });
        const savedArgs = await evaluate('window.__UI_LAST_ARGS("SaveSettings")');
        let savedUDP = null;
        try {
            const parsed = JSON.parse(savedArgs);
            savedUDP = parsed && parsed[0] ? parsed[0].udpEnabled : null;
        } catch (e) { savedUDP = 'PARSE_FAIL'; }
        check(savedUDP === false,
            '取消勾选后 SaveSettings 载荷里的 udpEnabled 为 false (实际 ' + JSON.stringify(savedUDP) + ')');
        check(await evaluate('window.__UI_CALL_COUNT("SaveSettings")') === 1,
            '保存只发一次 SaveSettings (' + (await evaluate('window.__UI_CALL_COUNT("SaveSettings")')) + ' 次)');

        // 状态页：UDP 开启时 SOCKS 那一项后面要带上 UDP 标记，否则用户看不出它在跑。
        await evaluate('window.__UI_PUSH({enabled: true})');
        await evaluate('window.__ui.selectView("status")');
        await new Promise(function (r) { setTimeout(r, 100); });
        const socksFact = await evaluate('(function(){' +
            'var f=document.querySelector("#body-status .facts .fact");' +
            'return f ? f.textContent : "";})()');
        check(socksFact.indexOf("UDP") >= 0,
            '状态页在 UDP 开启时标出 UDP（' + JSON.stringify(socksFact) + '）');

        // 还原：把测试期间挂在元素上的 innerHTML 覆盖去掉，并恢复默认日志等级，
        // 后面的截图才不会被测试改过的状态污染。
        await evaluate('(function(){var b=document.getElementById("body-logs");' +
            'if(b)delete b.innerHTML;return true;})()');
        await evaluate('(function(){var e=document.getElementById("settings-errors");' +
            'if(e)delete e.innerHTML;return true;})()');

        // 4g3) 离线页的"重试连接"会重新跑 boot()，而 boot() 在无后端时会提前返回
        //      ——watchAppearance() 正好在提前返回之前。所以重试必须幂等，否则每次
        //      点重试都往 matchMedia 上再挂一个永不摘除的监听。页面加载时已经注册过
        //      一个，这里把真实的 go 换成桩制造离线状态，数"额外注册"了几次。
        //      点击是经 onClick 的微任务派发的，必须等一拍再读计数。
        const mediaSetup = await evaluate('(function(){' +
            'window.__REAL_GO=window.go;' +
            'window.__REAL_MM=window.matchMedia;' +
            'window.__MEDIA=0;' +
            'window.matchMedia=function(q){var mq=window.__REAL_MM.call(window,q);' +
            'var add=mq.addEventListener.bind(mq);' +
            'mq.addEventListener=function(n,f){if(n==="change")window.__MEDIA++;return add(n,f);};' +
            'return mq;};' +
            'delete window.go;' +
            'var btn=document.querySelector(\'[data-act="retry-backend"]\');' +
            'if(!btn)return false;' +
            'btn.click();btn.click();btn.click();' +
            'return true;})()');
        await new Promise(function (r) { setTimeout(r, 250); });
        const mediaWatches = await evaluate('(function(){' +
            'var n=window.__MEDIA;' +
            'window.go=window.__REAL_GO;' +
            'window.matchMedia=window.__REAL_MM;' +
            'return n;})()');
        check(mediaSetup === true && mediaWatches >= 0 && mediaWatches <= 1,
            '离线页连点三次"重试连接"不会重复注册 theme 监听（3 次点击 -> ' + mediaWatches + ' 次注册）');
        // 有修复时这里是 0（加载时已注册过，重试不再补）；没有修复时是 3。
        // 离线状态可能已经把界面切走了，恢复成有后端的正常状态
        await evaluate('window.go && window.__ui.selectView("status")');
        await new Promise(function (r) { setTimeout(r, 80); });
        // 塞进去的 120 条要清掉，后面的截图才不会被这批测试日志污染
        await evaluate('window.__ui.selectView("logs")');
        await evaluate('document.querySelector(\'[data-act="logs-clear"]\').click()');
        await new Promise(function (r) { setTimeout(r, 80); });

        // 4h) 复制失败时不能留下离屏 <textarea>：execCommand 抛异常是最容易触发
        //     这条路径的方式（旧代码在 catch 里直接 return，节点就留在页面上了）。
        await evaluate('window.__ui.selectView("modes")');
        await new Promise(function (r) { setTimeout(r, 60); });
        const leak = await evaluate('(function(){' +
            'var origRT=window.runtime.ClipboardSetText;' +
            'window.runtime.ClipboardSetText=function(){return Promise.reject(new Error("boom"));};' +
            'var origExec=document.execCommand;' +
            'document.execCommand=function(){throw new Error("boom");};' +
            'Object.defineProperty(navigator,"clipboard",{configurable:true,' +
            'value:{writeText:function(){return Promise.reject(new Error("boom"));}}});' +
            'document.querySelector(\'[data-act="copy-cli-proxy"][data-id="socks"]\').click();' +
            'return new Promise(function(res){setTimeout(function(){' +
            'var n=document.querySelectorAll("textarea.copy-buffer").length;' +
            'window.runtime.ClipboardSetText=origRT;' +
            'document.execCommand=origExec;' +
            'res(n);},150);});})()');
        check(leak === 0, '三条复制路径全部失败后没有残留的离屏 textarea (' + leak + ' 个)');

        // 4i) 连点只发一次 RPC。删除是最危险的那个（旧代码还会重复弹确认框），
        //     上移与选中同样绕过过 runAction 的 pending 门。
        const dupCalls = await evaluate('(function(){' +
            'window.__UI_RESET_CALLS();' +
            'window.__CONFIRMS=0;' +
            'window.confirm=function(){window.__CONFIRMS++;return true;};' +
            'window.__ui.selectView("servers");' +
            'return true;})()');
        check(dupCalls === true, '测试前切到服务器页');
        await new Promise(function (r) { setTimeout(r, 80); });
        await evaluate('(function(){var b=document.querySelector(\'[data-act="delete-server"]\');' +
            'b.click();b.click();b.click();return true;})()');
        await new Promise(function (r) { setTimeout(r, 200); });
        const delCalls = await evaluate('window.__UI_CALL_COUNT("DeleteServer")');
        const confirms = await evaluate('window.__CONFIRMS');
        check(delCalls === 1, '连点三次删除只发一次 DeleteServer (' + delCalls + ' 次)');
        check(confirms === 1, '连点三次删除只弹一次确认框 (' + confirms + ' 次)');

        await evaluate('window.__UI_RESET_CALLS()');
        await evaluate('(function(){var b=document.querySelector(\'[data-act="select-server"]\');' +
            'if(b){b.click();b.click();}return !!b;})()');
        await new Promise(function (r) { setTimeout(r, 200); });
        const selCalls = await evaluate('window.__UI_CALL_COUNT("SelectServer")');
        check(selCalls <= 1, '连点选中节点不会重复发 SelectServer (' + selCalls + ' 次)');
        density.push('删除/选中在途只发一次 RPC，确认框也只弹一次');

        // 还原 fixture，后面的截图用原始状态
        await evaluate('window.__UI_PUSH()');
        await evaluate('window.__ui.selectView("status")');

        // 窄窗（侧栏折叠）下两栏 kv 必须回退单列，否则文字会被挤
        const narrow = await measure(700, 680, 'dark', 'status');
        check(narrow.kvGridCols === 0 || narrow.kvGridCols === 1,
            '窄窗下特权助手卡的 kv 回退单列 (' + narrow.kvGridCols + ')');
        check(status.radioCount === 0, '状态页不再堆模式单选，模式改由状态条 + "切换模式"入口 (' + status.radioCount + ')');

        // 关闭 + 没有节点：主按钮应变成引导动作，状态文案跟着变
        await evaluate('window.__UI_PUSH({enabled: false, server: null, lastError: ""})');
        const idle = await measure(980, 680, 'dark', 'status');
        check(idle.heroState.indexOf('代理已停止') >= 0, '关闭时状态条显示"代理已停止" (' + idle.heroState + ')');
        check((idle.heroSub || '').indexOf('尚未选择服务器') >= 0, '没有节点时状态条直接说明 (' + idle.heroSub + ')');
        check(idle.heroPrimary === 'goto-servers', '没有节点时主按钮是"添加服务器" (' + idle.heroPrimary + ')');

        // 代理报错时状态条要变成异常态
        await evaluate('window.__UI_PUSH({enabled: true, lastError: "本地端口 1086 被占用"})');
        const bad = await measure(980, 680, 'dark', 'status');
        check(bad.heroState.indexOf('代理异常') >= 0, '出错时状态条显示"代理异常" (' + bad.heroState + ')');
        await evaluate('window.__UI_PUSH()');

        // 模式单选搬到了"模式与代理"页，且选中态只在当前视图里统计
        const modes = await measure(980, 680, 'dark', 'modes');
        check(modes.radioCount === 5, '"模式与代理"页有 5 个模式单选 (' + modes.radioCount + ')');
        check(modes.modeSelected === 'global', '"模式与代理"页的当前模式整行高亮 (' + modes.modeSelected + ')');

        // 状态事件重新渲染后，状态条要跟着更新
        await evaluate('window.__UI_PUSH({mode: "manual"})');
        const manual = await measure(980, 680, 'dark', 'status');
        check((manual.heroSub || '').indexOf('手动模式') >= 0, '状态推送后状态条随之更新 (' + manual.heroSub + ')');
        await evaluate('window.__UI_PUSH()');
        const restored = await measure(980, 680, 'dark', 'status');
        check(restored.heroState.indexOf('代理运行中') >= 0, '还原 fixture 后状态条恢复 (' + restored.heroState + ')');

        // 4) 截图存档（人工复核用）
        const shotsDir = process.env.SSXNG_UI_SHOTS || path.join(dir, 'shots');
        fs.mkdirSync(shotsDir, {recursive: true});
        for (const theme of THEMES) {
            await s('Emulation.setDeviceMetricsOverride', {
                width: 980, height: 680, deviceScaleFactor: 2, mobile: false
            });
            await s('Emulation.setEmulatedMedia', {
                media: 'screen', features: [{name: 'prefers-color-scheme', value: theme}]
            });
            await evaluate('window.__ui.selectView("status")');
            await new Promise(function (r) { setTimeout(r, 80); });
            const shot = await s('Page.captureScreenshot', {format: 'png'});
            const file = path.join(shotsDir, 'status-' + theme + '.png');
            fs.writeFileSync(file, Buffer.from(shot.data, 'base64'));
            passes.push('截图: ' + file);
        }
        // 模式与代理页（本轮瘦身重点，留两张给人复核）
        for (const theme of THEMES) {
            await s('Emulation.setDeviceMetricsOverride', {
                width: 980, height: 680, deviceScaleFactor: 2, mobile: false
            });
            await s('Emulation.setEmulatedMedia', {
                media: 'screen', features: [{name: 'prefers-color-scheme', value: theme}]
            });
            await evaluate('window.__ui.selectView("modes")');
            await new Promise(function (r) { setTimeout(r, 80); });
            const shot = await s('Page.captureScreenshot', {format: 'png'});
            const file = path.join(shotsDir, 'modes-' + theme + '.png');
            fs.writeFileSync(file, Buffer.from(shot.data, 'base64'));
            passes.push('截图: ' + file);
        }

        // 服务器页：8 个节点 + 打开"更多"菜单（本轮重点，留给人复核）
        await s('Emulation.setDeviceMetricsOverride', {
            width: 980, height: 680, deviceScaleFactor: 2, mobile: false
        });
        await s('Emulation.setEmulatedMedia', {
            media: 'screen', features: [{name: 'prefers-color-scheme', value: 'dark'}]
        });
        await evaluate('window.__UI_FILL_SERVERS(8)');
        await evaluate('window.__ui.selectView("servers")');
        await evaluate('document.querySelector(\'[data-act="row-more"]\').click()');
        await new Promise(function (r) { setTimeout(r, 90); });
        const rowShot = await s('Page.captureScreenshot', {format: 'png'});
        const rowFile = path.join(shotsDir, 'servers-many-dark.png');
        fs.writeFileSync(rowFile, Buffer.from(rowShot.data, 'base64'));
        passes.push('截图: ' + rowFile);
        const shotTheme = await evaluate('window.matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light"');
        check(shotTheme === 'dark', '截图前主题已显式设为深色 (' + shotTheme + ')');
        await evaluate('document.dispatchEvent(new KeyboardEvent("keydown", {key: "Escape", bubbles: true}))');

        // 诊断页：修好桩以后才真的渲染出表格
        await evaluate('window.__UI_PUSH()');
        await evaluate('window.__ui.selectView("diag")');
        await evaluate('document.querySelector(\'[data-act="diag-run"]\').click()');
        await new Promise(function (r) { setTimeout(r, 150); });
        const diagShot = await s('Page.captureScreenshot', {format: 'png'});
        const diagFile = path.join(shotsDir, 'diag-table-dark.png');
        fs.writeFileSync(diagFile, Buffer.from(diagShot.data, 'base64'));
        passes.push('截图: ' + diagFile);

        // 关闭 + 无节点的引导态
        await s('Emulation.setEmulatedMedia', {
            media: 'screen', features: [{name: 'prefers-color-scheme', value: 'dark'}]
        });
        // 注意：上一段停在"模式与代理"页，截图前必须切回状态页
        await evaluate('window.__ui.selectView("status")');
        await evaluate('window.__UI_PUSH({enabled: false, server: null, lastError: ""})');
        await new Promise(function (r) { setTimeout(r, 80); });
        const idleView = await evaluate('window.__ui.metrics().view');
        check(idleView === 'status', '引导态截图拍的是状态页 (' + idleView + ')');
        const idleShot = await s('Page.captureScreenshot', {format: 'png'});
        const idleFile = path.join(shotsDir, 'status-idle-dark.png');
        fs.writeFileSync(idleFile, Buffer.from(idleShot.data, 'base64'));
        passes.push('截图: ' + idleFile);
        await evaluate('window.__UI_PUSH()');
    } catch (err) {
        // 让断言失败变成一条可读的失败记录，而不是把栈顶的行号丢给用户
        check(false, '检查过程异常中断: ' + (err && err.message ? err.message : String(err)));
    } finally {
        client.close();
        try { run.proc.kill('SIGKILL'); } catch (e) { /* 忽略 */ }
    }
    if (pageErrors.length) {
        check(false, '未捕获异常汇总 (' + pageErrors.length + ' 条): ' +
            pageErrors.slice(0, 5).join(' | '));
    }

    console.log('通过 ' + passes.length + ' 项检查');
    density.forEach(function (d) { console.log('  · ' + d); });
    if (process.env.SSXNG_UI_VERBOSE) passes.forEach(function (p) { console.log('  ✓ ' + p); });
    if (failures.length) {
        console.error('\n失败 ' + failures.length + ' 项:');
        failures.forEach(function (f) { console.error('  ✗ ' + f); });
        exitCode = 1;
    } else {
        console.log('全部通过：标题栏/拖动区、无溢出、文字对比度、视图渲染。');
    }
    process.exit(exitCode);
}

main().catch(function (err) {
    console.error(err && err.stack ? err.stack : String(err));
    process.exit(1);
});
