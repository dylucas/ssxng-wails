package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultSettingsValid(t *testing.T) {
	sett := DefaultSettings()
	if err := sett.Validate(); err != nil {
		t.Fatalf("默认设置应当合法: %v", err)
	}
	if sett.Enabled {
		t.Error("默认不应自动开启代理")
	}
	if sett.Mode != ModeAuto {
		t.Errorf("默认模式应为 auto，实际 %q", sett.Mode)
	}
	if sett.SOCKS.Port != 1086 || sett.HTTP.Port != 1087 || sett.PAC.Port != 1089 {
		t.Errorf("默认端口不对: %+v %+v %+v", sett.SOCKS, sett.HTTP, sett.PAC)
	}
	if !sett.AutoConfigureNetworkServices {
		t.Error("默认应自动选择网络服务")
	}
}

func TestValidateRejectsBadValues(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Settings)
	}{
		{"未知模式", func(s *Settings) { s.Mode = "nope" }},
		{"端口为 0", func(s *Settings) { s.SOCKS.Port = 0 }},
		{"端口过大", func(s *Settings) { s.HTTP.Port = 70000 }},
		{"端口冲突", func(s *Settings) { s.SOCKS.Port = s.HTTP.Port }},
		{"非回环监听地址", func(s *Settings) { s.SOCKS.Address = "8.8.8.8" }},
		{"外部 PAC 未填写", func(s *Settings) {
			s.Mode = ModeExternalPAC
			s.PAC.ExternalPACURL = ""
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sett := DefaultSettings()
			tc.mut(&sett)
			if err := sett.Validate(); err == nil {
				t.Fatalf("%s 应当校验失败", tc.name)
			}
		})
	}
}

func TestValidateAcceptsExternalPAC(t *testing.T) {
	sett := DefaultSettings()
	sett.Mode = ModeExternalPAC
	sett.PAC.ExternalPACURL = "https://example.com/proxy.pac"
	if err := sett.Validate(); err != nil {
		t.Fatalf("合法的外部 PAC 地址不应报错: %v", err)
	}
	// The scheme itself is checked on the way to the privileged helper (see
	// apply.go desiredProxy and internal/pac.ValidateURL), not here.
}

func TestExceptionList(t *testing.T) {
	sett := DefaultSettings()
	got := sett.ExceptionList()
	if len(got) == 0 {
		t.Fatal("默认例外列表不应为空")
	}
	for _, ex := range got {
		if strings.TrimSpace(ex) != ex || ex == "" {
			t.Errorf("例外项未清理: %q", ex)
		}
	}

	// Spaces, commas, semicolons and new lines all separate entries; separator
	// leftovers are dropped.
	sett.Exceptions = "a.com, b.com;c.com\n d.com ;;\n、"
	want := []string{"a.com", "b.com", "c.com", "d.com"}
	got = sett.ExceptionList()
	if len(got) != len(want) {
		t.Fatalf("切分结果不对: %v", got)
	}
	if len(got) != len(want) {
		t.Fatalf("切分结果不对: %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 项应为 %q，实际 %q", i, want[i], got[i])
		}
	}

	// An empty setting still yields the built-in list rather than wiping the
	// system bypass domains.
	sett.Exceptions = "   "
	if len(sett.ExceptionList()) == 0 {
		t.Error("空设置应回落到默认例外列表")
	}
}

func TestStoreRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "SSXNG")
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{store.ConfigPath(), store.ServersPath()} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("Open 应当创建 %s: %v", p, err)
		}
	}

	sett := DefaultSettings()
	sett.Enabled = true
	sett.Mode = ModeGlobal
	sett.Exceptions = "example.com"
	sett.SwitchModes.Manual = true
	if err := store.SaveSettings(sett); err != nil {
		t.Fatal(err)
	}
	got := store.Settings()
	if !got.Enabled || got.Mode != ModeGlobal || got.Exceptions != "example.com" || !got.SwitchModes.Manual {
		t.Fatalf("设置没有往返: %+v", got)
	}

	// Reopening must read the same values back from disk.
	store2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got2 := store2.Settings(); got2.Mode != ModeGlobal || !got2.Enabled {
		t.Fatalf("重新打开后设置丢失: %+v", got2)
	}
}

