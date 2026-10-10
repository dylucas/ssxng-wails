// Package ssurl parses and formats Shadowsocks URLs.
//
// Two dialects exist and both are widely deployed:
//
//	SIP002:  ss://base64url(method:password)@host:port/?plugin=...#remark
//	legacy:  ss://base64(method:password@host:port)#remark
//
// The plain "ss://method:password@host:port" form that some clients emit is
// accepted as well. Parsing mirrors ShadowsocksX-NG's ServerProfile.from(url:).
package ssurl

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// Profile is the parsed result.
type Profile struct {
	Host          string
	Port          int
	Method        string
	Password      string
	Remark        string
	Plugin        string
	PluginOptions string
}

// ErrNotSSURL is returned for input that is not a Shadowsocks URL.
var ErrNotSSURL = errors.New("不是有效的 ss:// 链接")

// Parse decodes a Shadowsocks URL.
func Parse(raw string) (Profile, error) {
	var p Profile
	s := strings.TrimSpace(raw)
	if s == "" {
		return p, ErrNotSSURL
	}
	// Tolerate QR scanners that strip or alter the scheme.
	if strings.HasPrefix(strings.ToLower(s), "ss://") {
		s = s[5:]
	} else if strings.Contains(s, "://") {
		return p, ErrNotSSURL
	}
	s = strings.TrimPrefix(s, "//")

	// Fragment carries the remark.
	if i := strings.IndexByte(s, '#'); i >= 0 {
		p.Remark = percentDecode(s[i+1:])
		s = s[:i]
	}
	// Query carries SIP002 plugin parameters.
	if i := strings.IndexByte(s, '?'); i >= 0 {
		query := s[i+1:]
		s = s[:i]
		if err := parsePluginQuery(query, &p); err != nil {
			return p, err
		}
	}
	s = strings.TrimSuffix(s, "/")
	if s == "" {
		return p, ErrNotSSURL
	}

	var userinfo, hostport string
	if at := strings.LastIndexByte(s, '@'); at >= 0 {
		userinfo, hostport = s[:at], s[at+1:]
		decoded, err := decodeUserInfo(userinfo)
		if err != nil {
			return p, err
		}
		userinfo = decoded
	} else {
		// Legacy: the whole payload is base64("method:password@host:port").
		decoded, ok := b64Decode(s)
		if !ok {
			return p, ErrNotSSURL
		}
		at := strings.LastIndexByte(decoded, '@')
		if at < 0 {
			return p, fmt.Errorf("%w: 缺少主机部分", ErrNotSSURL)
		}
		userinfo, hostport = decoded[:at], decoded[at+1:]
	}

	method, password, ok := strings.Cut(userinfo, ":")
	if !ok {
		return p, fmt.Errorf("%w: 缺少加密方式或密码", ErrNotSSURL)
	}
	p.Method = strings.TrimSpace(method)
	p.Password = password

	host, portStr, err := net.SplitHostPort(hostport)
	if err != nil {
		return p, fmt.Errorf("%w: 无法解析主机端口 %q", ErrNotSSURL, hostport)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return p, fmt.Errorf("%w: 端口 %q 不合法", ErrNotSSURL, portStr)
	}
	p.Port = port
	p.Host = strings.Trim(host, "[]")
	if p.Host == "" {
		return p, fmt.Errorf("%w: 主机为空", ErrNotSSURL)
	}
	return p, nil
}

// String renders the profile as a SIP002 URL.
func (p Profile) String() string {
	userinfo := base64.RawURLEncoding.EncodeToString([]byte(p.Method + ":" + p.Password))
	var b strings.Builder
	b.WriteString("ss://")
	b.WriteString(userinfo)
	b.WriteString("@")
	b.WriteString(net.JoinHostPort(p.Host, strconv.Itoa(p.Port)))
	var query []string
	if p.Plugin != "" {
		opts := p.Plugin
		if p.PluginOptions != "" {
			opts += ";" + p.PluginOptions
		}
		query = append(query, "plugin="+url.QueryEscape(opts))
	}
	if len(query) > 0 {
		b.WriteString("/?")
		b.WriteString(strings.Join(query, "&"))
	}
	if p.Remark != "" {
		b.WriteString("#")
		// Fragments are not form data: use path escaping so that a space
		// round-trips as %20 instead of turning into "+".
		b.WriteString(url.PathEscape(p.Remark))
	}
	return b.String()
}

// decodeUserInfo handles both SIP002 (base64) and plain userinfo.
func decodeUserInfo(userinfo string) (string, error) {
	userinfo = percentDecode(userinfo)
	if strings.Contains(userinfo, ":") {
		return userinfo, nil
	}
	decoded, ok := b64Decode(userinfo)
	if !ok || !strings.Contains(decoded, ":") {
		return "", fmt.Errorf("%w: 无法解析认证信息", ErrNotSSURL)
	}
	return decoded, nil
}

// b64Decode accepts URL-safe or standard base64, with or without padding.
func b64Decode(s string) (string, bool) {
	s = strings.TrimRight(s, "=")
	if s == "" {
		return "", false
	}
	for _, enc := range []*base64.Encoding{base64.RawURLEncoding, base64.RawStdEncoding} {
		if b, err := enc.DecodeString(s); err == nil {
			return string(b), true
		}
	}
	// Some exporters use standard base64 with padding inside the URL payload.
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return string(b), true
	}
	return "", false
}

func parsePluginQuery(query string, p *Profile) error {
	values, err := url.ParseQuery(query)
	if err != nil {
		return nil // ignore malformed plugin options, keep the rest
	}
	raw := values.Get("plugin")
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ";")
	p.Plugin = strings.TrimSpace(parts[0])
	if len(parts) > 1 {
		p.PluginOptions = strings.Join(parts[1:], ";")
	}
	return nil
}

func percentDecode(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	if dec, err := url.PathUnescape(s); err == nil {
		return dec
	}
	return s
}
