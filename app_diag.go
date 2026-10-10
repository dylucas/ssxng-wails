package main

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"time"

	"ssxng/internal/api"
	"ssxng/internal/config"
	"ssxng/internal/diagnose"
	"ssxng/internal/helperbin"
	"ssxng/internal/proxyconf"
)

// Diagnostics: helper status, self-test, and the diagnose report.

func (a *App) helperView() api.HelperStatus { return a.helperViewForced(false) }

// helperViewForced builds the helper status view. When force is false the
// underlying probe is served from a short lived cache.
//
// Why the cache exists: proxyconf.Status runs "<helper> --version" whenever the
// helper is installed, so an uncached probe forks a setuid process. buildState
// calls this on every state push (14 emitState call sites in this file plus the
// one second traffic tick's neighbours), which made a mode switch or a settings
// save spawn a process. The probe is only cached for helperProbeTTL; the
// refresh button, install and uninstall all force a real one, and the system
// proxy paths in apply.go deliberately call helperStatus() directly, where
// freshness matters more than the process cost.
func (a *App) helperViewForced(force bool) api.HelperStatus {
	info := a.helperInfo(force)
	applied := []string{}
	if last, ok := a.applier.lastApplied(); ok {
		applied = last.Services
	}
	return api.HelperStatus{
		Path:            info.Path,
		Installed:       info.Installed,
		Version:         info.Version,
		RequiredVersion: info.RequiredVersion,
		VersionOK:       info.VersionOK,
		Setuid:          info.Setuid,
		Owner:           info.Owner,
		Mode:            helperModeString(info),
		Applied:         applied,
		LastError:       firstNonEmpty(a.applier.lastApplyError(), info.LastError),
	}
}

// helperProbeTTL bounds how stale a cached helper probe may be.
const helperProbeTTL = 3 * time.Second

// helperInfo returns the helper status, probing at most once per helperProbeTTL
// unless force is set.
func (a *App) helperInfo(force bool) proxyconf.HelperStatusInfo {
	a.helperCache.mu.Lock()
	defer a.helperCache.mu.Unlock()
	if !force && !a.helperCache.at.IsZero() && time.Since(a.helperCache.at) < helperProbeTTL {
		return a.helperCache.info
	}
	info := helperStatus()
	a.helperCache.info = info
	a.helperCache.at = time.Now()
	return info
}

// invalidateHelperCache drops the memoised probe so the next view reflects a
// just-completed install or uninstall.
func (a *App) invalidateHelperCache() {
	a.helperCache.mu.Lock()
	a.helperCache.at = time.Time{}
	a.helperCache.mu.Unlock()
}

// InstallHelper installs the privileged helper (asks for an admin password).
func (a *App) InstallHelper() error {
	data, err := helperbin.Bytes()
	if err != nil {
		a.toastError("helper", err)
		return err
	}
	src, err := proxyconf.ExtractHelper(data)
	if err != nil {
		a.toastError("helper", err)
		return err
	}
	a.logf("info", "helper", "正在安装助手（需要管理员授权）…")
	info, err := proxyconf.InstallHelper(src)
	if err != nil {
		a.toastError("helper", err)
		return err
	}
	a.logf("info", "helper", "助手安装完成: %s（版本 %s）", info.Path, info.Version)
	a.invalidateHelperCache()
	a.emitToast("info", "助手安装完成")
	a.emitState()
	return nil
}

// HelperStatus reports the helper state. The 状态 page's refresh button calls
// this, so it bypasses the cache and always probes for real.
func (a *App) HelperStatus() api.HelperStatus { return a.helperViewForced(true) }

// SelfTest runs the built-in end-to-end check.
func (a *App) SelfTest() api.SelfTestReport {
	report := diagnose.SelfTest(a.diagnoseContext())
	steps := make([]api.SelfTestStep, 0, len(report.Steps))
	for _, s := range report.Steps {
		steps = append(steps, api.SelfTestStep{Name: s.Name, OK: s.OK, Detail: s.Detail})
		a.logf(pick(s.OK, "info", "error"), "selftest", "%s: %s", s.Name, s.Detail)
	}
	out := api.SelfTestReport{OK: report.OK, Steps: steps, ElapsedMS: report.ElapsedMS}
	a.emitState()
	return out
}

// Diagnose returns the diagnostics table.
func (a *App) Diagnose() []api.Check {
	checks := diagnose.Run(a.diagnoseContext())
	out := make([]api.Check, 0, len(checks))
	for _, c := range checks {
		out = append(out, api.Check{Name: c.Name, Status: c.Status, Detail: c.Detail, Suggestion: c.Suggestion})
	}
	return out
}

func (a *App) diagnoseContext() diagnose.Context {
	sett := a.settings()
	dctx := diagnose.Context{
		ConfigDir:     a.store.Dir(),
		Settings:      sett,
		Servers:       a.store.Servers(),
		EngineRunning: a.engine != nil && a.engine.Running(),
		PACRunning:    a.pacSrv != nil && a.pacSrv.Running(),
		SocksAddr:     listenAddr(sett.SOCKS.Address, sett.SOCKS.Port),
		HTTPAddr:      listenAddr(sett.HTTP.Address, sett.HTTP.Port),
		HTTPEnabled:   sett.HTTP.Enabled,
		PACAddr:       listenAddr(sett.PAC.Address, sett.PAC.Port),
		Helper:        helperStatus(),
		PACFile:       a.store.PACFilePath(),
		GFWListFile:   a.store.GFWListPath(),
		UserRuleFile:  a.store.UserRulePath(),
		LogFile:       a.store.LogFilePath(),
		Version:       AppVersion,
	}
	if cur, ok := a.store.CurrentServer(); ok {
		dctx.CurrentServer = &cur
	}
	return dctx
}

// helperModeString reports the installed helper's file mode, e.g. "4755".
func helperModeString(info proxyconf.HelperStatusInfo) string {
	if !info.Installed || info.Path == "" {
		return ""
	}
	fi, err := os.Stat(info.Path)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%04o", fi.Mode().Perm())
}

func describeProxy(enabled bool, host string, port int) string {
	if !enabled {
		return ""
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}

func describeAutoProxy(st proxyconf.ProxyState) string {
	if !st.AutoEnabled || st.AutoURL == "" {
		return ""
	}
	return st.AutoURL
}

func modeLabel(mode string) string {
	switch mode {
	case config.ModeAuto:
		return "自动代理（PAC）"
	case config.ModeGlobal:
		return "全局代理"
	case config.ModeManual:
		return "手动模式"
	case config.ModeExternalPAC:
		return "外部 PAC"
	case config.ModeOff:
		return "关闭"
	default:
		return mode
	}
}
