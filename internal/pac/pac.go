// Package pac turns a gfwlist into a Proxy Auto-Config file.
//
// The pipeline is the one ShadowsocksX-NG uses (see docs/reference/PACUtils.swift):
// gfwlist.txt is base64, user-rule.txt is prepended, rules already provided by
// the user are dropped from the downloaded list, empty lines and comments are
// removed, and the result is embedded into the Adblock-Plus derived abp.js
// template. abp.js is vendored verbatim (GPL-3.0) with its placeholders intact.
package pac

import (
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
)

//go:embed abp.js
var abpTemplate string

// Placeholders in abp.js.
const (
	placeholderRules = "__RULES__"
	placeholderAddr  = "__SOCKS5ADDR__"
	placeholderPort  = "__SOCKS5PORT__"
)

// Options describes one PAC generation run.
type Options struct {
	SocksHost string // SOCKS5 listener address written into the PAC proxy line
	SocksPort int
	GFWList   []byte // raw contents of gfwlist.txt (base64), may be nil
	UserRules []byte // raw contents of user-rule.txt, may be nil
}

// Result is the outcome of a generation run.
type Result struct {
	Content   []byte
	RuleCount int // rules embedded in the PAC file
	GFWCount  int // rules that came from the downloaded list
	UserCount int // rules that came from user-rule.txt
}

// Generate renders the PAC file.
func Generate(o Options) (Result, error) {
	var res Result
	host := o.SocksHost
	if host == "" {
		host = "127.0.0.1"
	}
	if o.SocksPort < 1 || o.SocksPort > 65535 {
		return res, fmt.Errorf("SOCKS5 端口 %d 不合法", o.SocksPort)
	}

	gfwLines, err := DecodeGFWList(o.GFWList)
	if err != nil {
		return res, err
	}
	userLines := SplitLines(string(o.UserRules))

	lines := MergeRules(gfwLines, userLines)
	res.RuleCount = len(lines)
	res.UserCount = countPresent(userLines, lines)
	res.GFWCount = res.RuleCount - res.UserCount

	rulesJSON, err := json.MarshalIndent(lines, "", "  ")
	if err != nil {
		return res, fmt.Errorf("规则序列化失败: %w", err)
	}

	for _, ph := range []string{placeholderRules, placeholderPort, placeholderAddr} {
		if !strings.Contains(abpTemplate, ph) {
			return res, fmt.Errorf("PAC 模板缺少占位符 %s", ph)
		}
	}
	js := abpTemplate
	js = strings.ReplaceAll(js, placeholderRules, string(rulesJSON))
	js = strings.ReplaceAll(js, placeholderPort, fmt.Sprint(o.SocksPort))
	js = strings.ReplaceAll(js, placeholderAddr, quoteIPv6(host))
	res.Content = []byte(js)
	return res, nil
}

// DecodeGFWList decodes the base64 payload of gfwlist.txt into rule lines.
//
// Apple's Data(base64Encoded:options:.ignoreUnknownCharacters) tolerates
// whitespace, so leading/trailing newlines from editors and CDNs are stripped
// before decoding. A plain text list is accepted too, since some mirrors serve
// it uncompressed.
func DecodeGFWList(raw []byte) ([]string, error) {
	text := string(raw)
	if strings.TrimSpace(text) == "" {
		return nil, nil
	}
	compact := stripWhitespace(text)
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding,
		base64.URLEncoding, base64.RawURLEncoding,
	} {
		if decoded, err := enc.DecodeString(compact); err == nil {
			return SplitLines(string(decoded)), nil
		}
	}
	if looksLikePlainList(text) {
		return SplitLines(text), nil
	}
	return nil, errors.New("gfwlist 数据不是有效的 base64")
}

// MergeRules prepends the user rules to the downloaded rules, dropping any
// downloaded rule the user already provides.
//
// The comparison mirrors PACUtils.swift: the leading run of '@' and '|' is
// stripped from the downloaded rule before looking it up in the user list, so
// that a user rule "example.com" also suppresses "||example.com". Blank lines
// and comments ('!' or '[') are removed last, so that a commented out user rule
// still takes part in the comparison, exactly as upstream does.
func MergeRules(gfw, user []string) []string {
	userSet := make(map[string]struct{}, len(user))
	for _, line := range user {
		userSet[line] = struct{}{}
	}

	out := make([]string, 0, len(gfw)+len(user))
	out = append(out, user...)
	for _, line := range gfw {
		trimmed := strings.TrimLeft(line, "@|")
		if _, dup := userSet[line]; dup {
			continue
		}
		if trimmed != line {
			if _, dup := userSet[trimmed]; dup {
				continue
			}
		}
		out = append(out, line)
	}
	return filterRules(out)
}

// filterRules drops empty lines and comments.
func filterRules(lines []string) []string {
	out := lines[:0:0]
	for _, line := range lines {
		s := strings.TrimSpace(line)
		if s == "" || strings.HasPrefix(s, "!") || strings.HasPrefix(s, "[") {
			continue
		}
		out = append(out, s)
	}
	return out
}

// SplitLines splits text into trimmed lines, accepting CRLF input.
func SplitLines(text string) []string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

func countPresent(subset, all []string) int {
	set := make(map[string]struct{}, len(all))
	for _, s := range all {
		set[s] = struct{}{}
	}
	n := 0
	for _, s := range subset {
		if _, ok := set[s]; ok {
			n++
		}
	}
	return n
}

func quoteIPv6(host string) string {
	if ip := net.ParseIP(host); ip != nil && ip.To4() == nil {
		return "[" + host + "]"
	}
	return host
}

func stripWhitespace(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\n', '\r', '\v', '\f':
			return -1
		}
		return r
	}, s)
}

// looksLikePlainList reports whether the text is an undecoded rule list rather
// than base64 garbage. A list has at least one line that is shaped like a rule:
// no spaces, and either a domain label, a "|" anchor or an "@@" exception.
func looksLikePlainList(text string) bool {
	for _, line := range SplitLines(text) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "!") || strings.HasPrefix(line, "[") {
			continue
		}
		if strings.ContainsAny(line, " \t#") {
			continue
		}
		if strings.HasPrefix(line, "|") || strings.HasPrefix(line, "@@") || strings.Contains(line, ".") {
			return true
		}
	}
	return false
}

// ValidateURL checks a user supplied PAC URL. ShadowsocksX-NG accepts file,
// http and https URLs (see docs/reference/PACURLFormatter.swift).
func ValidateURL(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	scheme := raw
	if i := strings.Index(raw, ":"); i >= 0 {
		scheme = strings.ToLower(raw[:i])
	} else {
		return errors.New("PAC 地址必须包含 http、https 或 file 前缀")
	}
	switch scheme {
	case "http", "https", "file":
		return nil
	default:
		return fmt.Errorf("PAC 地址必须使用 http、https 或 file，当前为 %q", scheme)
	}
}
