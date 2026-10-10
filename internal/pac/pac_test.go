package pac

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// gfwlistFixture mimics the real file: base64 with 64 column wrapping and a
// trailing newline, which is why unknown characters have to be tolerated.
func gfwlistFixture() []byte {
	plain := strings.Join([]string{
		"[AutoProxy 0.2.9]",
		"! this is a comment",
		"",
		"||example.com",
		"|http://blocked.test/path",
		"|@|doubled.test",
		"ads.example.org",
		"",
	}, "\n")
	encoded := base64.StdEncoding.EncodeToString([]byte(plain))
	var wrapped strings.Builder
	for i := 0; i < len(encoded); i += 64 {
		end := i + 64
		if end > len(encoded) {
			end = len(encoded)
		}
		wrapped.WriteString(encoded[i:end])
		wrapped.WriteString("\n")
	}
	return []byte(wrapped.String())
}

func TestDecodeGFWList(t *testing.T) {
	lines, err := DecodeGFWList(gfwlistFixture())
	if err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	want := []string{"[AutoProxy 0.2.9]", "! this is a comment", "", "||example.com", "|http://blocked.test/path", "|@|doubled.test", "ads.example.org", ""}
	if len(lines) != len(want) {
		t.Fatalf("行数不符: %d != %d (%q)", len(lines), len(want), lines)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Errorf("第 %d 行不符: %q != %q", i, lines[i], want[i])
		}
	}
}

func TestDecodeGFWListPlainText(t *testing.T) {
	lines, err := DecodeGFWList([]byte("! comment\n||plain.example.com\n"))
	if err != nil {
		t.Fatalf("明文列表解码失败: %v", err)
	}
	if len(lines) < 2 || lines[1] != "||plain.example.com" {
		t.Fatalf("明文列表解析错误: %q", lines)
	}
}

func TestDecodeGFWListRejectsGarbage(t *testing.T) {
	if _, err := DecodeGFWList([]byte("!!!not base64 at all###")); err == nil {
		t.Fatal("无效数据应当报错")
	}
}

func TestMergeRulesDropsUserDuplicates(t *testing.T) {
	gfw := []string{"||example.com", "|http://blocked.test/path", "|@|doubled.test", "keep.test"}
	user := []string{"!my rules", "http://blocked.test/path", ""}
	lines := MergeRules(gfw, user)

	// The user's own copy stays (it is prepended), but the downloaded duplicate
	// must not be added a second time.
	if n := count(lines, "http://blocked.test/path"); n != 1 {
		t.Errorf("用户规则应只出现一次，实际 %d 次: %q", n, lines)
	}
	// "||example.com" stays because the user only wrote "@|example.com".
	if count(lines, "||example.com") != 1 {
		t.Errorf("不应丢弃 ||example.com: %q", lines)
	}
	// The leading '@'/'|' run is stripped before the comparison, so a user rule
	// "doubled.test" suppresses the downloaded "|@|doubled.test".
	lines = MergeRules([]string{"|@|doubled.test"}, []string{"doubled.test"})
	if len(lines) != 1 || lines[0] != "doubled.test" {
		t.Errorf("去掉 @| 前缀后应命中用户规则: %q", lines)
	}
	// A comment in the user file still suppresses the matching downloaded rule,
	// because upstream compares before filtering comments out.
	lines = MergeRules([]string{"||commented.test"}, []string{"!||commented.test", "||commented.test"})
	if len(lines) != 1 {
		t.Errorf("用户注释不应影响去重结果: %q", lines)
	}
	// Comments and blank lines disappear from the output but user rules stay first.
	if lines := MergeRules(gfw, []string{"!c", "", "user.test"}); lines[0] != "user.test" {
		t.Errorf("用户规则应排在最前: %q", lines)
	}
}

func TestGenerate(t *testing.T) {
	res, err := Generate(Options{
		SocksHost: "127.0.0.1",
		SocksPort: 1086,
		GFWList:   gfwlistFixture(),
		UserRules: []byte("!mine\nuser-only.test\n"),
	})
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	js := string(res.Content)
	if strings.Contains(js, "__RULES__") || strings.Contains(js, "__SOCKS5PORT__") || strings.Contains(js, "__SOCKS5ADDR__") {
		t.Fatal("占位符未被替换")
	}
	if !strings.Contains(js, `"SOCKS5 127.0.0.1:1086; SOCKS 127.0.0.1:1086; DIRECT;"`) {
		t.Fatalf("代理行不正确:\n%s", firstProxyLine(js))
	}
	if res.RuleCount != 5 {
		t.Errorf("规则数错误: %d", res.RuleCount)
	}
	if res.UserCount != 1 || res.GFWCount != 4 {
		t.Errorf("规则来源统计错误: user=%d gfw=%d", res.UserCount, res.GFWCount)
	}
	if got := RuleCountFromPAC(res.Content); got != res.RuleCount {
		t.Errorf("PAC 内规则数不一致: %d != %d", got, res.RuleCount)
	}
	// The generated file must name the local SOCKS5 endpoint the client is
	// listening on; line 118 pins the full proxy line, this keeps the endpoint
	// itself asserted independently of the surrounding syntax.
	if !strings.Contains(js, "SOCKS5 127.0.0.1:1086") {
		t.Errorf("PAC 文件没有指向本地 SOCKS5 端点:\n%s", firstProxyLine(js))
	}
	// The rules array must be valid JSON so that browsers can parse the file.
	var rules []string
	if err := json.Unmarshal([]byte(extractRulesArray(t, js)), &rules); err != nil {
		t.Fatalf("规则不是合法 JSON: %v", err)
	}
	if len(rules) != res.RuleCount || rules[0] != "user-only.test" {
		t.Errorf("规则内容错误: %q", rules)
	}
}

