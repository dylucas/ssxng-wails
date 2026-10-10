package main

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"ssxng/internal/config"
	"ssxng/internal/sscore"
)

// Turning the local proxies on and off: start/stop, mode switching,
// and the enable toggle.

// startProxy starts the local listeners and installs the system proxy.
func (a *App) startProxy(reason string) error {
	sett := a.settings()
	srv, ok := a.store.CurrentServer()
	if !ok {
		return errors.New("请先添加并选择一个服务器")
	}
	cfg := sscore.Config{
		Server:      srv,
		SocksAddr:   listenAddr(sett.SOCKS.Address, sett.SOCKS.Port),
		DialTimeout: 15 * time.Second,
	}
	if sett.HTTP.Enabled {
		cfg.HTTPAddr = listenAddr(sett.HTTP.Address, sett.HTTP.Port)
	}
	if sett.UDPEnabled {
		// UDP shares the SOCKS port: the two are separate namespaces, and that is
		// the port an app is already configured to send its datagrams to.
		cfg.UDPAddr = cfg.SocksAddr
	}
	if err := a.engine.Start(cfg); err != nil {
		return err
	}
	a.syncPACServer()
	if err := a.applier.reconcile(reason); err != nil {
		// The local proxies work even when the system proxy could not be
		// installed, so this is a warning rather than a failure.
		a.toastError("proxy", err)
	}
	return nil
}

func (a *App) stopProxy(reason string) {
	if a.engine != nil {
		a.engine.Stop()
	}
	if a.applier != nil {
		a.applier.forceOff(reason)
	}
	a.syncPACServer()
}

// SetEnabled turns the proxy on or off.
//
// The new state is written to the settings *before* anything is applied:
// desiredProxy() works out what the system proxy should look like from the
// stored settings, so applying first would compute "off" and silently leave the
// system proxy alone.
func (a *App) SetEnabled(on bool) error {
	sett := a.settings()
	if on == sett.Enabled {
		return nil
	}
	if !on {
		sett.Enabled = false
		if err := a.store.SaveSettings(sett); err != nil {
			return err
		}
		a.stopProxy("关闭")
		a.logf("info", "app", "代理已关闭")
		a.emitState()
		return nil
	}
	if _, ok := a.store.CurrentServer(); !ok {
		return errors.New("请先在“服务器”页添加一个服务器")
	}
	for _, problem := range proxyPortsInUse(sett) {
		a.logf("warn", "proxy", "端口被占用: %s", problem)
	}
	sett.Enabled = true
	if err := a.store.SaveSettings(sett); err != nil {
		return err
	}
	if err := a.startProxy("开启"); err != nil {
		// Roll the stored state back so the UI does not claim the proxy is on.
		sett.Enabled = false
		if serr := a.store.SaveSettings(sett); serr != nil {
			a.logf("error", "app", "保存配置失败: %v", serr)
		}
		a.toastError("proxy", err)
		return err
	}
	a.setError(nil)
	a.logf("info", "app", "代理已开启（%s 模式）", modeLabel(sett.Mode))
	a.emitState()
	return nil
}

// SetMode switches the proxy mode.
func (a *App) SetMode(mode string) error {
	sett := a.settings()
	mode = strings.TrimSpace(mode)
	if !config.ValidMode(mode) {
		return fmt.Errorf("未知模式 %q", mode)
	}
	if mode != config.ModeOff && !modeEnabled(sett, mode) {
		return fmt.Errorf("%s 模式已在设置中关闭", modeLabel(mode))
	}
	if mode == config.ModeExternalPAC && strings.TrimSpace(sett.PAC.ExternalPACURL) == "" {
		return errors.New("请先在 PAC 页填写外部 PAC 地址")
	}
	if mode == sett.Mode {
		return nil
	}
	prev := sett.Mode
	sett.Mode = mode
	if err := a.store.SaveSettings(sett); err != nil {
		sett.Mode = prev
		return err
	}
	a.logf("info", "app", "模式切换: %s -> %s", modeLabel(prev), modeLabel(mode))

	a.syncPACServer()
	if sett.Enabled {
		if err := a.applier.reconcile("切换模式"); err != nil {
			a.toastError("proxy", err)
		}
	}
	a.emitState()
	return nil
}
