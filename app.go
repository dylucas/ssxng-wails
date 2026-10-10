package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	neturl "net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"

	"ssxng/internal/api"
	"ssxng/internal/config"
	"ssxng/internal/diagnose"
	"ssxng/internal/helperbin"
	"ssxng/internal/launchd"
	"ssxng/internal/lockfile"
	"ssxng/internal/logbuf"
	"ssxng/internal/pac"
	"ssxng/internal/proxyconf"
	"ssxng/internal/sscore"
	"ssxng/internal/ssurl"
	"ssxng/internal/tray"
)

// AppVersion is the application version reported to the UI.
const AppVersion = "1.0.0"

// Event names the frontend subscribes to (see docs/API.md).
const (
	EventState   = "ssxng:state"
	EventLog     = "ssxng:log"
	EventTraffic = "ssxng:traffic"
	EventToast   = "ssxng:toast"
)

// App is the Wails bound object. Every exported method becomes
// window.go.main.App.<Name>() in the frontend.
type App struct {
	ctx context.Context

	store   *config.Store
	logs    *logbuf.Buffer
	engine  *sscore.Engine
	pacSrv  *pac.Server
	applier *proxyApplier
	lock    *lockfile.Lock
	logFile *fileLogger

	startedAt time.Time

	mu        sync.Mutex
	lastError string
	lastSpeed struct {
		up, down int64
	}

	prevUp   atomic.Int64
	prevDown atomic.Int64
	prevTick atomic.Int64

	quitting atomic.Bool
	tray     *tray.Tray

	stopLoops chan struct{}
	loops     sync.WaitGroup

	// ensures the automatic first-use gfwlist download happens only once
	pacFetchOnce sync.Once
}

// NewApp creates the application object.
func NewApp() *App {
	return &App{logs: logbuf.New(500)}
}

// ---------------------------------------------------------------- lifecycle

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	a.startedAt = time.Now()

	dir, err := config.DefaultDir()
	if err != nil {
		a.fatal("无法确定配置目录: " + err.Error())
		return
	}
	store, err := config.Open(dir)
	if err != nil {
		a.fatal("无法读取配置: " + err.Error())
		return
	}
	a.store = store
	a.logFile = openFileLogger(store.LogFilePath(), 2<<20)
	a.applier = &proxyApplier{app: a}
	a.logf("info", "app", "SSXNG %s 启动，配置目录 %s", AppVersion, dir)

	lock, err := lockfile.Acquire(filepath.Join(store.Dir(), "ssxng.lock"))
	if err != nil {
		if errors.Is(err, lockfile.ErrAlreadyRunning) {
			msg := "SSXNG 已经在运行。"
			if info, ierr := lockfile.RunningInfo(filepath.Join(store.Dir(), "ssxng.lock")); ierr == nil && info.PID > 0 {
				msg = fmt.Sprintf("SSXNG 已经在运行（进程 %d）。\n请使用菜单栏图标操作，或先退出已有实例。", info.PID)
			}
			a.fatal(msg)
			return
		}
		a.logf("warn", "app", "无法创建单实例锁: %v", err)
	}
	a.lock = lock

	a.engine = sscore.NewEngine(func(level, scope, msg string) { a.logf(level, scope, "%s", msg) })
	a.pacSrv = pac.NewServer(a.readPACFile, func(level, message string) { a.logf(level, "pac", "%s", message) })

	sett := a.store.Settings()
	if sett.HideDockIcon {
		if err := tray.SetDockIconVisible(false); err != nil {
			a.logf("warn", "app", "隐藏 Dock 图标失败: %v", err)
		}
	}
	a.startTray()

	if sett.Enabled {
		if err := a.startProxy("启动"); err != nil {
			a.toastError("app", err)
			sett.Enabled = false
			if serr := a.store.SaveSettings(sett); serr != nil {
				a.logf("error", "app", "保存配置失败: %v", serr)
			}
		}
	} else {
		a.syncPACServer()
	}

	a.startLoops()
}

// domReady runs once the frontend has loaded and registered its listeners.
func (a *App) domReady(ctx context.Context) {
	a.emitState()
	a.emitToast("info", "SSXNG 已就绪")
}

