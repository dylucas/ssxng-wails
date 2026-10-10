package main

import (
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"ssxng/internal/config"
	"ssxng/internal/pac"
	"ssxng/internal/proxyconf"
)

// callHelper and helperStatus are the only two ways the app talks to the
// privileged helper. They are variables so tests can drive the applier without
// installing anything.
var (
	callHelper   = proxyconf.Call
	helperStatus = proxyconf.Status
)

// proxyApplier installs and clears the macOS system proxy through the
// privileged helper. All writes go through the helper: the app itself never
// needs root.
type proxyApplier struct {
	app *App

	mu        sync.Mutex
	last      appliedProxy
	hasLast   bool
	lastErr   string
	attemptAt time.Time
}

// markAttempt records that a reconcile was tried, so the watchdog can back off
// when the helper keeps failing.
func (p *proxyApplier) markAttempt() {
	p.mu.Lock()
	p.attemptAt = time.Now()
	p.mu.Unlock()
}

func (p *proxyApplier) lastAttempt() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.attemptAt
}

// appliedProxy records what we installed, so that the watchdog can tell whether
// something else (another client, or a macOS update) has since changed it.
type appliedProxy struct {
	Mode     string
	Services []string
	PACURL   string
	Socks    string
	HTTP     string
}

func (p *proxyApplier) lastApplied() (appliedProxy, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.last, p.hasLast
}

func (p *proxyApplier) lastApplyError() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastErr
}

func (p *proxyApplier) record(a appliedProxy) {
	p.mu.Lock()
	p.last = a
	p.hasLast = true
	p.lastErr = ""
	p.mu.Unlock()
}

// desired works out what the system proxy should look like right now.
//
// The returned mode is one of the helper's modes: "auto", "global" or "off".
// "manual" means "leave the system proxy alone", which is expressed as "off"
// *without* clearing anything we did not install.
func (a *App) desiredProxy() (mode, pacURL string, services []string, err error) {
	sett := a.store.Settings()
	if !sett.Enabled {
		return "off", "", a.currentServiceList(sett), nil
	}
	all := a.currentServiceList(sett)
	switch sett.Mode {
	case config.ModeGlobal:
		return "global", "", all, nil
	case config.ModeAuto:
		u, uerr := a.pacURLFor()
		if uerr != nil {
			return "", "", nil, uerr
		}
		return "auto", u, all, nil
	case config.ModeExternalPAC:
		u := strings.TrimSpace(sett.PAC.ExternalPACURL)
		if u == "" {
			return "", "", nil, fmt.Errorf("外部 PAC 模式需要填写 PAC 地址")
		}
		// networksetup would store whatever it is given, so check the scheme
		// here rather than letting a typo become a system setting.
		if err := pac.ValidateURL(u); err != nil {
			return "", "", nil, err
		}
		return "auto", u, all, nil
	case config.ModeManual:
		return "off", "", all, nil
	case config.ModeOff:
		return "off", "", a.servicesToClean(sett), nil
	default:
		return "", "", nil, fmt.Errorf("未知模式 %q", sett.Mode)
	}
}

// pacURLFor builds the local PAC URL including a cache buster, because macOS
// caches PAC files very aggressively.
func (a *App) pacURLFor() (string, error) {
	if a.pacSrv == nil || !a.pacSrv.Running() {
		return "", fmt.Errorf("PAC 服务器未运行")
	}
	return fmt.Sprintf("%s?v=%d", a.pacSrv.URL(), time.Now().Unix()), nil
}

