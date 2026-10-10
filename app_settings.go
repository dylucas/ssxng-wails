package main

import (
	"errors"
	"os"
	"strings"

	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"

	"ssxng/internal/api"
	"ssxng/internal/config"
	"ssxng/internal/launchd"
	"ssxng/internal/proxyconf"
	"ssxng/internal/tray"
)

// Settings: the UI/JSON view conversions, saving and applying,
// login-item and dock-icon toggles, and network service enumeration.

func toSettingsView(sett config.Settings) api.SettingsView {
	services := sett.NetworkServices
	if services == nil {
		services = []string{}
	}
	return api.SettingsView{
		SocksAddress:      sett.SOCKS.Address,
		SocksPort:         sett.SOCKS.Port,
		HTTPEnabled:       sett.HTTP.Enabled,
		HTTPAddress:       sett.HTTP.Address,
		HTTPPort:          sett.HTTP.Port,
		HTTPFollowGlobal:  sett.HTTP.FollowGlobal,
		UDPEnabled:        sett.UDPEnabled,
		PACAddress:        sett.PAC.Address,
		PACPort:           sett.PAC.Port,
		PACBindLocalhost:  sett.PAC.BindToLocalhost,
		GFWListURL:        sett.PAC.GFWListURL,
		ExternalPACURL:    sett.PAC.ExternalPACURL,
		Exceptions:        sett.Exceptions,
		AutoConfigure:     sett.AutoConfigureNetworkServices,
		NetworkServices:   services,
		EnablePAC:         sett.SwitchModes.PAC,
		EnableGlobal:      sett.SwitchModes.Global,
		EnableManual:      sett.SwitchModes.Manual,
		EnableExternalPAC: sett.SwitchModes.ExternalPAC,
		LaunchAtLogin:     sett.LaunchAtLogin,
		HideDockIcon:      sett.HideDockIcon,
		ClearProxyOnQuit:  sett.ClearProxyOnQuit,
		RestoreOnWake:     sett.RestoreProxyOnWake,
	}
}

// applySettingsView copies the view onto the stored settings.
func applySettingsView(sett *config.Settings, v api.SettingsView) {
	sett.SOCKS.Address = v.SocksAddress
	sett.SOCKS.Port = v.SocksPort
	sett.HTTP.Enabled = v.HTTPEnabled
	sett.HTTP.Address = v.HTTPAddress
	sett.HTTP.Port = v.HTTPPort
	sett.HTTP.FollowGlobal = v.HTTPFollowGlobal
	sett.UDPEnabled = v.UDPEnabled
	sett.PAC.Address = v.PACAddress
	sett.PAC.Port = v.PACPort
	sett.PAC.BindToLocalhost = v.PACBindLocalhost
	if strings.TrimSpace(v.GFWListURL) != "" {
		sett.PAC.GFWListURL = strings.TrimSpace(v.GFWListURL)
	}
	sett.PAC.ExternalPACURL = strings.TrimSpace(v.ExternalPACURL)
	sett.Exceptions = v.Exceptions
	sett.AutoConfigureNetworkServices = v.AutoConfigure
	if v.NetworkServices != nil {
		sett.NetworkServices = v.NetworkServices
	}
	sett.SwitchModes.PAC = v.EnablePAC
	sett.SwitchModes.Global = v.EnableGlobal
	sett.SwitchModes.Manual = v.EnableManual
	sett.SwitchModes.ExternalPAC = v.EnableExternalPAC
	sett.LaunchAtLogin = v.LaunchAtLogin
	sett.HideDockIcon = v.HideDockIcon
	sett.ClearProxyOnQuit = v.ClearProxyOnQuit
	sett.RestoreProxyOnWake = v.RestoreOnWake
}

// ListNetworkServices reports the macOS network services and their proxy state.
func (a *App) ListNetworkServices() ([]api.NetworkService, error) {
	services, err := proxyconf.ListServices()
	if err != nil {
		return nil, err
	}
	sett := a.settings()
	selected := map[string]bool{}
	for _, s := range sett.NetworkServices {
		selected[s] = true
	}
	auto := map[string]bool{}
	for _, s := range proxyconf.AutoSelected(services) {
		auto[s] = true
	}
	states := proxyconf.ReadAllStates(services)
	// A service counts as "ours" when its PAC URL or SOCKS proxy matches, and
	// when it still points at our default ports.
	match := proxyconf.Match{SocksHost: sett.SOCKS.Address, SocksPort: sett.SOCKS.Port}
	if a.pacSrv != nil && a.pacSrv.Running() {
		match.PACURL = a.pacSrv.URL()
	}
	out := make([]api.NetworkService, 0, len(services))
	for _, svc := range services {
		st := states[svc.Name]
		isSelected := selected[svc.Name]
		if sett.AutoConfigureNetworkServices {
			isSelected = auto[svc.Name]
		}
		out = append(out, api.NetworkService{
			Name:         svc.Name,
			HardwarePort: svc.HardwarePort,
			Device:       svc.Device,
			Selected:     isSelected,
			HasOurProxy:  proxyconf.Matches(st, match),
			WebProxy:     describeProxy(st.WebEnabled, st.WebHost, st.WebPort),
			SocksProxy:   describeProxy(st.SocksEnabled, st.SocksHost, st.SocksPort),
			AutoProxy:    describeAutoProxy(st),
		})
	}
	return out, nil
}