// beforeClose returns true to cancel the window close.
//
// In menu bar mode the window is hidden instead of quitting, which matches
// ShadowsocksX-NG; quitting is then done from the menu bar icon. In normal mode
// closing the window quits the app, so a user can never get stuck.
func (a *App) beforeClose(ctx context.Context) bool {
	if a.quitting.Load() {
		return false
	}
	sett := a.settings()
	if !sett.HideDockIcon || a.tray == nil || !a.tray.Running() {
		return false
	}
	wruntime.WindowHide(ctx)
	a.emitToast("info", "SSXNG 仍在后台运行，可从菜单栏图标退出")
	return true
}

func (a *App) shutdown(ctx context.Context) {
	if a.stopLoops != nil {
		close(a.stopLoops)
		a.stopLoops = nil
	}
	a.loops.Wait()

	if a.applier != nil && a.store != nil {
		if a.settings().ClearProxyOnQuit {
			a.applier.forceOff("退出")
		} else if last, ok := a.applier.lastApplied(); ok && last.Mode != "off" {
			a.logf("warn", "proxy", "已按设置保留系统代理；它现在指向本地端口 %s，退出后网络将无法访问，重新打开 SSXNG 即可恢复", last.Socks)
		}
	}
	if a.engine != nil {
		a.engine.Stop()
	}
	if a.pacSrv != nil {
		a.pacSrv.Stop()
	}
	if a.tray != nil {
		a.tray.Stop()
	}
	if a.logFile != nil {
		a.logFile.Close()
	}
	if a.lock != nil {
		a.lock.Release()
		a.lock = nil
	}
}

// fatal reports an unrecoverable startup problem and quits.
func (a *App) fatal(message string) {
	a.logf("error", "app", "%s", message)
	if a.ctx != nil {
		_, _ = wruntime.MessageDialog(a.ctx, wruntime.MessageDialogOptions{
			Type:    wruntime.ErrorDialog,
			Title:   "SSXNG 无法启动",
			Message: message,
		})
		wruntime.Quit(a.ctx)
	}
}

// ---------------------------------------------------------------- logging

// logf records a line in the in-app log, the log file and the frontend.
func (a *App) logf(level, scope, format string, args ...any) {
	msg := format
	if len(args) > 0 {
		msg = fmt.Sprintf(format, args...)
	}
	if a.logs != nil {
		a.logs.Add(level, scope, msg)
	}
	var entry logbuf.Entry
	if a.logs != nil {
		entry = a.logs.Entries(1)[0]
	} else {
		entry = logbuf.Entry{Time: time.Now(), Level: level, Scope: scope, Message: msg}
	}
	if a.logFile != nil {
		a.logFile.Write(entry)
	}
	if a.ctx != nil {
		wruntime.EventsEmit(a.ctx, EventLog, a.toLogEntry(entry))
	}
}

func (a *App) toLogEntry(e logbuf.Entry) api.LogEntry {
	return api.LogEntry{
		Seq:     e.Seq,
		Time:    e.Time.Unix(),
		Level:   e.Level,
		Scope:   e.Scope,
		Message: e.Message,
	}
}

func (a *App) setError(err error) {
	a.mu.Lock()
	if err == nil {
		a.lastError = ""
	} else {
		a.lastError = err.Error()
	}
	a.mu.Unlock()
}

func (a *App) emitState() {
	if a.ctx == nil || a.store == nil {
		return
	}
	wruntime.EventsEmit(a.ctx, EventState, a.buildState())
	if a.tray != nil {
		a.tray.Refresh()
	}
}

func (a *App) emitToast(level, message string) {
	if a.ctx == nil {
		return
	}
	wruntime.EventsEmit(a.ctx, EventToast, map[string]string{"level": level, "message": message})
}

// toastError both logs and surfaces an error to the user.
func (a *App) toastError(scope string, err error) {
	if err == nil {
		return
	}
	a.setError(err)
	a.logf("error", scope, "%s", err.Error())
	a.emitToast("error", err.Error())
}

func (a *App) settings() config.Settings {
	if a.store == nil {
		return config.DefaultSettings()
	}
	return a.store.Settings()
}

// ---------------------------------------------------------------- state