// pacMatchCandidates lists, in order of confidence, the PAC URLs that the
// installed system settings might be pointing at *because of us*.
//
// This exists because the helper refuses to touch a service whose settings do
// not look like ours (see proxyconf.Matches), and in PAC mode the PAC URL is the
// only thing that identifies us — the SOCKS proxy is switched off, so it cannot
// serve as the marker. At clear time neither "enabled" is true nor is the PAC
// server still running, so the URL cannot be derived from live state any more.
//
// The first candidate is what we actually installed. The rest cover the case
// where this process never applied anything (the app was restarted, so an entry
// from a previous run is still in place), in which case it is worth trying both
// the local and the configured external PAC URL — the caller stops as soon as
// the helper reports that it recognised one.
func (a *App) pacMatchCandidates(sett config.Settings) []string {
	var out []string
	add := func(u string) {
		u = pac.ParseCacheBustedURL(strings.TrimSpace(u))
		if u == "" {
			return
		}
		for _, seen := range out {
			if proxyconf.PACURLEqual(seen, u) {
				return
			}
		}
		out = append(out, u)
	}
	if last, ok := a.applier.lastApplied(); ok {
		add(last.PACURL)
	}
	add(sett.PAC.ExternalPACURL)
	add(pac.ServerURL(sett.PAC.Address, sett.PAC.Port))
	return out
}

