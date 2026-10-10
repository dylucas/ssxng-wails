package ssurl

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestParseSIP002(t *testing.T) {
	userinfo := base64.RawURLEncoding.EncodeToString([]byte("aes-256-gcm:pass:word/with+chars"))
	p, err := Parse("ss://" + userinfo + "@example.com:8388#%E6%B5%8B%E8%AF%95%E8%8A%82%E7%82%B9")
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if p.Host != "example.com" || p.Port != 8388 {
		t.Errorf("主机端口错误: %s:%d", p.Host, p.Port)
	}
	if p.Method != "aes-256-gcm" {
		t.Errorf("加密方式错误: %q", p.Method)
	}
	if p.Password != "pass:word/with+chars" {
		t.Errorf("密码错误: %q", p.Password)
	}
	if p.Remark != "测试节点" {
		t.Errorf("备注错误: %q", p.Remark)
	}
}

func TestParseSIP002WithPlugin(t *testing.T) {
	userinfo := base64.RawURLEncoding.EncodeToString([]byte("aes-128-gcm:pw"))
	raw := "ss://" + userinfo + "@1.2.3.4:443/?plugin=obfs-local%3Bobfs%3Dhttp%3Bobfs-host%3Dexample.com#remark"
	p, err := Parse(raw)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if p.Plugin != "obfs-local" {
		t.Errorf("插件名错误: %q", p.Plugin)
	}
	if p.PluginOptions != "obfs=http;obfs-host=example.com" {
		t.Errorf("插件参数错误: %q", p.PluginOptions)
	}
}

func TestParseLegacy(t *testing.T) {
	payload := base64.StdEncoding.EncodeToString([]byte("aes-128-gcm:hunter2@192.168.1.10:1080"))
	p, err := Parse("ss://" + payload)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if p.Host != "192.168.1.10" || p.Port != 1080 || p.Method != "aes-128-gcm" || p.Password != "hunter2" {
		t.Fatalf("解析结果错误: %+v", p)
	}
}

func TestParseLegacyWithAtInPassword(t *testing.T) {
	payload := base64.RawURLEncoding.EncodeToString([]byte("chacha20-ietf-poly1305:p@ss@host.example.org:443"))
	p, err := Parse("ss://" + payload)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if p.Password != "p@ss" || p.Host != "host.example.org" || p.Port != 443 {
		t.Fatalf("解析结果错误: %+v", p)
	}
}

func TestParsePlainUserInfo(t *testing.T) {
	p, err := Parse("ss://aes-192-gcm:secret@[2001:db8::1]:9000")
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if p.Host != "2001:db8::1" || p.Port != 9000 || p.Method != "aes-192-gcm" {
		t.Fatalf("IPv6 解析错误: %+v", p)
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	for _, raw := range []string{
		"",
		"http://example.com",
		"ss://",
		"ss://not-base64@@",
		"ss://" + base64.RawURLEncoding.EncodeToString([]byte("no-colon@host:80")),
		"ss://" + base64.RawURLEncoding.EncodeToString([]byte("aes-128-gcm:pw@host")),
		"ss://" + base64.RawURLEncoding.EncodeToString([]byte("aes-128-gcm:pw@host:99999")),
	} {
		if _, err := Parse(raw); err == nil {
			t.Errorf("应当拒绝: %q", raw)
		}
	}
}

func TestRoundTrip(t *testing.T) {
	in := Profile{
		Host:          "node.example.com",
		Port:          8443,
		Method:        "aes-256-gcm",
		Password:      "p@ss:word/+=~!",
		Remark:        "香港 01 号",
		Plugin:        "v2ray-plugin",
		PluginOptions: "tls;host=cdn.example.com",
	}
	out, err := Parse(in.String())
	if err != nil {
		t.Fatalf("回环解析失败: %v (url=%s)", err, in.String())
	}
	if out.Host != in.Host || out.Port != in.Port || out.Method != in.Method ||
		out.Password != in.Password || out.Remark != in.Remark ||
		out.Plugin != in.Plugin || out.PluginOptions != in.PluginOptions {
		t.Fatalf("回环结果不一致:\n in=%+v\nout=%+v", in, out)
	}
}

func TestStringIsReadableSIP002(t *testing.T) {
	p := Profile{Host: "h.example.com", Port: 8388, Method: "aes-128-gcm", Password: "pw"}
	s := p.String()
	if !strings.HasPrefix(s, "ss://") || !strings.Contains(s, "@h.example.com:8388") {
		t.Fatalf("URL 形式异常: %s", s)
	}
	decoded, ok := b64Decode(strings.TrimPrefix(strings.Split(s, "@")[0], "ss://"))
	if !ok || decoded != "aes-128-gcm:pw" {
		t.Fatalf("userinfo 不是 SIP002 base64: %s", s)
	}
}