func (a *App) buildState() api.State {
	sett := a.settings()
	running := a.engine != nil && a.engine.Running()
	st := api.State{
		Version:   AppVersion,
		Enabled:   sett.Enabled,
		Mode:      sett.Mode,
		Modes:     enabledModes(sett),
		Socks:     listenerView(sett.SOCKS.Address, sett.SOCKS.Port, running),
		HTTP:      listenerView(sett.HTTP.Address, sett.HTTP.Port, sett.HTTP.Enabled && running),
		PAC:       a.pacView(sett),
		Servers:   a.serverViews(sett),
		Traffic:   a.trafficView(),
		Helper:    a.helperView(),
		Settings:  toSettingsView(sett),
		StartedAt: a.startedAt.Unix(),
	}
	if cur, ok := a.store.CurrentServer(); ok {
		view := a.serverView(cur, cur.ID, sett)
		st.Server = &view
	}
	a.mu.Lock()
	st.LastError = a.lastError
	a.mu.Unlock()
	if st.Servers == nil {
		st.Servers = []api.ServerView{}
	}
	return st
}

func enabledModes(sett config.Settings) []string {
	out := []string{}
	for _, m := range []string{config.ModeAuto, config.ModeGlobal, config.ModeManual, config.ModeExternalPAC} {
		if modeEnabled(sett, m) {
			out = append(out, m)
		}
	}
	return append(out, config.ModeOff)
}

func modeEnabled(sett config.Settings, mode string) bool {
	switch mode {
	case config.ModeAuto:
		return sett.SwitchModes.PAC
	case config.ModeGlobal:
		return sett.SwitchModes.Global
	case config.ModeManual:
		return sett.SwitchModes.Manual
	case config.ModeExternalPAC:
		return sett.SwitchModes.ExternalPAC
	default:
		return true
	}
}

func listenerView(host string, port int, active bool) api.ListenerView {
	return api.ListenerView{Address: host, Port: port, Active: active}
}

func (a *App) trafficView() api.Traffic {
	var st sscore.Stats
	if a.engine != nil {
		st = a.engine.Stats()
	}
	a.mu.Lock()
	speed := a.lastSpeed
	a.mu.Unlock()
	return api.Traffic{
		Up:        st.Up,
		Down:      st.Down,
		UpSpeed:   speed.up,
		DownSpeed: speed.down,
		Active:    int(st.Active),
		Sessions:  st.Sessions,
	}
}

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
	data, err := a.readPACFile()
	if err != nil {
		if view.LastError == "" {
			view.LastError = err.Error()
		}
		return view
	}
	view.RuleCount = pac.RuleCountFromPAC(data)
	if info, serr := os.Stat(a.store.PACFilePath()); serr == nil {
		view.Generated = info.ModTime().Unix()
	}
	view.Source = a.pacSource(sett)
	return view
}

// pacSource is the human readable "数据源" for the PAC tab.
func (a *App) pacSource(sett config.Settings) string {
	if data, err := os.ReadFile(a.store.GFWListPath()); err == nil && len(data) > 0 {
		if info, serr := os.Stat(a.store.GFWListPath()); serr == nil {
			return fmt.Sprintf("%s（%s 更新）", hostOfURL(sett.PAC.GFWListURL), info.ModTime().Format("2006-01-02 15:04"))
		}
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

func (a *App) helperView() api.HelperStatus {
	info := helperStatus()
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

func (a *App) serverViews(sett config.Settings) []api.ServerView {
	servers := a.store.Servers()
	cur, _ := a.store.CurrentServer()
	out := make([]api.ServerView, 0, len(servers))
	for _, s := range servers {
		out = append(out, a.serverView(s, cur.ID, sett))
	}
	return out
}

func (a *App) serverView(s config.ServerProfile, currentID string, _ config.Settings) api.ServerView {
	running := a.engine != nil && a.engine.Running()
	active := running && s.ID == currentID
	return api.ServerView{
		ID:        s.ID,
		Host:      s.Host,
		Port:      s.Port,
		Method:    s.Method,
		Password:  s.Password,
		Remark:    s.Remark,
		Plugin:    s.Plugin,
		Title:     s.Title(),
		HasPlugin: strings.TrimSpace(s.Plugin) != "",
		Active:    active,
		Connected: active,
	}
}

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

// ---------------------------------------------------------------- proxy control

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
	return res, nil
}

const defaultUserRule = `! 用户自定义规则
! 语法与 AdBlock 相同，例如：
! ||example.com
! @@||example.com
`

// ---------------------------------------------------------------- loops

func (a *App) startLoops() {
	if a.stopLoops != nil {
		return
	}
	stop := make(chan struct{})
	a.stopLoops = stop

	a.loops.Add(2)
	go func() {
		defer a.loops.Done()
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				a.tickTraffic()
			}
		}
	}()
	go func() {
		defer a.loops.Done()
		ticker := time.NewTicker(20 * time.Second)
		defer ticker.Stop()
		last := time.Now()
		for {
			select {
			case <-stop:
				return
			case now := <-ticker.C:
				woke := now.Sub(last) > 90*time.Second
				last = now
				a.watchdog(woke)
			}
		}
	}()
}

