package main

import (
	"net"
	"strconv"
	"strings"
	"testing"

	"ssxng/internal/config"
	"ssxng/internal/logbuf"
	"ssxng/internal/pac"
	"ssxng/internal/proxyconf"
	"ssxng/internal/sscore"
)

// helperStub replaces the privileged helper. It records every invocation and,
// crucially, what the stored settings said at that moment: the app derives what
// to install from them, so "settings still say off" is exactly the bug that
// leaves the system proxy untouched.
type helperStub struct {
	store   *config.Store
	calls   [][]string
	enabled []bool
	applied []string
}

func stubHelper(t *testing.T, store *config.Store, applied []string) *helperStub {
	t.Helper()
	h := &helperStub{store: store, applied: applied}
	oldCall, oldStatus := callHelper, helperStatus
	callHelper = func(args ...string) (proxyconf.Response, error) {
		h.calls = append(h.calls, append([]string(nil), args...))
		if store != nil {
			h.enabled = append(h.enabled, store.Settings().Enabled)
		}
		return proxyconf.Response{OK: true, Applied: h.applied}, nil
	}
	helperStatus = func() proxyconf.HelperStatusInfo {
		return proxyconf.HelperStatusInfo{
			Installed:       true,
			VersionOK:       true,
			Setuid:          true,
			Path:            "/tmp/ssxng-helper-stub",
			Owner:           "0:80",
			Version:         proxyconf.HelperVersion,
			RequiredVersion: proxyconf.HelperVersion,
		}
	}
	t.Cleanup(func() {
		callHelper, helperStatus = oldCall, oldStatus
	})
	return h
}

func (h *helperStub) arg(call int, name string) string {
	if call < 0 || call >= len(h.calls) {
		return ""
	}
	args := h.calls[call]
	for i := 0; i+1 < len(args); i++ {
		if args[i] == name {
			return args[i+1]
		}
	}
	return ""
}

func (h *helperStub) hasValue(call int, name, want string) bool {
	if call < 0 || call >= len(h.calls) {
		return false
	}
	args := h.calls[call]
	for i := 0; i+1 < len(args); i++ {
		if args[i] == name && args[i+1] == want {
			return true
		}
	}
	return false
}

func (h *helperStub) modes() []string {
	out := make([]string, 0, len(h.calls))
	for i := range h.calls {
		out = append(out, h.arg(i, "--mode"))
	}
	return out
}

