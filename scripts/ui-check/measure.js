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
if (!dir || !chromePath) {
    console.error('用法: measure.js <harness 目录> <chrome 路径>');
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

// ---------------------------------------------------------------- CDP 客户端
function rpc(url) {
    return new Promise(function (resolve, reject) {
        const ws = new WebSocket(url);
        let nextId = 1;
        const pending = new Map();
        const eventWaiters = [];
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

    const run = await launch(chromePath, path.join(dir, 'profile'));
    const client = await rpc(run.wsURL);
    let exitCode = 0;
    try {
        const {targetId} = await client.send('Target.createTarget', {url: 'about:blank'});
        const {sessionId} = await client.send('Target.attachToTarget', {targetId: targetId, flatten: true});
        const s = function (method, params) { return client.send(method, params, sessionId); };

        await s('Page.enable');
        await s('Runtime.enable');
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
        // 门槛从 1275 提到 1450：本轮在"本地监听"卡里新增了"命令行代理"块（约 160px）。
        // 这是新增内容，不是排版退化，所以门槛跟着走；但新增块自身单独设上界，
        // 免得以后悄悄膨胀还被这条总高度断言吸收掉。
        check(modesPage.mainScroll.scroll <= 1450,
            '模式与代理页内容高 ≤ 1450px（基线 1498；含新增"命令行代理"块，实测 ' + modesPage.mainScroll.scroll + '）');
        check(modesPage.cliBlockHeight > 0 && modesPage.cliBlockHeight <= 200,
            '"命令行代理"块高度受控 ≤ 200px（实测 ' + modesPage.cliBlockHeight + 'px）');
        const cols = modesPage.blocks.filter(function (b) { return b.cls === 'card'; });
        check(cols.length >= 2 && cols.every(function (c) {
            return c.left === cols[0].left && c.width === cols[0].width;
        }), '模式与代理页保持单列，没有改成分栏 (' + cols.length + ' 张卡)');
        check(modesPage.blocks.every(function (b) { return b.top >= 0; }), '模式与代理页没有元素被挤出视口上方');
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
        await evaluate('document.dispatchEvent(new KeyboardEvent("keydown", {key: "Escape", bubbles: true}))');
        await new Promise(function (r) { setTimeout(r, 60); });

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

        // 还原 fixture，后面的截图用原始状态
        await evaluate('window.__UI_PUSH()');

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
    } finally {
        client.close();
        try { run.proc.kill('SIGKILL'); } catch (e) { /* 忽略 */ }
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