// tickTraffic updates the speed counters and pushes them to the UI.
func (a *App) tickTraffic() {
	if a.engine == nil {
		return
	}
	st := a.engine.Stats()
	now := time.Now()
	prevTick := a.prevTick.Swap(now.UnixNano())
	up := a.prevUp.Swap(st.Up)
	down := a.prevDown.Swap(st.Down)
	if prevTick == 0 {
		return
	}
	elapsed := float64(now.UnixNano()-prevTick) / float64(time.Second)
	if elapsed <= 0 {
		elapsed = 1
	}
	upSpeed := int64(float64(st.Up-up) / elapsed)
	downSpeed := int64(float64(st.Down-down) / elapsed)
	if upSpeed < 0 {
		upSpeed = 0
	}
	if downSpeed < 0 {
		downSpeed = 0
	}
	a.mu.Lock()
	a.lastSpeed.up, a.lastSpeed.down = upSpeed, downSpeed
	a.mu.Unlock()

	if a.ctx != nil {
		wruntime.EventsEmit(a.ctx, EventTraffic, api.Traffic{
			Up:        st.Up,
			Down:      st.Down,
			UpSpeed:   upSpeed,
			DownSpeed: downSpeed,
			Active:    int(st.Active),
			Sessions:  st.Sessions,
		})
	}
}

// watchdog re-applies the system proxy after a wake up or when another program
// changed it.
func (a *App) watchdog(woke bool) {
	sett := a.settings()
	if !sett.Enabled || a.applier == nil || !sett.RestoreProxyOnWake {
		return
	}
	if woke {
		a.logf("info", "proxy", "检测到系统从睡眠中唤醒，正在检查代理设置")
	}
	a.applier.verify()
}

// ---------------------------------------------------------------- bindings

// GetState returns the full UI snapshot.
func (a *App) GetState() api.State { return a.buildState() }

// Version returns the application version.
func (a *App) Version() string { return AppVersion }

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

// CycleMode switches to the next enabled mode and returns it.
func (a *App) CycleMode() string {
	sett := a.settings()
	modes := enabledModes(sett)
	idx := 0
	for i, m := range modes {
		if m == sett.Mode {
			idx = i
			break
		}
	}
	next := modes[(idx+1)%len(modes)]
	if err := a.SetMode(next); err != nil {
		a.toastError("app", err)
		return sett.Mode
	}
	return next
}

// ListServers returns every configured server.
func (a *App) ListServers() []api.ServerView { return a.serverViews(a.settings()) }

// AddServer stores a new server.
func (a *App) AddServer(view api.ServerView) (api.ServerView, error) {
	profile := configFromView(view)
	profile.ID = ""
	if err := profile.Validate(); err != nil {
		return api.ServerView{}, err
	}
	profile.ID = config.NewID()
	if err := a.store.MutateServers(func(list []config.ServerProfile) ([]config.ServerProfile, error) {
		return append(list, profile), nil
	}); err != nil {
		return api.ServerView{}, err
	}
	if cur, ok := a.store.CurrentServer(); !ok || cur.ID == "" {
		if err := a.store.SetCurrentServerID(profile.ID); err != nil {
			return api.ServerView{}, err
		}
	}
	a.logf("info", "app", "已添加服务器 %s", profile.Title())
	a.afterServersChanged()
	return a.serverView(profile, profile.ID, a.settings()), nil
}

// UpdateServer changes an existing server.
func (a *App) UpdateServer(view api.ServerView) error {
	if strings.TrimSpace(view.ID) == "" {
		return errors.New("缺少服务器 ID")
	}
	profile := configFromView(view)
	profile.ID = view.ID
	if err := profile.Validate(); err != nil {
		return err
	}
	err := a.store.MutateServers(func(list []config.ServerProfile) ([]config.ServerProfile, error) {
		for i := range list {
			if list[i].ID == profile.ID {
				list[i] = profile
				return list, nil
			}
		}
		return list, fmt.Errorf("找不到服务器 %s", profile.ID)
	})
	if err != nil {
		return err
	}
	a.logf("info", "app", "已更新服务器 %s", profile.Title())
	a.afterServersChanged()
	return nil
}

