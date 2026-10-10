package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"ssxng/internal/api"
	"ssxng/internal/config"
	"ssxng/internal/pac"
)

// PAC: generating proxy.pac, the local PAC server, downloading the
// gfwlist, and opening the rule files.

func (a *App) pacView(sett config.Settings) api.PACView {
	view := api.PACView{
		Address:  sett.PAC.Address,
		Port:     sett.PAC.Port,
		Active:   a.pacSrv != nil && a.pacSrv.Running(),
		External: sett.PAC.ExternalPACURL,
	}
	if a.pacSrv != nil {
		view.URL = a.pacSrv.URL()
		view.LastError = a.pacSrv.LastError()
	}
	// buildState runs on every state push (mode switch, settings save, server
	// selection), so this path must not read the PAC file. A stat is enough to
	// tell the user whether there is a usable file, and the rule count comes
	// from the cache, which only re-parses when the file actually changed.
	path := a.store.PACFilePath()
	info, err := os.Stat(path)
	switch {
	case err != nil:
		if view.LastError == "" {
			view.LastError = "尚未生成 PAC 文件，请先在 PAC 页点击“从 GFWList 更新”"
		}
		return view
	case info.Size() == 0:
		if view.LastError == "" {
			view.LastError = "PAC 文件为空，请重新生成"
		}
		return view
	}
	view.RuleCount = a.pacRules.Count(path)
	view.Generated = info.ModTime().Unix()
	view.Source = a.pacSource(sett)
	return view
}

// pacSource is the human readable "数据源" for the PAC tab.
func (a *App) pacSource(sett config.Settings) string {
	// One stat, not a stat plus a full read of the downloaded list: this runs on
	// every state push and only needs to know whether the cache is there.
	if info, err := os.Stat(a.store.GFWListPath()); err == nil && info.Size() > 0 {
		return fmt.Sprintf("%s（%s 更新）", hostOfURL(sett.PAC.GFWListURL), info.ModTime().Format("2006-01-02 15:04"))
	}
	return "本地规则"
}

func hostOfURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "GFWList"
	}
	if i := strings.Index(raw, "://"); i >= 0 {
		raw = raw[i+3:]
	}
	if i := strings.IndexAny(raw, "/?#"); i >= 0 {
		raw = raw[:i]
	}
	return raw
}

// syncPACServer starts or stops the local PAC server depending on the mode.
func (a *App) syncPACServer() {
	if a.pacSrv == nil {
		return
	}
	sett := a.settings()
	need := sett.Enabled && sett.Mode == config.ModeAuto
	if need {
		if !a.pacSrv.Running() {
			if err := a.pacSrv.Start(listenAddr(sett.PAC.Address, sett.PAC.Port)); err != nil {
				a.toastError("pac", err)
				return
			}
			a.logf("info", "pac", "PAC 服务器已启动: %s", a.pacSrv.URL())
		}
		a.ensurePACFile()
		return
	}
	if a.pacSrv.Running() {
		a.pacSrv.Stop()
		a.logf("info", "pac", "PAC 服务器已停止")
	}
}

// readPACFile is the PAC server's data source: it re-reads the file for every
// request so an update is picked up immediately.
func (a *App) readPACFile() ([]byte, error) {
	if a.store == nil {
		return nil, os.ErrNotExist
	}
	data, err := os.ReadFile(a.store.PACFilePath())
	if err != nil {
		return nil, errors.New("尚未生成 PAC 文件，请先在 PAC 页点击“从 GFWList 更新”")
	}
	if len(data) == 0 {
		return nil, errors.New("PAC 文件为空，请重新生成")
	}
	return data, nil
}

// ensurePACFile generates the PAC file from the local gfwlist when it is
// missing, and downloads the gfwlist the first time.
func (a *App) ensurePACFile() {
	sett := a.settings()
	if _, err := os.Stat(a.store.PACFilePath()); err == nil {
		return
	}
	if _, err := os.Stat(a.store.GFWListPath()); err == nil {
		if res, err := a.generatePAC(sett); err != nil {
			a.logf("warn", "pac", "生成 PAC 文件失败: %v", err)
		} else {
			a.logf("info", "pac", "已生成 PAC 文件，共 %d 条规则", res.RuleCount)
		}
		return
	}
	// First use: fetch the list once, in the background, then generate.
	a.pacFetchOnce.Do(func() {
		go func() {
			a.logf("info", "pac", "首次使用，正在下载 GFWList…")
			if _, err := a.UpdatePACFromGFWList(); err != nil {
				return
			}
			if err := a.applier.reconcile("PAC 就绪"); err != nil {
				a.logf("warn", "proxy", "PAC 就绪后应用失败: %v", err)
			}
			a.emitState()
		}()
	})
}

// generatePAC writes gfwlist.js from gfwlist.txt plus user-rule.txt.
func (a *App) generatePAC(sett config.Settings) (pac.Result, error) {
	var res pac.Result
	gfw, err := os.ReadFile(a.store.GFWListPath())
	if err != nil {
		return res, fmt.Errorf("读取 gfwlist 失败: %w", err)
	}
	user, err := os.ReadFile(a.store.UserRulePath())
	if err != nil {
		if !os.IsNotExist(err) {
			return res, fmt.Errorf("读取 user-rule.txt 失败: %w", err)
		}
		user = []byte(defaultUserRule)
		if werr := writeFileAtomic(a.store.UserRulePath(), user, 0o600); werr != nil {
			a.logf("warn", "pac", "写入默认 user-rule.txt 失败: %v", werr)
		}
	}
	res, err = pac.Generate(pac.Options{
		SocksHost: sett.SOCKS.Address,
		SocksPort: sett.SOCKS.Port,
		GFWList:   gfw,
		UserRules: user,
	})
	if err != nil {
		return res, err
	}
	if err := writeFileAtomic(a.store.PACFilePath(), res.Content, 0o600); err != nil {
		return res, fmt.Errorf("写入 PAC 文件失败: %w", err)
	}
	// The rule count just changed; do not make the UI wait for a differing
	// mtime/size to notice.
	a.pacRules.Invalidate()
	return res, nil
}

