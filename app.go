package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"

	"ssxng/internal/api"
	"ssxng/internal/config"
	"ssxng/internal/lockfile"
	"ssxng/internal/logbuf"
	"ssxng/internal/pac"
	"ssxng/internal/proxyconf"
	"ssxng/internal/sscore"
	"ssxng/internal/tray"
)

// Application lifecycle and the Wails-bound surface that the window
// talks to. The view builders here are shared by every other file.
// AppVersion is the application version reported to the UI.

const AppVersion = "1.0.0"

// Event names the frontend subscribes to (see docs/API.md).
const (
	EventState   = "ssxng:state"
	EventLogs    = "ssxng:logs"
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

	// pacRules memoises the generated PAC file's rule count so buildState does
	// not re-parse thousands of rules on every state push.
	pacRules pac.RuleCache

	// helperCache memoises the helper status probe, which spawns the setuid
	// helper process whenever the helper is installed.
	helperCache struct {
		mu   sync.Mutex
		at   time.Time
		info proxyconf.HelperStatusInfo
	}

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

	// logCh receives every line appended to logs; logPump batches it into
	// EventLogs. logCancel unsubscribes at shutdown. lastLogDrop is the UnixNano
	// of the last "the UI log fell behind" warning, used to rate limit it.
	logCancel   func()
	lastLogDrop atomic.Int64

	stopLoops chan struct{}
	loops     sync.WaitGroup

	// ensures the automatic first-use gfwlist download happens only once
	pacFetchOnce sync.Once
}

// NewApp creates the application object.
func NewApp() *App {
	return &App{logs: logbuf.New(500)}
}

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
	if a.logCancel != nil {
		a.logCancel()
		a.logCancel = nil
	}

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

// logf records a line in the in-app log and the log file.
//
// It deliberately does *not* talk to the frontend: a proxy serving one page
// logs an info line per connection and a debug line when each one ends, so a
// per-line EventsEmit meant a JSON serialisation and an IPC message per
// connection event. Lines reach the UI through startLogPump instead, which
// batches them; GetLogs still returns the ring verbatim, so nothing is lost if
// a batch is dropped.
func (a *App) logf(level, scope, format string, args ...any) {
	msg := format
	if len(args) > 0 {
		msg = fmt.Sprintf(format, args...)
	}
	var entry logbuf.Entry
	if a.logs != nil {
		// AddEntry hands the same entry to the pump's subscription, so the file
		// log and the UI batch can never disagree about the sequence number.
		entry = a.logs.AddEntry(level, scope, msg)
	} else {
		entry = logbuf.Entry{Time: time.Now(), Level: level, Scope: scope, Message: msg}
	}
	if a.logFile != nil {
		a.logFile.Write(entry)
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
		UDP:       a.udpView(sett, running),
		PAC:       a.pacView(sett),
		Servers:   a.serverViews(),
		Traffic:   a.trafficView(),
		Helper:    a.helperView(),
		Settings:  toSettingsView(sett),
		StartedAt: a.startedAt.Unix(),
	}
	if cur, ok := a.store.CurrentServer(); ok {
		view := a.serverView(cur, cur.ID)
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

// udpView reports the UDP relay. It shares the SOCKS port and is only up while
// the engine runs with UDP enabled, so the address shown is the resolved one
// the engine actually bound (which matters when the port was 0).
func (a *App) udpView(sett config.Settings, running bool) api.ListenerView {
	view := listenerView(sett.SOCKS.Address, sett.SOCKS.Port, sett.UDPEnabled && running)
	if !sett.UDPEnabled {
		return view
	}
	if a.engine != nil {
		if addr := a.engine.UDPAddr(); addr != "" {
			if host, port, err := net.SplitHostPort(addr); err == nil {
				if n, err := strconv.Atoi(port); err == nil {
					view.Address, view.Port = host, n
				}
			}
		}
	}
	return view
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

// ---------------------------------------------------------------- loops

func (a *App) startLoops() {
	if a.stopLoops != nil {
		return
	}
	stop := make(chan struct{})
	a.stopLoops = stop

	a.startLogPump(stop)

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

// Quit exits the application, clearing the system proxy when asked to.
func (a *App) Quit() {
	a.quitting.Store(true)
	if a.settings().ClearProxyOnQuit {
		a.applier.forceOff("退出")
	}
	wruntime.Quit(a.ctx)
}

// ShowWindow brings the window back.
func (a *App) ShowWindow() {
	wruntime.WindowShow(a.ctx)
	wruntime.WindowUnminimise(a.ctx)
}