// DeleteServer removes a server.
func (a *App) DeleteServer(id string) error {
	var removed config.ServerProfile
	err := a.store.MutateServers(func(list []config.ServerProfile) ([]config.ServerProfile, error) {
		out := make([]config.ServerProfile, 0, len(list))
		found := false
		for _, s := range list {
			if s.ID == id {
				removed = s
				found = true
				continue
			}
			out = append(out, s)
		}
		if !found {
			return list, fmt.Errorf("找不到服务器 %s", id)
		}
		return out, nil
	})
	if err != nil {
		return err
	}
	a.logf("info", "app", "已删除服务器 %s", removed.Title())

	if cur, ok := a.store.CurrentServer(); !ok || cur.ID == id {
		next := ""
		if servers := a.store.Servers(); len(servers) > 0 {
			next = servers[0].ID
		}
		if err := a.store.SetCurrentServerID(next); err != nil {
			return err
		}
		if next == "" && a.settings().Enabled {
			a.logf("warn", "app", "已没有可用服务器，正在关闭代理")
			if err := a.SetEnabled(false); err != nil {
				return err
			}
		}
	}
	a.afterServersChanged()
	return nil
}

// SelectServer makes a server the active one.
func (a *App) SelectServer(id string) error {
	if _, ok := a.store.ServerByID(id); !ok {
		return fmt.Errorf("找不到服务器 %s", id)
	}
	if err := a.store.SetCurrentServerID(id); err != nil {
		return err
	}
	srv, _ := a.store.ServerByID(id)
	a.logf("info", "app", "已选择服务器 %s", srv.Title())
	a.afterServersChanged()
	return nil
}

// MoveServer moves a server up (delta < 0) or down (delta > 0) in the list.
func (a *App) MoveServer(id string, delta int) error {
	if delta == 0 {
		return nil
	}
	step := 1
	if delta < 0 {
		step = -1
	}
	err := a.store.MutateServers(func(list []config.ServerProfile) ([]config.ServerProfile, error) {
		idx := -1
		for i, s := range list {
			if s.ID == id {
				idx = i
				break
			}
		}
		if idx < 0 {
			return list, fmt.Errorf("找不到服务器 %s", id)
		}
		j := idx + step
		if j < 0 || j >= len(list) {
			return list, nil
		}
		list[idx], list[j] = list[j], list[idx]
		return list, nil
	})
	if err != nil {
		return err
	}
	a.afterServersChanged()
	return nil
}

// ImportSSURL parses an ss:// link and adds it.
func (a *App) ImportSSURL(raw string) (api.ServerView, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return api.ServerView{}, errors.New("请输入 ss:// 链接")
	}
	parsed, err := ssurl.Parse(raw)
	if err != nil {
		return api.ServerView{}, err
	}
	method, ok := config.CanonicalMethod(parsed.Method)
	if !ok {
		return api.ServerView{}, fmt.Errorf("不支持的加密方式 %q，本版本支持: %s",
			parsed.Method, strings.Join(config.SupportedMethods, ", "))
	}
	profile := config.ServerProfile{
		Host:          parsed.Host,
		Port:          parsed.Port,
		Method:        method,
		Password:      parsed.Password,
		Remark:        parsed.Remark,
		Plugin:        parsed.Plugin,
		PluginOptions: parsed.PluginOptions,
	}
	if err := profile.Validate(); err != nil {
		return api.ServerView{}, err
	}
	for _, existing := range a.store.Servers() {
		if strings.EqualFold(existing.Host, profile.Host) && existing.Port == profile.Port && existing.Method == profile.Method {
			if err := a.SelectServer(existing.ID); err != nil {
				return api.ServerView{}, err
			}
			a.logf("info", "app", "服务器已存在，直接选中: %s", existing.Title())
			return a.serverView(existing, existing.ID, a.settings()), nil
		}
	}
	view, err := a.AddServer(api.ServerView{
		Host:     profile.Host,
		Port:     profile.Port,
		Method:   profile.Method,
		Password: profile.Password,
		Remark:   profile.Remark,
		Plugin:   profile.Plugin,
	})
	if err != nil {
		return api.ServerView{}, err
	}
	if err := a.SelectServer(view.ID); err != nil {
		return api.ServerView{}, err
	}
	return view, nil
}