// SaveSettings stores the settings without applying them.
func (a *App) SaveSettings(view api.SettingsView) error {
	sett := a.settings()
	old := sett
	applySettingsView(&sett, view)
	if err := sett.Validate(); err != nil {
		return err
	}
	if err := a.store.SaveSettings(sett); err != nil {
		return err
	}
	if sett.LaunchAtLogin != old.LaunchAtLogin {
		if err := a.setLaunchAtLogin(sett.LaunchAtLogin); err != nil {
			a.logf("warn", "app", "应用登录项设置失败: %v", err)
		}
	}
	a.logf("info", "app", "设置已保存")
	a.emitState()
	return nil
}

// ApplySettings saves the settings and makes them take effect immediately.
func (a *App) ApplySettings(view api.SettingsView) error {
	sett := a.settings()
	old := sett
	applySettingsView(&sett, view)
	if err := sett.Validate(); err != nil {
		return err
	}
	// UDPEnabled belongs here: it decides whether the engine binds the UDP
	// relay, so toggling it without a restart would leave the switch doing
	// nothing until the next launch.
	needRestart := old.SOCKS != sett.SOCKS || old.HTTP != sett.HTTP ||
		old.UDPEnabled != sett.UDPEnabled ||
		old.PAC.Address != sett.PAC.Address || old.PAC.Port != sett.PAC.Port
	if err := a.store.SaveSettings(sett); err != nil {
		return err
	}
	if sett.LaunchAtLogin != old.LaunchAtLogin {
		if err := a.setLaunchAtLogin(sett.LaunchAtLogin); err != nil {
			a.logf("warn", "app", "应用登录项设置失败: %v", err)
		}
	}
	if sett.Enabled && needRestart {
		a.logf("info", "app", "监听设置已改变，正在重启本地代理")
		a.engine.Stop()
		a.syncPACServer()
		if err := a.startProxy("应用设置"); err != nil {
			a.toastError("proxy", err)
			return err
		}
	} else {
		a.syncPACServer()
		if sett.Enabled {
			if err := a.applier.reconcile("应用设置"); err != nil {
				a.toastError("proxy", err)
			}
		}
	}
	a.logf("info", "app", "设置已应用")
	a.emitState()
	return nil
}

// SetLaunchAtLogin toggles the login item.
func (a *App) SetLaunchAtLogin(on bool) error {
	sett := a.settings()
	sett.LaunchAtLogin = on
	if err := a.store.SaveSettings(sett); err != nil {
		return err
	}
	if err := a.setLaunchAtLogin(on); err != nil {
		a.toastError("app", err)
		return err
	}
	a.emitState()
	return nil
}

func (a *App) setLaunchAtLogin(on bool) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if err := launchd.SetEnabled(on, "org.ssxng.SSXNG", exe); err != nil {
		return err
	}
	if on {
		a.logf("info", "app", "已设置开机自动启动")
	} else {
		a.logf("info", "app", "已取消开机自动启动")
	}
	return nil
}

// SetHideDockIcon toggles the menu bar (no dock icon) mode.
func (a *App) SetHideDockIcon(on, ask bool) error {
	if ask && on {
		choice, err := wruntime.MessageDialog(a.ctx, wruntime.MessageDialogOptions{
			Type:    wruntime.QuestionDialog,
			Title:   "隐藏 Dock 图标",
			Message: "隐藏 Dock 图标后 SSXNG 只在菜单栏显示。关闭窗口不会退出程序，请从菜单栏图标选择“退出 SSXNG”。",
			Buttons: []string{"隐藏", "取消"},
		})
		if err != nil || choice != "隐藏" {
			return errors.New("已取消")
		}
	}
	sett := a.settings()
	sett.HideDockIcon = on
	if err := a.store.SaveSettings(sett); err != nil {
		return err
	}
	if err := tray.SetDockIconVisible(!on); err != nil {
		a.toastError("app", err)
		return err
	}
	if on {
		a.logf("info", "app", "已隐藏 Dock 图标，SSXNG 现在只显示菜单栏图标")
	} else {
		a.logf("info", "app", "已显示 Dock 图标")
	}
	a.emitState()
	return nil
}
