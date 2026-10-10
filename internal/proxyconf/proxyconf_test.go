package proxyconf

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeNetworksetup stubs the networksetup binary.
type fakeNetworksetup struct {
	responses map[string]string
	errs      map[string]error
	calls     []string
}

func (f *fakeNetworksetup) run(_ string, args ...string) (string, error) {
	key := strings.Join(args, " ")
	f.calls = append(f.calls, key)
	if err := f.errs[key]; err != nil {
		return "", err
	}
	out, ok := f.responses[key]
	if !ok {
		return "", fmt.Errorf("unexpected networksetup call: %s", key)
	}
	return out, nil
}

func (f *fakeNetworksetup) install(t *testing.T) {
	t.Helper()
	prev := runCommand
	runCommand = f.run
	t.Cleanup(func() { runCommand = prev })
}

func cmdStr(c Command) string { return strings.Join(c, " ") }

const listServiceOrderOutput = `An asterisk (*) denotes that a network service is disabled.
(1) Wi-Fi
(Hardware Port: Wi-Fi, Device: en0)

(2) Thunderbolt Bridge
(Hardware Port: Thunderbolt Bridge, Device: bridge0)

(*) iPhone USB
(Hardware Port: iPhone USB, Device: en5)
`

func TestListServices(t *testing.T) {
	f := &fakeNetworksetup{responses: map[string]string{
		"-listnetworkserviceorder": listServiceOrderOutput,
	}}
	f.install(t)

	services, err := ListServices()
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(services) != 3 {
		t.Fatalf("服务数量错误: %d (%+v)", len(services), services)
	}
	want := []Service{
		{Name: "Wi-Fi", HardwarePort: "Wi-Fi", Device: "en0", Enabled: true},
		{Name: "Thunderbolt Bridge", HardwarePort: "Thunderbolt Bridge", Device: "bridge0", Enabled: true},
		{Name: "iPhone USB", HardwarePort: "iPhone USB", Device: "en5", Enabled: false},
	}
	for i, w := range want {
		if services[i] != w {
			t.Errorf("第 %d 项不符:\n got %+v\nwant %+v", i, services[i], w)
		}
	}

	// Only hardware ports that are real network interfaces are auto-selected.
	auto := AutoSelected(services)
	if len(auto) != 1 || auto[0] != "Wi-Fi" {
		t.Errorf("自动选择结果错误: %v", auto)
	}
}

func TestListServicesReportsError(t *testing.T) {
	f := &fakeNetworksetup{
		responses: map[string]string{"-listnetworkserviceorder": ""},
		errs:      map[string]error{"-listnetworkserviceorder": fmt.Errorf("networksetup 不可用")},
	}
	f.install(t)
	if _, err := ListServices(); err == nil {
		t.Fatal("应当返回错误")
	}
}

func TestReadState(t *testing.T) {
	f := &fakeNetworksetup{responses: map[string]string{
		"-getwebproxy Wi-Fi":           "Enabled: No\nServer: 127.0.0.1\nPort: 1087\nAuthenticated Proxy Enabled: 0\n",
		"-getsecurewebproxy Wi-Fi":     "Enabled: Yes\nServer: 127.0.0.1\nPort: 1087\n",
		"-getsocksfirewallproxy Wi-Fi": "Enabled: Yes\nServer: 127.0.0.1\nPort: 1086\n",
		"-getautoproxyurl Wi-Fi":       "URL: http://127.0.0.1:8090/proxy.pac\nEnabled: No\n",
		"-getproxybypassdomains Wi-Fi": "127.0.0.1\nlocalhost\n*.cn\nfc00::/7\n",
		"-getproxyautodiscovery Wi-Fi": "Automatic Proxy Discovery: Off\n",
	}}
	f.install(t)

	st, err := ReadState("Wi-Fi")
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if st.WebEnabled || !st.SecureEnabled || !st.SocksEnabled {
		t.Errorf("启用状态解析错误: %+v", st)
	}
	if st.SocksHost != "127.0.0.1" || st.SocksPort != 1086 {
		t.Errorf("SOCKS 解析错误: %s:%d", st.SocksHost, st.SocksPort)
	}
	if st.WebHost != "127.0.0.1" || st.WebPort != 1087 {
		t.Errorf("HTTP 解析错误: %s:%d", st.WebHost, st.WebPort)
	}
	if st.AutoURL != "http://127.0.0.1:8090/proxy.pac" || st.AutoEnabled {
		t.Errorf("PAC 解析错误: %q %v", st.AutoURL, st.AutoEnabled)
	}
	if len(st.Bypass) != 4 || st.Bypass[2] != "*.cn" {
		t.Errorf("例外列表解析错误: %v", st.Bypass)
	}
	if st.AutoDiscovery {
		t.Errorf("自动发现解析错误: %v", st.AutoDiscovery)
	}
}