func TestStoreServerLifecycle(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	first := ServerProfile{Host: "1.2.3.4", Port: 8388, Method: "aes-256-gcm", Password: "p", Remark: "香港节点"}
	first.ID = NewID()
	second := ServerProfile{Host: "5.6.7.8", Port: 443, Method: "chacha20-ietf-poly1305", Password: "q"}
	second.ID = NewID()
	if err := store.MutateServers(func([]ServerProfile) ([]ServerProfile, error) {
		return []ServerProfile{first, second}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if n := len(store.Servers()); n != 2 {
		t.Fatalf("应有 2 个服务器，实际 %d", n)
	}
	if srv, ok := store.ServerByID(first.ID); !ok || srv.Remark != "香港节点" {
		t.Fatalf("按 ID 查找失败: %+v %v", srv, ok)
	}
	if _, ok := store.ServerByID("不存在"); ok {
		t.Error("不存在的 ID 不应命中")
	}

	if err := store.SetCurrentServerID(second.ID); err != nil {
		t.Fatal(err)
	}
	cur, ok := store.CurrentServer()
	if !ok || cur.ID != second.ID {
		t.Fatalf("当前服务器不对: %+v", cur)
	}
	if err := store.SetCurrentServerID("不存在"); err == nil {
		t.Error("选中不存在的服务器应当报错")
	}
	// Selecting an empty ID is how "no current server" is stored.
	if err := store.SetCurrentServerID(""); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.CurrentServer(); ok {
		t.Error("清空后不应有当前服务器")
	}

	// Deleting the current server must not leave a dangling ID.
	if err := store.SetCurrentServerID(first.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.MutateServers(func(list []ServerProfile) ([]ServerProfile, error) {
		return []ServerProfile{list[1]}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if cur, ok := store.CurrentServer(); ok {
		t.Errorf("被删除的服务器仍是当前服务器: %+v", cur)
	}
}

func TestServerProfileValidate(t *testing.T) {
	base := ServerProfile{Host: "example.com", Port: 8388, Method: "aes-256-gcm", Password: "secret"}
	if err := base.Validate(); err != nil {
		t.Fatalf("合法配置不应报错: %v", err)
	}
	if base.Title() != "example.com:8388" {
		t.Errorf("标题应为 host:port，实际 %q", base.Title())
	}
	withRemark := base
	withRemark.Remark = " 东京 "
	if withRemark.Title() != "东京" {
		t.Errorf("有备注时应优先用备注，实际 %q", withRemark.Title())
	}
	upper := base
	upper.Method = "AES-256-GCM"
	if err := upper.Validate(); err != nil {
		t.Fatalf("大小写不同的加密方式应被接受: %v", err)
	}
	if m, ok := CanonicalMethod("AES-256-GCM"); !ok || m != "aes-256-gcm" {
		t.Errorf("CanonicalMethod 归一化失败: %q %v", m, ok)
	}
	if _, ok := CanonicalMethod("rc4-md5"); ok {
		t.Error("流式加密不应被接受")
	}

	for name, mut := range map[string]func(*ServerProfile){
		"空主机":   func(p *ServerProfile) { p.Host = "" },
		"端口为0":  func(p *ServerProfile) { p.Port = 0 },
		"端口过大":  func(p *ServerProfile) { p.Port = 65536 },
		"空密码":   func(p *ServerProfile) { p.Password = "" },
		"未知加密":  func(p *ServerProfile) { p.Method = "rot13" },
		"主机含空格": func(p *ServerProfile) { p.Host = "a b" },
	} {
		t.Run(name, func(t *testing.T) {
			p := base
			mut(&p)
			if err := p.Validate(); err == nil {
				t.Fatalf("%s 应当校验失败", name)
			}
		})
	}
}

func TestDefaultDirHonoursEnv(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SSXNG_CONFIG_DIR", dir)
	got, err := DefaultDir()
	if err != nil {
		t.Fatal(err)
	}
	if got != dir {
		t.Fatalf("应当使用 SSXNG_CONFIG_DIR，实际 %q", got)
	}
	t.Setenv("SSXNG_CONFIG_DIR", "")
	got, err = DefaultDir()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(got, filepath.Join("Application Support", "SSXNG")) {
		t.Fatalf("默认目录不对: %q", got)
	}
}

func TestConfigFileIsReadableJSON(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSettings(DefaultSettings()); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(store.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("配置不是合法 JSON: %v", err)
	}
	if parsed["socks"] == nil || parsed["pac"] == nil {
		t.Errorf("配置缺少关键字段: %s", raw)
	}
	// Passwords live in servers.json, which must not be world readable.
	info, err := os.Stat(store.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Errorf("配置文件权限过于宽松: %v", info.Mode().Perm())
	}
}
