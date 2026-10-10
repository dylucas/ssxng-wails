package main

import (
	"os"
	"path/filepath"
	"testing"

	"ssxng/internal/config"
)

// The menu bar copies the same text the window's 命令行代理 block shows. These
// strings are the contract between the two: scripts/ui-check/measure.js pins
// the very same literals for the window, so a change to either side has to be
// made in both places (and in both tests).
const (
	wantHTTPCommand  = "export http_proxy=http://127.0.0.1:1087;export https_proxy=http://127.0.0.1:1087;"
	wantSOCKSCommand = "export http_proxy=socks5://127.0.0.1:1086;export https_proxy=socks5://127.0.0.1:1086;"
)

// saveSettings applies a mutation to the stored settings, failing the test if
// validation rejects the result.
func saveSettings(t *testing.T, store *config.Store, mutate func(*config.Settings)) config.Settings {
	t.Helper()
	sett := store.Settings()
	mutate(&sett)
	if err := store.SaveSettings(sett); err != nil {
		t.Fatalf("保存设置: %v", err)
	}
	return sett
}

func TestTrayProxyCommandsMatchTheWindow(t *testing.T) {
	app, store, _ := newTestApp(t)
	saveSettings(t, store, func(s *config.Settings) {
		s.SOCKS = config.Listener{Address: "127.0.0.1", Port: 1086}
		s.HTTP.Enabled = true
		s.HTTP.Address = "127.0.0.1"
		s.HTTP.Port = 1087
	})

	got := app.trayProxyCommands()
	if len(got) != 2 {
		t.Fatalf("HTTP 开启时应给出两条命令，实际 %d 条", len(got))
	}
	if got[0].Key != "http" || got[0].Command != wantHTTPCommand {
		t.Errorf("HTTP 命令 = %q，期望 %q", got[0].Command, wantHTTPCommand)
	}
	if got[1].Key != "socks" || got[1].Command != wantSOCKSCommand {
		t.Errorf("SOCKS5 命令 = %q，期望 %q", got[1].Command, wantSOCKSCommand)
	}
	for _, c := range got {
		if c.Title == "" {
			t.Errorf("%s 命令缺少菜单标题", c.Key)
		}
	}
}

// Turning the HTTP listener off must drop its menu item, exactly like the
// window drops its row: the command would point at a port nothing listens on.
func TestTrayProxyCommandsDropHTTPWhenDisabled(t *testing.T) {
	app, store, _ := newTestApp(t)
	saveSettings(t, store, func(s *config.Settings) {
		s.SOCKS = config.Listener{Address: "127.0.0.1", Port: 1086}
		s.HTTP.Enabled = false
		s.HTTP.Address = "127.0.0.1"
		s.HTTP.Port = 1087
	})

	got := app.trayProxyCommands()
	if len(got) != 1 || got[0].Key != "socks" || got[0].Command != wantSOCKSCommand {
		t.Fatalf("HTTP 关闭时应只剩 SOCKS5 命令，实际 %+v", got)
	}
}

// The commands track saved settings, not a draft, and custom ports/addresses
// flow through verbatim.
func TestTrayProxyCommandsFollowSavedSettings(t *testing.T) {
	app, store, _ := newTestApp(t)
	saveSettings(t, store, func(s *config.Settings) {
		s.SOCKS = config.Listener{Address: "127.0.0.1", Port: 2080}
		s.HTTP.Enabled = true
		s.HTTP.Address = "localhost"
		s.HTTP.Port = 2081
	})

	got := app.trayProxyCommands()
	want := []string{
		"export http_proxy=http://localhost:2081;export https_proxy=http://localhost:2081;",
		"export http_proxy=socks5://127.0.0.1:2080;export https_proxy=socks5://127.0.0.1:2080;",
	}
	if len(got) != len(want) {
		t.Fatalf("命令条数 = %d，期望 %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].Command != w {
			t.Errorf("第 %d 条 = %q，期望 %q", i+1, got[i].Command, w)
		}
	}
}

// A hand-edited config.json is loaded straight into Settings without validation
// (see config.Open), so a blank address and a missing port really can reach the
// menu. The command must still be usable instead of reading "://" with no host.
func TestTrayProxyCommandsFallBackOnEmptyAddress(t *testing.T) {
	dir := t.TempDir()
	raw := `{"version":1,"enabled":false,"mode":"global",
		"socks":{"address":"","port":0},
		"http":{"enabled":true,"address":"  ","port":0},
		"pac":{"address":"127.0.0.1","port":1089}}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := config.Open(dir)
	if err != nil {
		t.Fatalf("打开配置: %v", err)
	}
	app := &App{store: store}

	got := app.trayProxyCommands()
	if len(got) != 2 {
		t.Fatalf("命令条数 = %d，期望 2", len(got))
	}
	if got[0].Command != wantHTTPCommand || got[1].Command != wantSOCKSCommand {
		t.Fatalf("地址为空时应回落到 127.0.0.1:1087/1086，实际 %q / %q", got[0].Command, got[1].Command)
	}
}

// The snapshot is what the menu actually renders, so the commands must travel
// with it (they are rebuilt on every state change, not cached at startup).
func TestTrayStateCarriesCommandsAndModes(t *testing.T) {
	app, store, _ := newTestApp(t)
	saveSettings(t, store, func(s *config.Settings) {
		s.SOCKS = config.Listener{Address: "127.0.0.1", Port: 1086}
		s.HTTP.Enabled = true
		s.HTTP.Address = "127.0.0.1"
		s.HTTP.Port = 1087
		s.Enabled = true
	})

	snap := app.trayState()
	if snap.Title != "SSXNG" {
		t.Errorf("标题 = %q", snap.Title)
	}
	if !snap.Enabled {
		t.Error("快照应报告代理已开启")
	}
	if len(snap.Modes) == 0 {
		t.Error("快照应带上可切换的模式")
	}
	if len(snap.Commands) != 2 || snap.Commands[0].Command != wantHTTPCommand {
		t.Fatalf("快照应带上两条复制命令，实际 %+v", snap.Commands)
	}
}

// The status item picks its icon (filled vs hollow) from this flag, so it has to
// follow the stored setting on every refresh — including after SetEnabled rolls
// the state back when the proxy fails to start.
func TestTrayStateEnabledFollowsSettings(t *testing.T) {
	app, store, _ := newTestApp(t)

	saveSettings(t, store, func(s *config.Settings) { s.Enabled = false })
	if snap := app.trayState(); snap.Enabled {
		t.Error("代理关闭时快照不该报告已开启")
	}
	// A stopped proxy whose mode is still "global" is the normal off state.
	if snap := app.trayState(); snap.Status != "已关闭" {
		t.Errorf("代理关闭时状态行 = %q，期望 %q", snap.Status, "已关闭")
	}

	saveSettings(t, store, func(s *config.Settings) { s.Enabled = true })
	snap := app.trayState()
	if !snap.Enabled {
		t.Fatal("代理开启时快照必须报告已开启（否则图标不会变实心）")
	}
	if snap.Status == "已关闭" {
		t.Error("代理开启时状态行不该是“已关闭”")
	}
}