func TestGenerateIPv6Host(t *testing.T) {
	res, err := Generate(Options{SocksHost: "::1", SocksPort: 1086})
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	if !strings.Contains(string(res.Content), "SOCKS5 [::1]:1086") {
		t.Fatalf("IPv6 地址未加方括号")
	}
}

func TestGenerateRejectsBadPort(t *testing.T) {
	if _, err := Generate(Options{SocksHost: "127.0.0.1", SocksPort: 0}); err == nil {
		t.Fatal("端口 0 应当报错")
	}
}

func TestGenerateWithNoRules(t *testing.T) {
	res, err := Generate(Options{SocksHost: "127.0.0.1", SocksPort: 1086})
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	if res.RuleCount != 0 {
		t.Errorf("空规则集应生成 0 条规则，实际 %d", res.RuleCount)
	}
	if !strings.Contains(string(res.Content), "var rules = []") {
		t.Errorf("空规则数组格式不正确")
	}
}

func TestValidateURL(t *testing.T) {
	for _, ok := range []string{"", "http://127.0.0.1:8090/proxy.pac", "https://example.com/p.pac", "file:///tmp/x.pac"} {
		if err := ValidateURL(ok); err != nil {
			t.Errorf("%q 应当被接受: %v", ok, err)
		}
	}
	for _, bad := range []string{"127.0.0.1:8090/proxy.pac", "ftp://example.com/x.pac", "javascript:alert(1)"} {
		if err := ValidateURL(bad); err == nil {
			t.Errorf("%q 应当被拒绝", bad)
		}
	}
}

func TestServerServesAndRefreshes(t *testing.T) {
	content := []byte("var rules = [];\n")
	srv := NewServer(func() ([]byte, error) { return content, nil }, nil)
	if err := srv.Start("127.0.0.1:0"); err != nil {
		t.Fatalf("启动失败: %v", err)
	}
	defer srv.Stop()

	resp, err := http.Get(srv.URL())
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != string(content) {
		t.Fatalf("响应异常: %d %q", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Content-Type"); got != ContentType {
		t.Errorf("Content-Type 错误: %q", got)
	}
	if resp.Header.Get("Cache-Control") == "" {
		t.Error("缺少缓存控制头")
	}

	// The file is re-read on every request, so an update shows up at once.
	content = []byte("var rules = [\"x\"];\n")
	resp2, err := http.Get(srv.URL() + "?v=123")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	body2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if !strings.Contains(string(body2), "\"x\"") {
		t.Fatalf("PAC 内容未刷新: %q", body2)
	}
	if srv.Hits() != 2 {
		t.Errorf("命中计数错误: %d", srv.Hits())
	}
	if got := ParseCacheBustedURL(srv.URL() + "?v=123"); strings.Contains(got, "v=") {
		t.Errorf("缓存破坏参数未被剥离: %q", got)
	}
}

func TestServerStopReleasesPort(t *testing.T) {
	srv := NewServer(func() ([]byte, error) { return []byte("x"), nil }, nil)
	if err := srv.Start("127.0.0.1:0"); err != nil {
		t.Fatalf("启动失败: %v", err)
	}
	addr := strings.TrimPrefix(srv.URL(), "http://")
	addr = strings.TrimSuffix(addr, "/proxy.pac")
	srv.Stop()
	if srv.Running() {
		t.Fatal("停止后仍在运行")
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("端口未释放: %v", err)
	}
	_ = ln.Close()
	// Stop is idempotent, and a stopped server reports no URL.
	srv.Stop()
	if srv.URL() != "" {
		t.Errorf("停止后仍返回 URL: %s", srv.URL())
	}
}

func TestServerHandlesMissingFile(t *testing.T) {
	srv := NewServer(func() ([]byte, error) { return nil, io.ErrUnexpectedEOF }, nil)
	if err := srv.Start("127.0.0.1:0"); err != nil {
		t.Fatalf("启动失败: %v", err)
	}
	defer srv.Stop()
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(srv.URL())
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("缺少 PAC 文件时应返回 503，实际 %d", resp.StatusCode)
	}
	if srv.LastError() == "" {
		t.Error("应当记录最近错误")
	}
}

func count(lines []string, want string) int {
	n := 0
	for _, l := range lines {
		if l == want {
			n++
		}
	}
	return n
}

func extractRulesArray(t *testing.T, js string) string {
	t.Helper()
	const marker = "var rules = "
	i := strings.Index(js, marker)
	if i < 0 {
		t.Fatal("找不到规则数组")
	}
	rest := js[i+len(marker):]
	end := strings.Index(rest, "];")
	if end < 0 {
		t.Fatal("规则数组没有结束")
	}
	return rest[:end+1]
}

func firstProxyLine(js string) string {
	for _, line := range strings.Split(js, "\n") {
		if strings.HasPrefix(line, "var proxy") {
			return line
		}
	}
	return ""
}