// ExportSSURL renders a server as a SIP002 ss:// link.
func (a *App) ExportSSURL(id string) (string, error) {
	srv, ok := a.store.ServerByID(id)
	if !ok {
		return "", fmt.Errorf("找不到服务器 %s", id)
	}
	return ssurl.Profile{
		Host:          srv.Host,
		Port:          srv.Port,
		Method:        srv.Method,
		Password:      srv.Password,
		Remark:        srv.Remark,
		Plugin:        srv.Plugin,
		PluginOptions: srv.PluginOptions,
	}.String(), nil
}

// TestServer checks TCP connectivity to a server.
func (a *App) TestServer(id string) error {
	srv, ok := a.store.ServerByID(id)
	if !ok {
		return fmt.Errorf("找不到服务器 %s", id)
	}
	addr := net.JoinHostPort(srv.Host, strconv.Itoa(srv.Port))
	start := time.Now()
	conn, err := net.DialTimeout("tcp", addr, 6*time.Second)
	if err != nil {
		a.toastError("app", fmt.Errorf("无法连接 %s: %v", addr, err))
		return err
	}
	_ = conn.Close()
	msg := fmt.Sprintf("%s 可以连接（%d ms）", addr, time.Since(start).Milliseconds())
	a.logf("info", "app", "%s", msg)
	a.emitToast("info", msg)
	return nil
}

// GetLogs returns log lines after afterSeq (or the most recent 500).
func (a *App) GetLogs(afterSeq int64) []api.LogEntry {
	entries := a.logs.Entries(500)
	out := make([]api.LogEntry, 0, len(entries))
	for _, e := range entries {
		if afterSeq > 0 && e.Seq <= afterSeq {
			continue
		}
		out = append(out, a.toLogEntry(e))
	}
	return out
}

// ClearLogs empties the in-app log.
func (a *App) ClearLogs() {
	a.logs.Clear()
	a.logf("info", "app", "日志已清空")
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
	a.emitToast("info", "助手安装完成")
	a.emitState()
	return nil
}

// UninstallHelper removes the privileged helper.
func (a *App) UninstallHelper() error {
	if err := proxyconf.UninstallHelper(); err != nil {
		a.toastError("helper", err)
		return err
	}
	a.logf("info", "helper", "助手已卸载")
	a.emitState()
	return nil
}

// HelperStatus reports the helper state.
func (a *App) HelperStatus() api.HelperStatus { return a.helperView() }

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
	needRestart := old.SOCKS != sett.SOCKS || old.HTTP != sett.HTTP ||
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

// Quit exits the application, clearing the system proxy when asked to.
func (a *App) Quit() {
	a.quitting.Store(true)
	if a.settings().ClearProxyOnQuit {
		a.applier.forceOff("退出")
	}
	wruntime.Quit(a.ctx)
}

// HideWindow hides the main window.
func (a *App) HideWindow() { wruntime.WindowHide(a.ctx) }

// ShowWindow brings the window back.
func (a *App) ShowWindow() {
	wruntime.WindowShow(a.ctx)
	wruntime.WindowUnminimise(a.ctx)
}

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
		AppliedMode:   appliedModeOf(a.applier),
		Version:       AppVersion,
	}
	if cur, ok := a.store.CurrentServer(); ok {
		dctx.CurrentServer = &cur
	}
	return dctx
}

// afterServersChanged re-points a running engine at the current server.
func (a *App) afterServersChanged() {
	sett := a.settings()
	cur, ok := a.store.CurrentServer()
	if a.engine != nil && a.engine.Running() {
		switch {
		case !ok:
			a.logf("warn", "app", "没有可用服务器，正在关闭代理")
			if err := a.SetEnabled(false); err != nil {
				a.toastError("app", err)
			}
			return
		case sett.Enabled:
			if err := a.engine.SetServer(cur); err != nil {
				a.toastError("proxy", err)
			}
		}
	}
	a.emitState()
}

