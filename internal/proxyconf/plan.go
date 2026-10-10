package proxyconf

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Command is a single networksetup invocation: the arguments after the binary
// path. Using argv slices (never a shell) is what keeps the setuid helper safe.
type Command []string

// String renders the command for logs.
func (c Command) String() string {
	parts := make([]string, 0, len(c))
	for _, a := range c {
		if strings.ContainsAny(a, " \t") {
			parts = append(parts, fmt.Sprintf("%q", a))
		} else {
			parts = append(parts, a)
		}
	}
	return NetworksetupPath + " " + strings.Join(parts, " ")
}

// Match decides whether the proxy configuration on a service looks like it was
// written by us. This mirrors ShadowsocksX-NG's proxy_conf_helper: the PAC URL
// (when enabled) or the SOCKS host/port (when enabled) have to match. We
// deliberately do not treat a matching HTTP proxy as "ours" because 127.0.0.1
// with our default port could belong to another client.
type Match struct {
	PACURL    string
	SocksHost string
	SocksPort int
}

// Matches reports whether st was most likely written by this client.
func Matches(st ProxyState, m Match) bool {
	if st.AutoEnabled && m.PACURL != "" && PACURLEqual(st.AutoURL, m.PACURL) {
		return true
	}
	if st.SocksEnabled && sameHost(st.SocksHost, m.SocksHost) && st.SocksPort == m.SocksPort {
		return true
	}
	return false
}

// PACURLEqual compares two PAC URLs ignoring the cache-busting query string we
// append and trailing slashes.
func PACURLEqual(a, b string) bool {
	norm := func(s string) string {
		s = strings.TrimSpace(s)
		if i := strings.IndexByte(s, '?'); i >= 0 {
			s = s[:i]
		}
		s = strings.TrimSuffix(s, "/")
		return strings.ToLower(s)
	}
	return a != "" && b != "" && norm(a) == norm(b)
}

func sameHost(a, b string) bool {
	return strings.EqualFold(strings.Trim(strings.TrimSpace(a), "[]"), strings.Trim(strings.TrimSpace(b), "[]"))
}

// PlanGlobal builds the commands for "global" mode: point the system web,
// secure web and SOCKS proxies at our local listeners.
func PlanGlobal(services []string, socksHost string, socksPort int, httpHost string, httpPort int, httpEnabled bool, bypass []string) []Command {
	var cmds []Command
	for _, svc := range services {
		cmds = append(cmds,
			Command{"-setautoproxystate", svc, "off"},
			Command{"-setproxyautodiscovery", svc, "off"},
		)
		if httpEnabled {
			cmds = append(cmds,
				Command{"-setwebproxy", svc, httpHost, itoa(httpPort), "off"},
				Command{"-setsecurewebproxy", svc, httpHost, itoa(httpPort), "off"},
				Command{"-setwebproxystate", svc, "on"},
				Command{"-setsecurewebproxystate", svc, "on"},
			)
		} else {
			cmds = append(cmds,
				Command{"-setwebproxystate", svc, "off"},
				Command{"-setsecurewebproxystate", svc, "off"},
			)
		}
		cmds = append(cmds,
			Command{"-setsocksfirewallproxy", svc, socksHost, itoa(socksPort), "off"},
			Command{"-setsocksfirewallproxystate", svc, "on"},
			Command{"-setproxybypassdomains", svc, bypassArg(bypass)},
		)
	}
	return cmds
}

// PlanAuto builds the commands for "auto" (PAC) mode: clear the per-protocol
// proxies and let the PAC file decide.
//
// Unlike ShadowsocksX-NG we do not wipe the bypass domain list here; it is
// irrelevant while a PAC file is in use and keeping it avoids destroying a
// user's own exceptions.
func PlanAuto(services []string, pacURL string) []Command {
	var cmds []Command
	for _, svc := range services {
		cmds = append(cmds,
			Command{"-setwebproxystate", svc, "off"},
			Command{"-setsecurewebproxystate", svc, "off"},
			Command{"-setsocksfirewallproxystate", svc, "off"},
			Command{"-setproxyautodiscovery", svc, "off"},
			Command{"-setautoproxyurl", svc, pacURL},
			Command{"-setautoproxystate", svc, "on"},
		)
	}
	return cmds
}

// PlanOffFor builds the commands that clear the proxy on a single service.
// restore, when non-nil, is the bypass domain list to put back.
func PlanOffFor(service string, restore []string) []Command {
	cmds := []Command{
		{"-setwebproxystate", service, "off"},
		{"-setsecurewebproxystate", service, "off"},
		{"-setsocksfirewallproxystate", service, "off"},
		{"-setautoproxystate", service, "off"},
		{"-setproxyautodiscovery", service, "off"},
	}
	if restore != nil {
		cmds = append(cmds, Command{"-setproxybypassdomains", service, bypassArg(restore)})
	}
	return cmds
}

func bypassArg(bypass []string) string {
	if len(bypass) == 0 {
		return "Empty"
	}
	return strings.Join(bypass, " ")
}

func itoa(i int) string { return fmt.Sprintf("%d", i) }

// RunAll executes the commands in order, best effort, and reports the first
// error while still attempting the rest (a single failing service must not
// leave the others unconfigured).
func RunAll(cmds []Command) error {
	var firstErr error
	for _, c := range cmds {
		if len(c) == 0 {
			continue
		}
		if _, err := run(NetworksetupPath, c...); err != nil {
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// Snapshot remembers, per service, the bypass domain list that existed before
// we touched it, so "off" can put the user's own exceptions back.
type Snapshot map[string][]string

// LoadSnapshot reads a snapshot file, returning an empty snapshot when the
// file does not exist yet.
func LoadSnapshot(path string) (Snapshot, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return Snapshot{}, nil
	}
	if err != nil {
		return nil, err
	}
	snap := Snapshot{}
	if err := json.Unmarshal(raw, &snap); err != nil {
		// A corrupt snapshot is not fatal - the caller starts over - but it is
		// reported so that the user learns the previous proxy state is gone.
		return Snapshot{}, fmt.Errorf("代理状态快照已损坏，将重新记录: %w", err)
	}
	return snap, nil
}

// Save writes the snapshot atomically.
func (s Snapshot) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Record stores the bypass list of service unless we already have one, or the
// list is empty (nothing worth restoring).
func (s Snapshot) Record(service string, bypass []string) {
	if _, ok := s[service]; ok {
		return // only the first (pristine) state is kept
	}
	if bypass == nil {
		// Store an empty but non-nil slice: "this service had no bypass
		// domains" has to round-trip through JSON as [] rather than null, so
		// that turning the proxy off can restore an empty list. Skipping the
		// record instead would leave our own exceptions installed forever.
		bypass = []string{}
	}
	s[service] = bypass
}

// Take returns the recorded bypass list for service and forgets it.
func (s Snapshot) Take(service string) []string {
	out, ok := s[service]
	if !ok {
		return nil
	}
	delete(s, service)
	return out
}

// ValidateServiceName makes sure a service name came from the system rather
// than from user input.
func ValidateServiceName(name string, services []Service) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("网络服务名不能为空")
	}
	if strings.HasPrefix(name, "-") {
		return fmt.Errorf("网络服务名不合法: %q", name)
	}
	for _, s := range services {
		if s.Name == name {
			return nil
		}
	}
	return fmt.Errorf("系统中不存在网络服务 %q", name)
}