// freePort returns a loopback port that was free a moment ago, so the test never
// fights the user's running instance for 1086/1087/1089.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, portStr, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func newTestApp(t *testing.T) (*App, *config.Store, config.Settings) {
	t.Helper()
	store, err := config.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sett := config.DefaultSettings()
	// Global mode installs the SOCKS/HTTP proxy directly; the auto mode tests
	// switch to "auto" explicitly.
	sett.Mode = config.ModeGlobal
	sett.SOCKS.Port = freePort(t)
	sett.HTTP.Port = freePort(t)
	sett.PAC.Port = freePort(t)
	// Avoid shelling out to networksetup: pick the services explicitly.
	sett.AutoConfigureNetworkServices = false
	sett.NetworkServices = []string{"Wi-Fi"}
	if err := store.SaveSettings(sett); err != nil {
		t.Fatal(err)
	}
	srv := config.ServerProfile{Host: "127.0.0.1", Port: 1, Method: "aes-256-gcm", Password: "pw"}
	srv.ID = config.NewID()
	if err := store.MutateServers(func([]config.ServerProfile) ([]config.ServerProfile, error) {
		return []config.ServerProfile{srv}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetCurrentServerID(srv.ID); err != nil {
		t.Fatal(err)
	}

	app := &App{store: store, logs: logbuf.New(200)}
	app.applier = &proxyApplier{app: app}
	app.engine = sscore.NewEngine(func(string, string, string) {})
	app.pacSrv = pac.NewServer(app.readPACFile, func(string, string) {})
	t.Cleanup(func() {
		app.engine.Stop()
		app.pacSrv.Stop()
	})
	return app, store, sett
}

// TestSetEnabledInstallsProxyOnFirstEnable is the regression test for the bug
// where turning the proxy on left the system proxy untouched: reconcile ran
// before "enabled" was stored, so it computed mode "off" and only cleaned up.
func TestSetEnabledInstallsProxyOnFirstEnable(t *testing.T) {
	app, store, sett := newTestApp(t)
	h := stubHelper(t, store, []string{"Wi-Fi"})

	if err := app.SetEnabled(true); err != nil {
		t.Fatalf("开启代理失败: %v", err)
	}
	defer func() { _ = app.SetEnabled(false) }()

	if len(h.calls) != 1 {
		t.Fatalf("应当恰好调用一次助手，实际 %d 次: %v", len(h.calls), h.modes())
	}
	if h.arg(0, "--mode") != "global" {
		t.Errorf("模式应为 global（开启时不能算成 off），实际 %q（参数: %v）", h.arg(0, "--mode"), h.calls[0])
	}
	if len(h.enabled) == 1 && !h.enabled[0] {
		t.Error("调用助手时设置里必须已经记录为开启，否则助手只会清理而不会安装")
	}
	if !store.Settings().Enabled {
		t.Error("开启后设置应当已持久化为 enabled")
	}
	if got := h.arg(0, "--port"); got != strconv.Itoa(sett.SOCKS.Port) {
		t.Errorf("--port 应为 %d，实际 %q", sett.SOCKS.Port, got)
	}
	if got := h.arg(0, "--http-port"); got != strconv.Itoa(sett.HTTP.Port) {
		t.Errorf("--http-port 应为 %d，实际 %q", sett.HTTP.Port, got)
	}
	if !h.hasValue(0, "--network-service", "Wi-Fi") {
		t.Errorf("应当把选中的网络服务传给助手: %v", h.calls[0])
	}
	if !h.hasValue(0, "--proxy-exception", "127.0.0.1") {
		t.Errorf("应当把例外列表传给助手: %v", h.calls[0])
	}
	if got := h.arg(0, "--socks-listen-address"); got != "127.0.0.1" {
		t.Errorf("--socks-listen-address 应为 127.0.0.1，实际 %q", got)
	}
}

func TestSetEnabledOffClearsProxy(t *testing.T) {
	app, store, _ := newTestApp(t)
	h := stubHelper(t, store, []string{"Wi-Fi"})

	if err := app.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	if err := app.SetEnabled(false); err != nil {
		t.Fatal(err)
	}
	if len(h.calls) < 2 {
		t.Fatalf("关闭时也应当调用助手清理: %v", h.modes())
	}
	last := len(h.calls) - 1
	if h.arg(last, "--mode") != "off" {
		t.Errorf("关闭时模式应为 off，实际 %q", h.arg(last, "--mode"))
	}
	if store.Settings().Enabled {
		t.Error("关闭后设置应当是 enabled=false")
	}
	if app.engine.Running() {
		socksAddr, httpAddr := app.engine.Addresses()
		t.Errorf("关闭后本地监听应当已停止，实际 %s / %s", socksAddr, httpAddr)
	}
}

// TestDesiredProxyIsOffWhenDisabled documents the trap that caused the bug: the
// applier reads the stored settings, so the state has to be persisted first.
func TestDesiredProxyIsOffWhenDisabled(t *testing.T) {
	app, store, _ := newTestApp(t)
	mode, _, _, err := app.desiredProxy()
	if err != nil {
		t.Fatal(err)
	}
	if mode != "off" {
		t.Fatalf("未开启时应当为 off，实际 %q", mode)
	}
	if _, _, services, _ := app.desiredProxy(); len(services) == 0 {
		t.Error("即便是 off，也应当带上要清理的网络服务")
	}

	sett := store.Settings()
	sett.Enabled = true
	if err := store.SaveSettings(sett); err != nil {
		t.Fatal(err)
	}
	mode, _, _, err = app.desiredProxy()
	if err != nil {
		t.Fatal(err)
	}
	if mode != "global" {
		t.Fatalf("开启 + 全局模式时应当为 global，实际 %q", mode)
	}
}

// TestVerifyRecoversUnappliedProxy covers the self healing path: the watchdog
// must notice that the system proxy does not match the settings and re-apply,
// even when the recorded state says "off" (which is what the bug left behind).
func TestVerifyRecoversUnappliedProxy(t *testing.T) {
	app, store, _ := newTestApp(t)
	sett := store.Settings()
	sett.Enabled = true
	if err := store.SaveSettings(sett); err != nil {
		t.Fatal(err)
	}
	// What the broken enable recorded: "we cleaned up, nothing to install".
	app.applier.record(appliedProxy{Mode: "off"})
	h := stubHelper(t, store, []string{"Wi-Fi"})

	app.applier.verify()
	if len(h.calls) != 1 {
		t.Fatalf("看门狗应当重新应用一次，实际 %d 次: %v", len(h.calls), h.modes())
	}
	if h.arg(0, "--mode") != "global" {
		t.Errorf("重新应用时模式应为 global，实际 %q", h.arg(0, "--mode"))
	}
}

func TestVerifyDoesNothingWhenDisabledOrManual(t *testing.T) {
	app, store, _ := newTestApp(t)
	h := stubHelper(t, store, []string{"Wi-Fi"})

	app.applier.verify()
	if len(h.calls) != 0 {
		t.Errorf("未开启时看门狗不应调用助手: %v", h.modes())
	}

	sett := store.Settings()
	sett.Enabled = true
	sett.Mode = config.ModeManual
	if err := store.SaveSettings(sett); err != nil {
		t.Fatal(err)
	}
	if mode, _, _, err := app.desiredProxy(); err != nil || mode != "off" {
		t.Fatalf("手动模式应当表达为 off（不改系统代理），实际 %q %v", mode, err)
	}
	app.applier.verify()
	if len(h.calls) != 0 {
		t.Errorf("手动模式看门狗不应调用助手: %v", h.modes())
	}
}

// TestVerifyIsRateLimited keeps a failing helper from being hammered every tick.
func TestVerifyIsRateLimited(t *testing.T) {
	app, store, _ := newTestApp(t)
	sett := store.Settings()
	sett.Enabled = true
	if err := store.SaveSettings(sett); err != nil {
		t.Fatal(err)
	}
	h := stubHelper(t, store, []string{"Wi-Fi"})

	app.applier.verify()
	app.applier.verify()
	app.applier.verify()
	if len(h.calls) > 1 {
		t.Errorf("短时间内的重复检查应当被限流，实际调用 %d 次", len(h.calls))
	}
}

func TestModeSwitchToAutoPassesPACURL(t *testing.T) {
	app, store, sett := newTestApp(t)
	// The PAC server needs a file or it cannot start; give it one.
	if err := writeFileAtomic(store.PACFilePath(), []byte(`var rules = ["a"];`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := app.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = app.SetEnabled(false) }()

	h := stubHelper(t, store, []string{"Wi-Fi"})
	if err := app.SetMode(config.ModeAuto); err != nil {
		t.Fatalf("切换到自动代理失败: %v", err)
	}
	if len(h.calls) != 1 {
		t.Fatalf("切换模式应当调用助手一次，实际 %d 次", len(h.calls))
	}
	if h.arg(0, "--mode") != "auto" {
		t.Errorf("模式应为 auto，实际 %q", h.arg(0, "--mode"))
	}
	pacURL := h.arg(0, "--pac-url")
	if pacURL == "" {
		t.Fatal("auto 模式必须带上 PAC 地址")
	}
	want := "http://127.0.0.1:" + strconv.Itoa(sett.PAC.Port) + "/proxy.pac"
	if len(pacURL) < len(want) || pacURL[:len(want)] != want {
		t.Errorf("PAC 地址应以 %s 开头（带缓存击穿参数），实际 %q", want, pacURL)
	}
	if !app.pacSrv.Running() {
		t.Error("自动模式下 PAC 服务器应当在运行")
	}
}

func TestExternalPACModeValidatesURL(t *testing.T) {
	app, store, _ := newTestApp(t)
	sett := store.Settings()
	sett.Enabled = true
	sett.Mode = config.ModeExternalPAC
	sett.PAC.ExternalPACURL = "ftp://example.com/proxy.pac"
	if err := store.SaveSettings(sett); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := app.desiredProxy(); err == nil {
		t.Fatal("非法协议的外部 PAC 地址应当被拒绝，而不是写进系统设置")
	}
	sett.PAC.ExternalPACURL = "https://example.com/proxy.pac"
	if err := store.SaveSettings(sett); err != nil {
		t.Fatal(err)
	}
	mode, pacURL, _, err := app.desiredProxy()
	if err != nil {
		t.Fatal(err)
	}
	if mode != "auto" || pacURL != "https://example.com/proxy.pac" {
		t.Fatalf("外部 PAC 应当作为 auto 模式安装: %q %q", mode, pacURL)
	}
}

// TestDisableAutoModeClearsPACEntry is the regression test for the bug where
// turning the proxy off in PAC mode left macOS with "Auto Proxy" still enabled,
// pointing at the (now stopped) local PAC server.
//
// The helper only touches a service whose settings look like ours, and in auto
// mode the PAC URL is the only thing that identifies us. The old code derived
// that URL from the running PAC server, which by then had already been stopped
// (and from sett.Enabled, which was already false), so it passed no --pac-url
// at all and the helper skipped every service. The app still logged "已清理系统代理".
func TestDisableAutoModeClearsPACEntry(t *testing.T) {
	app, store, sett := newTestApp(t)
	if err := writeFileAtomic(store.PACFilePath(), []byte(`var rules = ["a"];`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := app.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	if err := app.SetMode(config.ModeAuto); err != nil {
		t.Fatal(err)
	}
	if !app.pacSrv.Running() {
		t.Fatal("自动模式下 PAC 服务器应当在运行")
	}

	h := stubHelper(t, store, []string{"Wi-Fi"})
	if err := app.SetEnabled(false); err != nil {
		t.Fatalf("关闭代理失败: %v", err)
	}
	if app.pacSrv.Running() {
		t.Error("关闭后 PAC 服务器应当停止")
	}
	if len(h.calls) == 0 {
		t.Fatal("关闭代理应当调用助手清理系统代理")
	}
	last := len(h.calls) - 1
	if h.arg(last, "--mode") != "off" {
		t.Fatalf("关闭时模式应为 off，实际 %q", h.arg(last, "--mode"))
	}
	// 这里是关键：必须带上我们自己的 PAC 地址，助手才认得出这条 PAC 是我们的。
	pacURL := h.arg(last, "--pac-url")
	want := "http://127.0.0.1:" + strconv.Itoa(sett.PAC.Port) + "/proxy.pac"
	if pacURL != want {
		t.Errorf("关闭时必须带上用于比对的 PAC 地址 %q，实际 %q", want, pacURL)
	}
	if !h.hasValue(last, "--network-service", "Wi-Fi") {
		t.Error("关闭时应当带上需要清理的网络服务")
	}
}

// TestForceOffKeepsPACMatchURLAfterRestart covers the harder case: the app was
// restarted, so nothing was applied in this process, yet macOS may still carry
// our PAC entry from a previous run. The matching URL then has to come from the
// settings rather than from the (never started) PAC server.
func TestForceOffKeepsPACMatchURLAfterRestart(t *testing.T) {
	app, store, sett := newTestApp(t)
	sett.Enabled = false
	sett.Mode = config.ModeAuto
	if err := store.SaveSettings(sett); err != nil {
		t.Fatal(err)
	}
	h := stubHelper(t, store, []string{"Wi-Fi"})

	app.applier.forceOff("退出")

	if len(h.calls) != 1 {
		t.Fatalf("应当调用助手一次，实际 %d 次", len(h.calls))
	}
	if h.arg(0, "--mode") != "off" {
		t.Errorf("模式应为 off，实际 %q", h.arg(0, "--mode"))
	}
	want := "http://127.0.0.1:" + strconv.Itoa(sett.PAC.Port) + "/proxy.pac"
	if got := h.arg(0, "--pac-url"); got != want {
		t.Errorf("重启后也应当能从设置推出 PAC 地址 %q，实际 %q", want, got)
	}
}

// TestForceOffDoesNotClaimSuccessWhenNothingWasCleared pins the second half of
// the bug: the helper reports the services it actually touched, so a skipped
// service must not be recorded (or logged) as "已清理". When we know we had
// installed something, that mismatch is a user-visible failure and must be a
// warning, because the machine is left with a proxy pointing at a dead port.
func TestForceOffDoesNotClaimSuccessWhenNothingWasCleared(t *testing.T) {
	app, store, sett := newTestApp(t)
	sett.Enabled = false
	sett.Mode = config.ModeAuto
	if err := store.SaveSettings(sett); err != nil {
		t.Fatal(err)
	}
	// 先记成"我们装过 auto"，再让助手跳过所有服务。
	app.applier.record(appliedProxy{
		Mode:     "auto",
		Services: []string{"Wi-Fi"},
		PACURL:   "http://127.0.0.1:1089/proxy.pac?v=1",
	})
	h := stubHelper(t, store, nil) // 助手什么都没清
	app.applier.forceOff("退出")

	// 助手一个都没认出来时，会依次试完所有候选 PAC 地址（最多两个：记录里的 + 设置里的）
	if len(h.calls) == 0 {
		t.Fatal("应当调用助手尝试清理")
	}
	for i := range h.calls {
		if h.arg(i, "--mode") != "off" {
			t.Errorf("第 %d 次调用应为 off 模式，实际 %q", i, h.arg(i, "--mode"))
		}
	}
	if len(h.calls) > 2 {
		t.Errorf("候选地址最多两个，不应尝试 %d 次", len(h.calls))
	}
	last, _ := app.applier.lastApplied()
	if last.Mode != "auto" {
		t.Errorf("没有清掉任何服务时，不应把状态记成 off，实际 %q", last.Mode)
	}
	entries := app.logs.Entries(50)
	found := false
	for _, e := range entries {
		if e.Level == "warn" && strings.Contains(e.Message, "未能清理系统代理") {
			found = true
		}
	}
	if !found {
		t.Errorf("清不掉时应当给出 warn 提示，实际日志：%v", entries)
	}

	// 反过来：本来就没装过东西（例如第一次运行就退出），不该报警。
	app2, store2, sett2 := newTestApp(t)
	sett2.Enabled = false
	if err := store2.SaveSettings(sett2); err != nil {
		t.Fatal(err)
	}
	h2 := stubHelper(t, store2, nil)
	app2.applier.forceOff("退出")
	if len(h2.calls) == 0 {
		t.Fatal("应当调用助手尝试清理")
	}
	for _, e := range app2.logs.Entries(50) {
		if e.Level == "warn" && strings.Contains(e.Message, "未能清理系统代理") {
			t.Errorf("从未安装过时不应告警，实际日志：%v", e)
		}
	}
}

// TestPACMatchCandidates covers the ordered list used to recognise our own PAC
// entry when clearing: what we installed first, then the configured external
// PAC, then our local one.
func TestPACMatchCandidates(t *testing.T) {
	app, _, sett := newTestApp(t)

	// 没有任何记录时：外部 PAC（若配置了）+ 本地 PAC
	sett.PAC.ExternalPACURL = "https://example.com/p.pac/"
	sett.PAC.Address = "127.0.0.1"
	local := pac.ServerURL(sett.PAC.Address, sett.PAC.Port)
	got := app.pacMatchCandidates(sett)
	want := []string{"https://example.com/p.pac", local}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("候选顺序应为 %v，实际 %v", want, got)
	}

	// 有记录时：我们实际装过的地址排在最前面，且去掉缓存击穿参数
	app.applier.record(appliedProxy{Mode: "auto", PACURL: local + "?v=7"})
	got = app.pacMatchCandidates(sett)
	if len(got) == 0 || got[0] != local {
		t.Fatalf("应当优先使用我们实际安装过的 PAC 地址，实际 %v", got)
	}
	// 本地地址与"装过的"是同一个 → 去重，不应重复出现
	seen := map[string]int{}
	for _, u := range got {
		seen[u]++
	}
	if seen[local] != 1 {
		t.Errorf("重复的 PAC 地址应当被去重，实际 %v", got)
	}

	// 全局模式：仍然给出候选（调用方只在 off 模式用），但至少不能为空
	sett.PAC.ExternalPACURL = ""
	sett.NetworkServices = []string{"Wi-Fi"}
	if got := app.pacMatchCandidates(sett); len(got) != 1 || got[0] != local {
		t.Errorf("没有外部 PAC 时应当只剩本地 PAC，实际 %v", got)
	}
}

// TestSwitchToOffModeClearsPACEntry covers the same class of bug through the
// other route: switching the mode to "off" while the proxy stays enabled goes
// through reconcile's "off" branch, not through forceOff. Both must hand the
// helper a PAC URL it can recognise, otherwise macOS keeps using a PAC file
// served by a server we are about to stop.
func TestSwitchToOffModeClearsPACEntry(t *testing.T) {
	app, store, sett := newTestApp(t)
	if err := writeFileAtomic(store.PACFilePath(), []byte(`var rules = ["a"];`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := app.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = app.SetEnabled(false) }()
	if err := app.SetMode(config.ModeAuto); err != nil {
		t.Fatal(err)
	}

	h := stubHelper(t, store, []string{"Wi-Fi"})
	if err := app.SetMode(config.ModeOff); err != nil {
		t.Fatalf("切换到关闭代理失败: %v", err)
	}
	if len(h.calls) != 1 {
		t.Fatalf("切换模式应当调用助手一次，实际 %d 次", len(h.calls))
	}
	if h.arg(0, "--mode") != "off" {
		t.Fatalf("模式应为 off，实际 %q", h.arg(0, "--mode"))
	}
	// 关键：切换模式时 sett.Mode 已经是 off，不能靠"当前模式"去推 PAC 地址，
	// 必须用刚才实际安装的那个。
	want := "http://127.0.0.1:" + strconv.Itoa(sett.PAC.Port) + "/proxy.pac"
	if got := h.arg(0, "--pac-url"); got != want {
		t.Errorf("切换到 off 时必须带上 PAC 地址 %q，实际 %q", want, got)
	}
	if !h.hasValue(0, "--network-service", "Wi-Fi") {
		t.Error("切换到 off 时应当带上需要清理的网络服务")
	}
	if app.pacSrv.Running() {
		t.Error("切换到 off 后 PAC 服务器应当停止")
	}
}