// currentServiceList is the set of network services the settings select.
func (a *App) currentServiceList(sett config.Settings) []string {
	if sett.AutoConfigureNetworkServices {
		services, err := proxyconf.ListServices()
		if err != nil {
			a.logf("warn", "proxy", "读取网络服务列表失败: %v", err)
			return nil
		}
		return proxyconf.AutoSelected(services)
	}
	out := make([]string, 0, len(sett.NetworkServices))
	seen := map[string]bool{}
	for _, s := range sett.NetworkServices {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// servicesToClean is the set we must clear when switching the proxy off: the
// services we last configured plus whatever the settings select now.
func (a *App) servicesToClean(sett config.Settings) []string {
	seen := map[string]bool{}
	var out []string
	if last, ok := a.applier.lastApplied(); ok {
		for _, s := range last.Services {
			if !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}
	for _, s := range a.currentServiceList(sett) {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// reconcile makes the system proxy match the settings.
func (p *proxyApplier) reconcile(reason string) error {
	p.markAttempt()
	mode, pacURL, services, err := p.app.desiredProxy()
	if err != nil {
		return err
	}

	info := helperStatus()
	if !info.Installed {
		if mode == "off" {
			// Nothing to undo: we never installed anything.
			return nil
		}
		return fmt.Errorf("尚未安装助手，无法修改系统代理（请在“状态”页安装助手）")
	}

	sett := p.app.store.Settings()
	if mode == "off" {
		// 关闭系统代理走单独的路径：它需要反复尝试几个"可能属于我们的"PAC 地址，
		// 否则助手会把设置当成别人的配置跳过，系统自动代理就会一直指向已经停掉的
		// PAC 服务（表现为"关了代理就断网"）。
		return p.reconcileOff(reason, sett, services)
	}
	args := []string{"--mode", mode, "--state-path", proxyconf.HelperStatePath}
	switch mode {
	case "auto":
		args = append(args, "--pac-url", pacURL)
	case "global":
		args = append(args, "--port", strconv.Itoa(sett.SOCKS.Port),
			"--socks-listen-address", sett.SOCKS.Address)
		if sett.HTTP.Enabled {
			args = append(args, "--http-port", strconv.Itoa(sett.HTTP.Port),
				"--http-listen-address", sett.HTTP.Address)
		}
	}
	for _, svc := range services {
		args = append(args, "--network-service", svc)
	}
	for _, ex := range p.app.exceptionList(sett) {
		args = append(args, "--proxy-exception", ex)
	}

	resp, err := callHelper(args...)
	if err != nil {
		p.mu.Lock()
		p.lastErr = err.Error()
		p.mu.Unlock()
		return err
	}
	for _, w := range resp.Warnings {
		p.app.logf("warn", "helper", "%s", w)
	}
	if !resp.OK {
		msg := resp.Error
		if msg == "" {
			msg = "助手未返回成功状态"
		}
		p.mu.Lock()
		p.lastErr = msg
		p.mu.Unlock()
		return fmt.Errorf("配置系统代理失败: %s", msg)
	}

	applied := appliedProxy{
		Mode:     mode,
		Services: resp.Applied,
		PACURL:   pacURL,
		Socks:    proxyconf.FormatPort(sett.SOCKS.Address, sett.SOCKS.Port),
	}
	if sett.HTTP.Enabled {
		applied.HTTP = proxyconf.FormatPort(sett.HTTP.Address, sett.HTTP.Port)
	}
	if len(applied.Services) == 0 && mode != "off" {
		applied.Services = services
	}
	p.record(applied)

	// mode is never "off" here: reconcile returns through reconcileOff above.
	switch {
	case mode == "auto":
		p.app.logf("info", "proxy", "已启用 PAC 自动代理（%s）：%s -> %s", reason, describeServices(applied.Services), stripCacheBuster(pacURL))
	default:
		p.app.logf("info", "proxy", "已启用全局代理（%s）：%s -> SOCKS %s", reason, describeServices(applied.Services), applied.Socks)
	}
	return nil
}

// reconcileOff clears the system proxy and logs the outcome.
func (p *proxyApplier) reconcileOff(reason string, sett config.Settings, services []string) error {
	cleared, err := p.clearProxy(sett, services)
	if err != nil {
		p.mu.Lock()
		p.lastErr = err.Error()
		p.mu.Unlock()
		return err
	}
	switch {
	case len(cleared) == 0:
		// Nothing of ours was in the way, which is normal in manual mode and
		// after a restart. Worth stating plainly: "off" here is a decision, not
		// a failure.
		p.app.logf("info", "proxy", "未改动系统代理（%s）：没有需要清理的设置", reason)
	default:
		p.record(appliedProxy{Mode: "off", Services: cleared})
		p.app.logf("info", "proxy", "已关闭系统代理（%s）：%s", reason, describeServices(cleared))
	}
	return nil
}

// clearProxy runs the helper in "off" mode and returns the services it actually
// cleared.
//
// The helper only touches settings that look like ours, and it can only compare
// one PAC URL per call, so we walk pacMatchCandidates and stop at the first
// candidate it recognises. In practice that is one call (the URL we installed);
// the extra attempts only happen after a restart, when this process has no
// record of what is currently installed.
func (p *proxyApplier) clearProxy(sett config.Settings, services []string) ([]string, error) {
	base := []string{"--mode", "off", "--state-path", proxyconf.HelperStatePath}
	// The SOCKS host/port identify us in global mode; passing them is harmless
	// in PAC mode and necessary when a global-mode entry was left behind.
	base = append(base,
		"--port", strconv.Itoa(sett.SOCKS.Port),
		"--socks-listen-address", sett.SOCKS.Address)
	for _, svc := range services {
		base = append(base, "--network-service", svc)
	}

	candidates := p.app.pacMatchCandidates(sett)
	if len(candidates) == 0 {
		candidates = []string{""} // 没有 PAC 可比对（例如只用全局模式）
	}
	var cleared []string
	for i, cand := range candidates {
		args := append([]string(nil), base...)
		if cand != "" {
			args = append(args, "--pac-url", cand)
		}
		resp, err := callHelper(args...)
		if err != nil {
			return nil, err
		}
		for _, w := range resp.Warnings {
			p.app.logf("warn", "helper", "%s", w)
		}
		if !resp.OK {
			msg := resp.Error
			if msg == "" {
				msg = "助手未返回成功状态"
			}
			return nil, fmt.Errorf("清除系统代理失败: %s", msg)
		}
		cleared = append(cleared, resp.Applied...)
		if len(resp.Applied) > 0 || i == len(candidates)-1 {
			break
		}
	}
	return cleared, nil
}

// verify re-applies the system proxy when it no longer matches the settings.
//
// This is what recovers from a wake up, a network change, another client taking
// over - or an earlier apply that never happened.
func (p *proxyApplier) verify() {
	sett := p.app.store.Settings()
	if !sett.Enabled {
		return
	}
	mode, pacURL, services, err := p.app.desiredProxy()
	if err != nil || mode == "off" {
		// Manual mode (or a missing PAC URL) means "leave the system alone".
		return
	}
	if len(services) == 0 {
		return
	}

	last, applied := p.lastApplied()
	stale := !applied || last.Mode != mode || !sameStrings(last.Services, services)
	if !stale {
		match := proxyconf.Match{SocksHost: sett.SOCKS.Address, SocksPort: sett.SOCKS.Port}
		if mode == "auto" {
			match.PACURL = stripCacheBuster(pacURL)
		}
		for _, svc := range services {
			st, rerr := proxyconf.ReadState(svc)
			if rerr != nil {
				continue // the service may have gone away; nothing to verify
			}
			if !proxyconf.Matches(st, match) {
				stale = true
				break
			}
		}
	}
	if !stale {
		return
	}
	// Do not hammer the helper if it keeps failing (for example because it is
	// not installed at all).
	if at := p.lastAttempt(); !at.IsZero() && time.Since(at) < 30*time.Second {
		return
	}
	p.app.logf("warn", "proxy", "系统代理与本应用设置不一致，正在重新应用")
	if err := p.reconcile("自动恢复"); err != nil {
		p.app.logf("error", "proxy", "自动恢复失败: %v", err)
	}
}

// sameStrings reports whether two string slices hold the same values in order.
func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// forceOff clears the proxy even if the settings still say it is on. Used when
// quitting and when turning the proxy off.
func (p *proxyApplier) forceOff(reason string) {
	sett := p.app.store.Settings()
	info := helperStatus()
	if !info.Installed {
		return
	}
	p.markAttempt()
	cleared, err := p.clearProxy(sett, p.app.servicesToClean(sett))
	if err != nil {
		p.app.logf("warn", "proxy", "清理系统代理失败（%s）: %v", reason, err)
		return
	}
	// 助手会跳过"看着不像我们写的"服务。什么都没清掉时不能再报"已清理"——
	// 之前正是这条假成功掩盖了 PAC 没被清掉、系统代理一直指向已停止的 PAC 服务。
	if len(cleared) == 0 {
		if last, ok := p.lastApplied(); ok && last.Mode != "off" {
			// 我们自己装过东西，现在却清不掉：这是用户能感知到的故障（断网），
			// 必须说清楚，而不是记成"已关闭"。
			p.app.logf("warn", "proxy",
				"未能清理系统代理（%s）：助手没有识别出我们安装的设置；系统代理可能仍指向本地端口", reason)
			return
		}
		p.app.logf("info", "proxy", "退出前无需清理系统代理（%s）", reason)
		return
	}
	p.app.logf("info", "proxy", "退出前已清理系统代理（%s）：%s", reason, describeServices(cleared))
	p.record(appliedProxy{Mode: "off", Services: cleared})
}

// exceptionList is the bypass list handed to the helper. Settings.ExceptionList
// already guarantees a non-empty result.
func (a *App) exceptionList(sett config.Settings) []string {
	return sett.ExceptionList()
}

func describeServices(services []string) string {
	if len(services) == 0 {
		return "未选择网络服务"
	}
	if len(services) == 1 {
		return services[0]
	}
	return fmt.Sprintf("%s 等 %d 个服务", services[0], len(services))
}

func stripCacheBuster(u string) string {
	if i := strings.IndexByte(u, '?'); i >= 0 {
		return u[:i]
	}
	return u
}

// proxyPortsInUse reports which of our listeners cannot be bound, so the UI can
// explain a conflict before the user hits "start".
func proxyPortsInUse(sett config.Settings) []string {
	var out []string
	for _, addr := range []string{
		listenAddr(sett.SOCKS.Address, sett.SOCKS.Port),
	} {
		if err := canListen(addr); err != nil {
			out = append(out, fmt.Sprintf("%s（%v）", addr, err))
		}
	}
	if sett.HTTP.Enabled {
		addr := listenAddr(sett.HTTP.Address, sett.HTTP.Port)
		if err := canListen(addr); err != nil {
			out = append(out, fmt.Sprintf("%s（%v）", addr, err))
		}
	}
	if sett.Mode == config.ModeAuto {
		addr := listenAddr(sett.PAC.Address, sett.PAC.Port)
		if err := canListen(addr); err != nil {
			out = append(out, fmt.Sprintf("%s（%v）", addr, err))
		}
	}
	return out
}

func listenAddr(host string, port int) string {
	return net.JoinHostPort(host, strconv.Itoa(port))
}

func canListen(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return ln.Close()
}