// TestReadStateToleratesFailures covers networksetup replying "** Error" for a
// service that has never had an auto proxy URL configured.
func TestReadStateToleratesFailures(t *testing.T) {
	f := &fakeNetworksetup{
		responses: map[string]string{
			"-getwebproxy Wi-Fi":           "** Error: The parameters were not valid.\n",
			"-getsecurewebproxy Wi-Fi":     "Enabled: No\nServer: \nPort: 0\n",
			"-getsocksfirewallproxy Wi-Fi": "Enabled: No\nServer: \nPort: 0\n",
			"-getautoproxyurl Wi-Fi":       "URL: (null)\nEnabled: No\n",
			"-getproxybypassdomains Wi-Fi": "There aren't any bypass domains set on Wi-Fi.\n",
			"-getproxyautodiscovery Wi-Fi": "Automatic Proxy Discovery: Off\n",
		},
		errs: map[string]error{
			"-getwebproxy Wi-Fi": fmt.Errorf("networksetup: ** Error"),
		},
	}
	f.install(t)

	st, err := ReadState("Wi-Fi")
	if err != nil {
		t.Fatalf("局部失败不应整体失败: %v", err)
	}
	if st.WebEnabled || st.SocksEnabled || st.AutoEnabled {
		t.Errorf("失败项应按未启用处理: %+v", st)
	}
	if len(st.Bypass) != 0 {
		t.Errorf("无例外时应返回空列表: %v", st.Bypass)
	}
	if st.AutoURL != "" {
		t.Errorf("(null) 应被当作空地址: %q", st.AutoURL)
	}
}

func TestReadAllStates(t *testing.T) {
	f := &fakeNetworksetup{responses: map[string]string{}}
	for _, svc := range []string{"Wi-Fi", "Thunderbolt Bridge"} {
		f.responses["-getwebproxy "+svc] = "Enabled: No\nServer: \nPort: 0\n"
		f.responses["-getsecurewebproxy "+svc] = "Enabled: No\nServer: \nPort: 0\n"
		f.responses["-getsocksfirewallproxy "+svc] = "Enabled: No\nServer: \nPort: 0\n"
		f.responses["-getautoproxyurl "+svc] = "URL: \nEnabled: No\n"
		f.responses["-getproxybypassdomains "+svc] = "There aren't any bypass domains set on " + svc + ".\n"
		f.responses["-getproxyautodiscovery "+svc] = "Automatic Proxy Discovery: Off\n"
	}
	f.install(t)

	states := ReadAllStates([]Service{{Name: "Wi-Fi"}, {Name: "Thunderbolt Bridge"}})
	if len(states) != 2 {
		t.Fatalf("状态数量错误: %d", len(states))
	}
	if _, ok := states["Wi-Fi"]; !ok {
		t.Error("缺少 Wi-Fi 状态")
	}
}

func TestParseProxy(t *testing.T) {
	cases := []struct {
		out     string
		enabled bool
		host    string
		port    int
	}{
		{"Enabled: Yes\nServer: 127.0.0.1\nPort: 1086\n", true, "127.0.0.1", 1086},
		{"Enabled: No\nServer: 127.0.0.1\nPort: 1086\n", false, "127.0.0.1", 1086},
		{"Enabled: Yes\nServer: \nPort: \n", true, "", 0},
		{"** Error: The parameters were not valid.\n", false, "", 0},
		{"", false, "", 0},
	}
	for _, c := range cases {
		enabled, host, port := parseProxy(c.out)
		if enabled != c.enabled || host != c.host || port != c.port {
			t.Errorf("parseProxy(%q) = %v,%q,%d; want %v,%q,%d", c.out, enabled, host, port, c.enabled, c.host, c.port)
		}
	}
}

