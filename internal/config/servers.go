package config

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strings"
)

// ServerProfile is one Shadowsocks node. The plugin fields are kept so that an
// imported ss:// URL round-trips losslessly, but this client does not run
// SIP003 plugins (the UI warns about it).
type ServerProfile struct {
	ID            string `json:"id"`
	Host          string `json:"host"`
	Port          int    `json:"port"`
	Method        string `json:"method"`
	Password      string `json:"password"`
	Remark        string `json:"remark"`
	Plugin        string `json:"plugin,omitempty"`
	PluginOptions string `json:"pluginOptions,omitempty"`
}

// SupportedMethods lists the ciphers this client can speak, lowercase.
// aes-192-gcm is the same AEAD construction with a 24 byte key. The legacy
// stream ciphers (aes-*-cfb, rc4-md5) and the 2022 methods
// (2022-blake3-aes-*-gcm) are deliberately not supported - see README.
var SupportedMethods = []string{
	"aes-128-gcm",
	"aes-192-gcm",
	"aes-256-gcm",
	"chacha20-ietf-poly1305",
}

// CanonicalMethod normalises a user supplied method name and reports whether
// it is supported. Aliases mirror go-shadowsocks2's spelling.
func CanonicalMethod(name string) (string, bool) {
	n := strings.ToLower(strings.TrimSpace(name))
	switch n {
	case "aead_aes_128_gcm", "aes128-gcm", "aes-128-gcm":
		n = "aes-128-gcm"
	case "aead_aes_192_gcm", "aes192-gcm", "aes-192-gcm":
		n = "aes-192-gcm"
	case "aead_aes_256_gcm", "aes256-gcm", "aes-256-gcm":
		n = "aes-256-gcm"
	case "aead_chacha20_poly1305", "chacha20-poly1305", "chacha20-ietf-poly1305":
		n = "chacha20-ietf-poly1305"
	}
	for _, m := range SupportedMethods {
		if m == n {
			return n, true
		}
	}
	return n, false
}

// NewID returns a random 128-bit hex identifier.
func NewID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand never fails on supported platforms; fall back to a fixed
		// prefix so we never hand out an empty id.
		return "0000000000000000"
	}
	return hex.EncodeToString(b[:])
}

// Validate checks the fields needed to actually dial this node.
func (p *ServerProfile) Validate() error {
	p.Host = strings.TrimSpace(p.Host)
	if p.Host == "" {
		return errors.New("服务器地址不能为空")
	}
	if strings.ContainsAny(p.Host, " \t/") {
		return fmt.Errorf("服务器地址 %q 不合法", p.Host)
	}
	if p.Port < 1 || p.Port > 65535 {
		return fmt.Errorf("服务器端口必须在 1-65535 之间，当前为 %d", p.Port)
	}
	if net.ParseIP(p.Host) == nil && !strings.Contains(p.Host, ".") && !strings.Contains(p.Host, ":") {
		return fmt.Errorf("服务器地址 %q 看起来不是域名或 IP", p.Host)
	}
	m, ok := CanonicalMethod(p.Method)
	if !ok {
		return fmt.Errorf("不支持的加密方式 %q（支持：%s）", p.Method, strings.Join(SupportedMethods, ", "))
	}
	p.Method = m
	if p.Password == "" {
		return errors.New("密码不能为空")
	}
	if p.ID == "" {
		p.ID = NewID()
	}
	return nil
}

// Title returns the label shown in lists and the menu bar.
func (p ServerProfile) Title() string {
	if r := strings.TrimSpace(p.Remark); r != "" {
		return r
	}
	return net.JoinHostPort(p.Host, fmt.Sprint(p.Port))
}