// startTray creates the menu bar icon.
func (a *App) startTray() {
	t, err := tray.Start(tray.Menu{
		OnToggle: func() {
			sett := a.settings()
			if err := a.SetEnabled(!sett.Enabled); err != nil {
				a.toastError("app", err)
			}
		},
		OnMode: func(mode string) {
			if err := a.SetMode(mode); err != nil {
				a.toastError("app", err)
			}
		},
		OnShow: func() { a.ShowWindow() },
		OnQuit: func() { a.Quit() },
		State:  a.trayState,
	})
	if err != nil {
		a.logf("warn", "app", "菜单栏图标不可用: %v", err)
		return
	}
	a.tray = t
}

// trayState feeds the menu bar with the current status line and mode list.
func (a *App) trayState() (string, bool, string, string, []tray.ModeOption) {
	sett := a.settings()
	status := modeLabel(sett.Mode)
	if !sett.Enabled {
		status = "已关闭"
	} else if cur, ok := a.store.CurrentServer(); ok {
		status = fmt.Sprintf("%s · %s", status, cur.Title())
	}
	modes := make([]tray.ModeOption, 0, len(config.ModeOrder))
	for _, m := range enabledModes(sett) {
		modes = append(modes, tray.ModeOption{Key: m, Title: modeLabel(m)})
	}
	return "SSXNG", sett.Enabled, status, sett.Mode, modes
}

// ---------------------------------------------------------------- helpers

func configFromView(view api.ServerView) config.ServerProfile {
	method, ok := config.CanonicalMethod(view.Method)
	if !ok {
		method = strings.ToLower(strings.TrimSpace(view.Method))
	}
	return config.ServerProfile{
		ID:       strings.TrimSpace(view.ID),
		Host:     strings.TrimSpace(view.Host),
		Port:     view.Port,
		Method:   method,
		Password: view.Password,
		Remark:   strings.TrimSpace(view.Remark),
		Plugin:   strings.TrimSpace(view.Plugin),
	}
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

func appliedModeOf(p *proxyApplier) string {
	if p == nil {
		return ""
	}
	if last, ok := p.lastApplied(); ok {
		return last.Mode
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func pick(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}

func revealInFinder(path string) error {
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("文件不存在: %s", path)
	}
	if err := exec.Command("/usr/bin/open", "-R", path).Run(); err != nil {
		return fmt.Errorf("无法在 Finder 中显示 %s: %w", path, err)
	}
	return nil
}

// downloadFile fetches a URL. When proxy is not empty the request goes through
// it (used to fetch rules through our own tunnel).
func downloadFile(url, proxy string, limit int64) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "SSXNG/"+AppVersion)
	client := &http.Client{Timeout: 90 * time.Second}
	if proxy != "" {
		purl, perr := neturl.Parse(proxy)
		if perr != nil {
			return nil, fmt.Errorf("本地代理地址无效: %w", perr)
		}
		client.Transport = &http.Transport{Proxy: http.ProxyURL(purl)}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("下载失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("下载失败: HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return nil, fmt.Errorf("读取下载内容失败: %w", err)
	}
	if len(data) == 0 {
		return nil, errors.New("下载内容为空")
	}
	return data, nil
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// fileLogger appends log lines to a file, rotating by truncation.
type fileLogger struct {
	mu   sync.Mutex
	path string
	max  int64
	f    *os.File
	size int64
}

func openFileLogger(path string, max int64) *fileLogger {
	l := &fileLogger{path: path, max: max}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return l
	}
	if info, err := os.Stat(path); err == nil && info.Size() > max {
		_ = os.Remove(path)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return l
	}
	l.f = f
	if info, err := f.Stat(); err == nil {
		l.size = info.Size()
	}
	return l
}

func (l *fileLogger) Write(e logbuf.Entry) {
	if l == nil || l.f == nil {
		return
	}
	line := fmt.Sprintf("%s [%s] %s: %s\n", e.Time.Format("2006-01-02 15:04:05"), strings.ToUpper(e.Level), e.Scope, e.Message)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.size > l.max {
		if err := l.f.Truncate(0); err != nil {
			return
		}
		if _, err := l.f.Seek(0, 0); err != nil {
			return
		}
		l.size = 0
	}
	n, err := l.f.WriteString(line)
	if err == nil {
		l.size += int64(n)
	}
}

func (l *fileLogger) Close() {
	if l == nil || l.f == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_ = l.f.Close()
	l.f = nil
}