func TestParseAutoProxyURL(t *testing.T) {
	for _, c := range []struct {
		out     string
		enabled bool
		url     string
	}{
		{"URL: http://127.0.0.1:8090/proxy.pac\nEnabled: Yes\n", true, "http://127.0.0.1:8090/proxy.pac"},
		{"URL: http://127.0.0.1:8090/proxy.pac\nEnabled: No\n", false, "http://127.0.0.1:8090/proxy.pac"},
		{"URL: (null)\nEnabled: No\n", false, ""},
		{"Enabled: No\n", false, ""},
	} {
		enabled, u := parseAutoProxyURL(c.out)
		if enabled != c.enabled || u != c.url {
			t.Errorf("parseAutoProxyURL(%q) = %v,%q; want %v,%q", c.out, enabled, u, c.enabled, c.url)
		}
	}
}

func TestParseBypass(t *testing.T) {
	if got := parseBypass("There aren't any bypass domains set on Wi-Fi.\n"); len(got) != 0 {
		t.Errorf("无例外时应为空: %v", got)
	}
	got := parseBypass("127.0.0.1\nlocalhost\n\n  *.cn  \n")
	want := []string{"127.0.0.1", "localhost", "*.cn"}
	if len(got) != len(want) {
		t.Fatalf("解析结果错误: %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 项错误: %q != %q", i, got[i], want[i])
		}
	}
}

func TestPlanGlobal(t *testing.T) {
	cmds := PlanGlobal([]string{"Wi-Fi"}, "127.0.0.1", 1086, "127.0.0.1", 1087, true, []string{"127.0.0.1", "localhost"})
	var got []string
	for _, c := range cmds {
		got = append(got, cmdStr(c))
	}
	joined := strings.Join(got, "\n")
	for _, want := range []string{
		"-setautoproxystate Wi-Fi off",
		"-setproxyautodiscovery Wi-Fi off",
		"-setwebproxy Wi-Fi 127.0.0.1 1087 off",
		"-setsecurewebproxy Wi-Fi 127.0.0.1 1087 off",
		"-setwebproxystate Wi-Fi on",
		"-setsecurewebproxystate Wi-Fi on",
		"-setsocksfirewallproxy Wi-Fi 127.0.0.1 1086 off",
		"-setsocksfirewallproxystate Wi-Fi on",
		"-setproxybypassdomains Wi-Fi 127.0.0.1 localhost",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("缺少命令 %q\n实际:\n%s", want, joined)
		}
	}
	// The PAC URL must never be set in global mode.
	if strings.Contains(joined, "-setautoproxyurl") {
		t.Error("全局模式不应设置 PAC 地址")
	}
	// No arguments may be empty (an empty argv element would be passed through).
	for _, c := range cmds {
		for i, a := range c {
			if strings.TrimSpace(a) == "" {
				t.Errorf("命令 %v 的第 %d 个参数为空", c, i)
			}
		}
	}
}

func TestPlanGlobalWithoutHTTP(t *testing.T) {
	cmds := PlanGlobal([]string{"Wi-Fi"}, "127.0.0.1", 1086, "", 0, false, nil)
	joined := strings.Join(func() []string {
		var out []string
		for _, c := range cmds {
			out = append(out, cmdStr(c))
		}
		return out
	}(), "\n")
	if !strings.Contains(joined, "-setwebproxystate Wi-Fi off") {
		t.Error("HTTP 关闭时应显式关闭 web 代理")
	}
	if strings.Contains(joined, "-setwebproxy ") {
		t.Error("HTTP 关闭时不应设置 web 代理地址")
	}
	// An empty bypass list must be passed as the literal "Empty" keyword.
	if !strings.Contains(joined, "-setproxybypassdomains Wi-Fi Empty") {
		t.Errorf("空例外列表应使用 Empty 关键字:\n%s", joined)
	}
}

func TestPlanAutoDoesNotTouchBypass(t *testing.T) {
	cmds := PlanAuto([]string{"Wi-Fi"}, "http://127.0.0.1:8090/proxy.pac?v=42")
	for _, c := range cmds {
		if c[0] == "-setproxybypassdomains" {
			t.Fatalf("PAC 模式不应改写例外列表: %v", c)
		}
	}
	joined := strings.Join(func() []string {
		var out []string
		for _, c := range cmds {
			out = append(out, cmdStr(c))
		}
		return out
	}(), "\n")
	if !strings.Contains(joined, "-setautoproxyurl Wi-Fi http://127.0.0.1:8090/proxy.pac?v=42") {
		t.Errorf("未设置 PAC 地址:\n%s", joined)
	}
	if !strings.Contains(joined, "-setautoproxystate Wi-Fi on") {
		t.Error("未启用 PAC")
	}
}

