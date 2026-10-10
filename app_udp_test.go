package main

import (
	"net"
	"os"
	"strings"
	"testing"

	"ssxng/internal/config"
)

// TestApplySettingsRestartsEngineForUDP checks the switch actually takes
// effect. UDPEnabled decides whether the engine binds the relay, so changing it
// without restarting would leave the checkbox doing nothing until the next
// launch - a silent failure the user cannot diagnose from the UI.
func TestApplySettingsRestartsEngineForUDP(t *testing.T) {
	app, _, _ := newTestApp(t)

	// Turn the proxy on the way the UI does; ApplySettings only restarts the
	// engine when a listener changed, so it cannot be the thing that starts it.
	// Enabled must still be false here, otherwise SetEnabled short-circuits.
	if err := app.SetEnabled(true); err != nil {
		t.Fatalf("启用代理失败: %v", err)
	}
	if !app.engine.Running() {
		t.Fatal("启用后引擎应在运行")
	}
	view := toSettingsView(app.settings())
	if app.engine.UDPAddr() == "" {
		t.Fatal("UDPEnabled 默认为真，引擎应已绑定 UDP 中继")
	}

	// Turn UDP off through the settings view and confirm the engine rebound
	// without it.
	view.UDPEnabled = false
	if err := app.ApplySettings(view); err != nil {
		t.Fatalf("关闭 UDP 失败: %v", err)
	}
	if !app.engine.Running() {
		t.Fatal("关闭 UDP 后引擎应仍在运行")
	}
	if got := app.engine.UDPAddr(); got != "" {
		t.Fatalf("关闭 UDP 后引擎不应再绑定 UDP 中继，实际 %q", got)
	}
	if st := app.buildState(); st.UDP.Active {
		t.Fatal("关闭 UDP 后状态页不应报告 UDP 已启用")
	}

	// And back on again.
	view.UDPEnabled = true
	if err := app.ApplySettings(view); err != nil {
		t.Fatalf("重新开启 UDP 失败: %v", err)
	}
	addr := app.engine.UDPAddr()
	if addr == "" {
		t.Fatal("重新开启 UDP 后引擎应绑定 UDP 中继")
	}
	// The relay must be reachable: binding the address we advertise is the only
	// way a client could ever use it.
	pc, err := net.ListenPacket("udp", addr)
	if err == nil {
		_ = pc.Close()
		t.Fatalf("UDP 中继地址 %s 竟然可以被别人再次绑定，说明引擎没有真正占用它", addr)
	}

	st := app.buildState()
	if !st.UDP.Active {
		t.Fatal("开启 UDP 后状态页应报告 UDP 已启用")
	}
	if st.UDP.Port == 0 || st.UDP.Address == "" {
		t.Fatalf("状态页的 UDP 监听地址不完整: %s:%d", st.UDP.Address, st.UDP.Port)
	}
	// It shares the SOCKS port, which is what an app is already configured for.
	if st.UDP.Port != st.Socks.Port {
		t.Fatalf("UDP 应与 SOCKS 共用端口，实际 UDP %d / SOCKS %d", st.UDP.Port, st.Socks.Port)
	}
}

// TestApplySettingsKeepsUDPOffWhenDisabled is the other half: a disabled relay
// must never be bound, and the reason has to be visible in the state.
func TestApplySettingsKeepsUDPOffWhenDisabled(t *testing.T) {
	app, store, _ := newTestApp(t)
	// Persist UDP off while the proxy is still stopped, then start. Read the live
	// settings rather than the fixture: newTestApp selects the current server
	// *after* it returns its snapshot, and SaveSettings would clobber that.
	live := store.Settings()
	live.UDPEnabled = false
	if err := store.SaveSettings(live); err != nil {
		t.Fatal(err)
	}
	if err := app.SetEnabled(true); err != nil {
		t.Fatalf("启用代理失败: %v", err)
	}
	if got := app.engine.UDPAddr(); got != "" {
		t.Fatalf("UDP 关闭时不应绑定中继，实际 %q", got)
	}

	st := app.buildState()
	if st.UDP.Active {
		t.Error("UDP 关闭时状态页不应报告已启用")
	}
	// The address is still shown (it is where UDP would listen), which is what
	// makes the setting understandable, but it must not claim to be active.
	if st.UDP.Port != st.Socks.Port {
		t.Errorf("UDP 关闭时也应显示与 SOCKS 相同的端口，实际 %d", st.UDP.Port)
	}
	if st.Settings.UDPEnabled {
		t.Error("SettingsView 应回显 UDPEnabled=false，否则界面会显示成已开启")
	}
}

// TestUDPSettingDefaultsToOnForExistingConfigs guards backward compatibility:
// config.json written before the field existed must not silently disable UDP.
func TestUDPSettingDefaultsToOnForExistingConfigs(t *testing.T) {
	dir := t.TempDir()
	store, err := config.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !store.Settings().UDPEnabled {
		t.Fatal("默认设置应启用 UDP")
	}

	// Simulate an older file: rewrite config.json without the key, which is what
	// a pre-UDP version would have left behind.
	sett := store.Settings()
	if err := store.SaveSettings(sett); err != nil {
		t.Fatal(err)
	}
	raw := readFileString(t, store.ConfigPath())
	if !strings.Contains(raw, "udpEnabled") {
		t.Fatal("保存后的 config.json 应包含 udpEnabled")
	}
	stripped := strings.Replace(raw, "\"udpEnabled\": true,", "", 1)
	stripped = strings.Replace(stripped, "\"udpEnabled\": true", "", 1)
	if stripped == raw {
		t.Skip("config.json 的字段顺序与预期不同，跳过这条针对旧文件的断言")
	}
	writeFileString(t, store.ConfigPath(), stripped)

	reopened, err := config.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reopened.Settings().UDPEnabled {
		t.Fatal("旧配置文件缺少 udpEnabled 时应保持启用，而不是被静默关掉")
	}
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func writeFileString(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