const defaultUserRule = `! 用户自定义规则
! 语法与 AdBlock 相同，例如：
! ||example.com
! @@||example.com
`

// fetchGFWList downloads the rule list, trying the configured URL first and then
// a few fallbacks.
//
// raw.githubusercontent.com is frequently unreachable from mainland networks,
// and the app's own HTTP client does not use the system proxy, so a working
// Shadowsocks tunnel is the most reliable route. It also means the very first
// update (before any proxy is up) still has a chance through a mirror.
func (a *App) fetchGFWList(sett config.Settings) ([]byte, string, error) {
	primary := strings.TrimSpace(sett.PAC.GFWListURL)
	if primary == "" {
		primary = config.DefaultGFWListURL
	}

	type attempt struct {
		url   string
		proxy string
	}
	var attempts []attempt
	seen := map[string]bool{}
	push := func(url, proxy string) {
		if url == "" {
			return
		}
		key := url + "|" + proxy
		if seen[key] {
			return
		}
		seen[key] = true
		attempts = append(attempts, attempt{url: url, proxy: proxy})
	}
	// The local HTTP proxy is only useful while the tunnel is up.
	localProxy := ""
	if a.engine != nil && a.engine.Running() {
		if _, httpAddr := a.engine.Addresses(); httpAddr != "" {
			localProxy = "http://" + httpAddr
		}
	}
	push(primary, "")
	if primary != config.DefaultGFWListMirror {
		push(config.DefaultGFWListMirror, "")
	}
	if localProxy != "" {
		push(primary, localProxy)
		if primary != config.DefaultGFWListMirror {
			push(config.DefaultGFWListMirror, localProxy)
		}
	}

	var lastErr error
	for _, at := range attempts {
		if at.proxy == "" {
			a.logf("info", "pac", "正在下载规则: %s", at.url)
		} else {
			a.logf("info", "pac", "正在通过本地代理下载规则: %s", at.url)
		}
		data, err := downloadFile(at.url, at.proxy, 16<<20)
		if err != nil {
			lastErr = err
			a.logf("warn", "pac", "下载失败（%s）: %v", at.url, err)
			continue
		}
		if _, derr := pac.DecodeGFWList(data); derr != nil {
			lastErr = fmt.Errorf("下载到的规则无法解析: %w", derr)
			a.logf("warn", "pac", "下载到的内容不是有效的规则文件（%s）: %v", at.url, derr)
			continue
		}
		return data, at.url, nil
	}
	if lastErr == nil {
		lastErr = errors.New("没有可用的下载地址")
	}
	return nil, "", fmt.Errorf("下载规则失败，已尝试 %d 个地址。最后错误: %w", len(attempts), lastErr)
}

// UpdatePACFromGFWList downloads gfwlist and regenerates the PAC file.
func (a *App) UpdatePACFromGFWList() (api.PACView, error) {
	sett := a.settings()
	data, source, err := a.fetchGFWList(sett)
	if err != nil {
		a.toastError("pac", err)
		return a.pacView(sett), err
	}
	if source != strings.TrimSpace(sett.PAC.GFWListURL) {
		a.logf("info", "pac", "规则来自 %s", source)
	}
	if source != sett.PAC.GFWListURL {
		// Remember where it actually came from, so the next update starts there.
		sett.PAC.GFWListURL = source
		if serr := a.store.SaveSettings(sett); serr != nil {
			a.logf("warn", "pac", "保存规则地址失败: %v", serr)
		}
	}
	// Validate before overwriting a working list.
	if _, derr := pac.DecodeGFWList(data); derr != nil {
		err := fmt.Errorf("下载到的规则无法解析: %w", derr)
		a.toastError("pac", err)
		return a.pacView(sett), err
	}
	if err := writeFileAtomic(a.store.GFWListPath(), data, 0o600); err != nil {
		a.toastError("pac", err)
		return a.pacView(sett), err
	}
	res, err := a.generatePAC(sett)
	if err != nil {
		a.toastError("pac", err)
		return a.pacView(sett), err
	}
	a.logf("info", "pac", "PAC 已更新，共 %d 条规则（其中用户规则 %d 条）", res.RuleCount, res.UserCount)
	a.emitToast("info", fmt.Sprintf("PAC 已更新，共 %d 条规则", res.RuleCount))

	if sett.Enabled && sett.Mode == config.ModeAuto {
		if err := a.applier.reconcile("PAC 更新"); err != nil {
			a.logf("warn", "proxy", "PAC 更新后应用失败: %v", err)
		}
	}
	view := a.pacView(sett)
	a.emitState()
	return view, nil
}

// PACStatus returns the PAC state without touching the network.
func (a *App) PACStatus() api.PACView { return a.pacView(a.settings()) }

// OpenUserRuleFile reveals user-rule.txt in Finder.
func (a *App) OpenUserRuleFile() error {
	if _, err := os.Stat(a.store.UserRulePath()); os.IsNotExist(err) {
		if werr := writeFileAtomic(a.store.UserRulePath(), []byte(defaultUserRule), 0o600); werr != nil {
			return werr
		}
	}
	return revealInFinder(a.store.UserRulePath())
}

// OpenConfigFolder opens the configuration folder in Finder.
func (a *App) OpenConfigFolder() error { return revealInFinder(a.store.Dir()) }