func TestPlanOffFor(t *testing.T) {
	cmds := PlanOffFor("Wi-Fi", []string{"*.cn", "localhost"})
	joined := strings.Join(func() []string {
		var out []string
		for _, c := range cmds {
			out = append(out, cmdStr(c))
		}
		return out
	}(), "\n")
	for _, want := range []string{
		"-setwebproxystate Wi-Fi off",
		"-setsecurewebproxystate Wi-Fi off",
		"-setsocksfirewallproxystate Wi-Fi off",
		"-setautoproxystate Wi-Fi off",
		"-setproxyautodiscovery Wi-Fi off",
		"-setproxybypassdomains Wi-Fi *.cn localhost",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("缺少命令 %q\n实际:\n%s", want, joined)
		}
	}
	// A nil restore list must leave the bypass domains alone.
	for _, c := range PlanOffFor("Wi-Fi", nil) {
		if c[0] == "-setproxybypassdomains" {
			t.Fatal("没有快照时不应改写例外列表")
		}
	}
}

func TestMatches(t *testing.T) {
	m := Match{PACURL: "http://127.0.0.1:8090/proxy.pac", SocksHost: "127.0.0.1", SocksPort: 1086}
	cases := []struct {
		name string
		st   ProxyState
		want bool
	}{
		{"PAC enabled", ProxyState{AutoEnabled: true, AutoURL: "http://127.0.0.1:8090/proxy.pac"}, true},
		{"PAC with cache buster", ProxyState{AutoEnabled: true, AutoURL: "http://127.0.0.1:8090/proxy.pac?v=9"}, true},
		{"PAC disabled", ProxyState{AutoEnabled: false, AutoURL: "http://127.0.0.1:8090/proxy.pac"}, false},
		{"other PAC", ProxyState{AutoEnabled: true, AutoURL: "http://example.com/x.pac"}, false},
		{"SOCKS enabled", ProxyState{SocksEnabled: true, SocksHost: "127.0.0.1", SocksPort: 1086}, true},
		{"SOCKS wrong port", ProxyState{SocksEnabled: true, SocksHost: "127.0.0.1", SocksPort: 1087}, false},
		{"SOCKS disabled", ProxyState{SocksHost: "127.0.0.1", SocksPort: 1086}, false},
		// A matching HTTP proxy alone is not enough: 127.0.0.1:1087 could be
		// another client's listener.
		{"HTTP only", ProxyState{WebEnabled: true, WebHost: "127.0.0.1", WebPort: 1087}, false},
		{"nothing", ProxyState{}, false},
	}
	for _, c := range cases {
		if got := Matches(c.st, m); got != c.want {
			t.Errorf("%s: Matches = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestPACURLEqual(t *testing.T) {
	a := "http://127.0.0.1:8090/proxy.pac"
	for _, b := range []string{
		"http://127.0.0.1:8090/proxy.pac",
		"http://127.0.0.1:8090/proxy.pac/",
		"http://127.0.0.1:8090/proxy.pac?v=12345",
		" HTTP://127.0.0.1:8090/proxy.pac ",
	} {
		if !PACURLEqual(a, b) {
			t.Errorf("%q 应视为相同", b)
		}
	}
	if PACURLEqual(a, "http://127.0.0.1:8091/proxy.pac") {
		t.Error("不同端口不应视为相同")
	}
	if PACURLEqual("", "") {
		t.Error("两个空地址不应视为相同")
	}
}

func TestValidateServiceName(t *testing.T) {
	services := []Service{{Name: "Wi-Fi"}, {Name: "Thunderbolt Bridge"}}
	if err := ValidateServiceName("Wi-Fi", services); err != nil {
		t.Errorf("已知服务应当通过: %v", err)
	}
	if err := ValidateServiceName("Evil; rm -rf /", services); err == nil {
		t.Error("未知服务名应当被拒绝")
	}
	if err := ValidateServiceName("", services); err == nil {
		t.Error("空服务名应当被拒绝")
	}
}

func TestSnapshotRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "proxy-state.json")

	snap, err := LoadSnapshot(path)
	if err != nil {
		t.Fatalf("加载空快照失败: %v", err)
	}
	if len(snap) != 0 {
		t.Fatalf("空快照应为空: %v", snap)
	}
	snap.Record("Wi-Fi", []string{"*.cn", "localhost"})
	snap.Record("Thunderbolt Bridge", nil)
	if err := snap.Save(path); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("快照权限应为 0600，实际 %o", perm)
	}

	reloaded, err := LoadSnapshot(path)
	if err != nil {
		t.Fatalf("重新加载失败: %v", err)
	}
	if got := reloaded.Take("Wi-Fi"); len(got) != 2 || got[0] != "*.cn" {
		t.Errorf("快照内容错误: %v", got)
	}
	// Taking removes the entry, so a second off run cannot restore stale values.
	if got := reloaded.Take("Wi-Fi"); got != nil {
		t.Errorf("取走后应为空: %v", got)
	}
	// An explicitly recorded empty list must survive as an empty (not nil) slice
	// so that "no exceptions" is restored faithfully.
	if got := reloaded.Take("Thunderbolt Bridge"); got == nil || len(got) != 0 {
		t.Errorf("空例外列表应被保留: %#v", got)
	}
}

func TestSnapshotCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "proxy-state.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSnapshot(path); err == nil {
		t.Fatal("损坏的快照应当报错")
	}
}

func TestRunAllStopsReportingFirstError(t *testing.T) {
	err := RunAll([]Command{{"-nonexistent-flag", "x"}})
	if err == nil {
		t.Fatal("非法命令应当返回错误")
	}
}

func TestFormatPort(t *testing.T) {
	if got := FormatPort("127.0.0.1", 1086); got != "127.0.0.1:1086" {
		t.Errorf("端口格式化错误: %q", got)
	}
	if got := FormatPort("::1", 1086); got != "::1:1086" {
		t.Errorf("IPv6 端口格式化错误: %q", got)
	}
}

func TestVersionAtLeast(t *testing.T) {
	cases := []struct {
		have, want string
		ok         bool
	}{
		{"1.0.0", "1.0.0", true},
		{"1.0.1", "1.0.0", true},
		{"1.1.0", "1.0.9", true},
		{"2.0.0", "1.9.9", true},
		{"0.9.9", "1.0.0", false},
		{"1.0.0", "1.0.1", false},
		{"", "1.0.0", false},
		{"dev", "1.0.0", false},
	}
	for _, c := range cases {
		if got := VersionAtLeast(c.have, c.want); got != c.ok {
			t.Errorf("VersionAtLeast(%q, %q) = %v, want %v", c.have, c.want, got, c.ok)
		}
	}
}

func TestShellQuoting(t *testing.T) {
	// The installer hands a shell command to osascript, so a service name or a
	// path must not be able to break out of the quoting. Verify by execution
	// rather than by pattern matching.
	inputs := []string{"plain", "with space", "quote'inside", `"double"`, "$(whoami)", "`id`", "semi;colon", "back\\slash"}
	for _, in := range inputs {
		quoted := shellQuote(in)
		out, err := exec.Command("/bin/sh", "-c", "printf %s "+quoted).Output()
		if err != nil {
			t.Errorf("shellQuote(%q) = %q 执行失败: %v", in, quoted, err)
			continue
		}
		if string(out) != in {
			t.Errorf("shellQuote(%q) = %q 还原为 %q", in, quoted, out)
		}
	}
	if err := exec.Command("/bin/sh", "-c", "true "+shellQuote("x; rm -rf /tmp/ssxng-should-not-exist")).Run(); err != nil {
		t.Errorf("带分号的字符串未被安全引用: %v", err)
	}
}

func TestAppleScriptQuote(t *testing.T) {
	for _, in := range []string{`plain`, `a"b`, `a\b`, `a"b\c`, "line\nbreak", "服务 Wi-Fi"} {
		got := appleScriptQuote(in)
		if len(got) < 2 || got[0] != '"' || got[len(got)-1] != '"' {
			t.Fatalf("appleScriptQuote(%q) = %q，应使用双引号包裹", in, got)
		}
		// Inside the quotes every backslash must introduce an escaped character.
		body := got[1 : len(got)-1]
		for i := 0; i < len(body); i++ {
			switch body[i] {
			case '\\':
				i++
				if i >= len(body) {
					t.Fatalf("appleScriptQuote(%q) = %q，结尾有多余的反斜杠", in, got)
				}
			case '"':
				t.Fatalf("appleScriptQuote(%q) = %q，存在未转义的双引号", in, got)
			}
		}
	}
}
