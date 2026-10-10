package diagnose

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ssxng/internal/config"
	"ssxng/internal/proxyconf"
)

// TestMain disables go-shadowsocks2's process-global salt replay filter, without
// which the in-process Shadowsocks server of the self test rejects this client's
// own salts as replays. The application disables it for the same reason (see
// main.go).
func TestMain(m *testing.M) {
	_ = os.Setenv("SHADOWSOCKS_SF_CAPACITY", "-1")
	os.Exit(m.Run())
}

func testContext(t *testing.T) Context {
	t.Helper()
	dir := t.TempDir()
	sett := config.DefaultSettings()
	sett.Enabled = true
	sett.Mode = config.ModeGlobal
	return Context{
		ConfigDir:    dir,
		Settings:     sett,
		Servers:      []config.ServerProfile{{ID: "a", Host: "127.0.0.1", Port: 1, Method: "aes-256-gcm", Password: "x"}},
		SocksAddr:    "127.0.0.1:1086",
		HTTPAddr:     "127.0.0.1:1087",
		HTTPEnabled:  true,
		PACAddr:      "127.0.0.1:1089",
		PACFile:      filepath.Join(dir, "gfwlist.js"),
		GFWListFile:  filepath.Join(dir, "gfwlist.txt"),
		UserRuleFile: filepath.Join(dir, "user-rule.txt"),
		LogFile:      filepath.Join(dir, "ssxng.log"),
		Version:      "test",
	}
}

func TestRunProducesSensibleChecks(t *testing.T) {
	ctx := testContext(t)
	ctx.Helper = proxyconf.HelperStatusInfo{Installed: false, RequiredVersion: proxyconf.HelperVersion}
	checks := Run(ctx)
	if len(checks) < 6 {
		t.Fatalf("检查项太少: %d", len(checks))
	}
	byName := map[string]Check{}
	for _, c := range checks {
		byName[c.Name] = c
		if c.Status != StatusOK && c.Status != StatusWarn && c.Status != StatusFail && c.Status != StatusInfo {
			t.Errorf("%s 的状态不合法: %q", c.Name, c.Status)
		}
	}
	if c, ok := byName["配置目录"]; !ok || c.Status != StatusOK {
		t.Errorf("配置目录应为 ok: %+v", c)
	}
	if c, ok := byName["特权助手"]; !ok || c.Status != StatusWarn {
		t.Errorf("未安装助手应为 warn: %+v", c)
	}
	// Nothing has been generated yet, so the PAC checks must say so rather than
	// claiming success.
	if c, ok := byName["PAC 文件"]; ok && c.Status == StatusOK {
		t.Errorf("PAC 文件尚未生成时不应为 ok: %+v", c)
	}
	if c, ok := byName["GFWList 数据"]; !ok || c.Status != StatusInfo {
		t.Errorf("未下载规则应为 info: %+v", c)
	}

	// With a helper installed and a valid PAC file the checks turn green.
	ctx.Helper = proxyconf.HelperStatusInfo{Installed: true, VersionOK: true, Setuid: true, Path: "/tmp/x", Version: proxyconf.HelperVersion}
	if err := os.WriteFile(ctx.PACFile, []byte(`var rules = ["a","b","c"];`), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx.Settings.Mode = config.ModeAuto
	ctx.PACRunning = true
	checks = Run(ctx)
	byName = map[string]Check{}
	for _, c := range checks {
		byName[c.Name] = c
	}
	if c := byName["PAC 文件"]; c.Status != StatusOK {
		t.Errorf("PAC 文件应为 ok: %+v", c)
	}
	if c := byName["特权助手"]; c.Status != StatusOK {
		t.Errorf("助手应为 ok: %+v", c)
	}
}

func TestRunSurfacesUnsupportedMethod(t *testing.T) {
	ctx := testContext(t)
	srv := config.ServerProfile{ID: "a", Host: "127.0.0.1", Port: 1, Method: "rc4-md5", Password: "x"}
	ctx.Servers = []config.ServerProfile{srv}
	ctx.CurrentServer = &srv
	checks := Run(ctx)
	for _, c := range checks {
		if c.Name == "服务器" && c.Status != StatusFail {
			t.Errorf("不支持的加密方式应为 fail: %+v", c)
		}
	}
}

func TestSelfTestRunsEndToEnd(t *testing.T) {
	ctx := testContext(t)
	report := SelfTest(ctx)
	if !report.OK {
		for _, s := range report.Steps {
			t.Errorf("失败步骤 %s: %s", s.Name, s.Detail)
		}
		t.Fatalf("自检未通过（耗时 %d ms）", report.ElapsedMS)
	}
	if len(report.Steps) < 6 {
		t.Fatalf("自检步骤太少: %d", len(report.Steps))
	}
	names := make([]string, 0, len(report.Steps))
	for _, s := range report.Steps {
		names = append(names, s.Name)
		if !s.OK || s.Detail == "" {
			t.Errorf("步骤 %s 未通过或缺少细节: %+v", s.Name, s)
		}
	}
	joined := strings.Join(names, ",")
	for _, want := range []string{"SOCKS5", "HTTP", "CONNECT", "大数据", "清理"} {
		if !strings.Contains(joined, want) {
			t.Errorf("自检缺少步骤 %q（实际: %s）", want, joined)
		}
	}
}

func TestSelfTestFailsOnUnwritableDir(t *testing.T) {
	ctx := testContext(t)
	ctx.ConfigDir = filepath.Join(ctx.ConfigDir, "missing", "deeper")
	// A deeply nested path that the test process cannot create is hard to fake
	// portably; instead use a path under a file, which can never be a directory.
	file := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx.ConfigDir = filepath.Join(file, "sub")
	report := SelfTest(ctx)
	if report.OK {
		t.Fatal("配置目录不可写时自检应当失败")
	}
	if len(report.Steps) == 0 || report.Steps[0].OK {
		t.Fatalf("第一个步骤应当失败: %+v", report.Steps)
	}
}
