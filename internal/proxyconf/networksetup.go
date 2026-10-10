// Package proxyconf reads and writes the macOS system proxy configuration.
//
// Reading works as a normal user. Writing requires root, so the mutating paths
// are driven by the setuid helper (cmd/ssxng-helper) while the GUI uses the
// read helpers directly. Every command is assembled as an argv slice and
// executed without a shell.
package proxyconf

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// NetworksetupPath is the system tool we drive. All proxy mutations go through
// it because it is the only supported command line interface for
// SystemConfiguration proxy settings.
const NetworksetupPath = "/usr/sbin/networksetup"

// Service is one macOS network service (a "location" entry such as Wi-Fi).
type Service struct {
	Name         string `json:"name"`
	HardwarePort string `json:"hardwarePort"`
	Device       string `json:"device"`
	Enabled      bool   `json:"enabled"`
}

// ProxyState is the proxy configuration of a single service.
type ProxyState struct {
	WebEnabled    bool     `json:"webEnabled"`
	WebHost       string   `json:"webHost"`
	WebPort       int      `json:"webPort"`
	SecureEnabled bool     `json:"secureEnabled"`
	SecureHost    string   `json:"secureHost"`
	SecurePort    int      `json:"securePort"`
	SocksEnabled  bool     `json:"socksEnabled"`
	SocksHost     string   `json:"socksHost"`
	SocksPort     int      `json:"socksPort"`
	AutoEnabled   bool     `json:"autoEnabled"`
	AutoURL       string   `json:"autoURL"`
	AutoDiscovery bool     `json:"autoDiscovery"`
	Bypass        []string `json:"bypass"`
}

// AutoHardwarePorts are the hardware ports ShadowsocksX-NG considers when
// "automatically configure network services" is enabled.
var AutoHardwarePorts = []string{"AirPort", "Wi-Fi", "Ethernet"}

var (
	reServiceLine = regexp.MustCompile(`^\((?P<num>\d+|\*)\)\s*(?P<name>.*?)\s*$`)
	rePortLine    = regexp.MustCompile(`Hardware Port:\s*(?P<hp>.*?),\s*Device:\s*(?P<dev>[^)]*)\)`)
)

// ListServices returns the network services in the order macOS tries them.
func ListServices() ([]Service, error) {
	out, err := runCommand(NetworksetupPath, "-listnetworkserviceorder")
	if err != nil {
		return nil, err
	}
	var (
		services []Service
		pending  *Service
	)
	for _, raw := range strings.Split(out, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if m := reServiceLine.FindStringSubmatch(line); m != nil {
			// A service header line, e.g. "(1) Wi-Fi" or "(*) Foo".
			if m[2] == "" {
				continue
			}
			if pending != nil {
				services = append(services, *pending)
			}
			pending = &Service{Name: m[2], Enabled: m[1] != "*"}
			// Disabled services are sometimes written "* Name".
			if strings.HasPrefix(pending.Name, "*") {
				pending.Name = strings.TrimSpace(strings.TrimPrefix(pending.Name, "*"))
				pending.Enabled = false
			}
			continue
		}
		if pending != nil {
			if m := rePortLine.FindStringSubmatch(line); m != nil {
				pending.HardwarePort = strings.TrimSpace(m[1])
				pending.Device = strings.TrimSpace(m[2])
			}
		}
	}
	if pending != nil {
		services = append(services, *pending)
	}
	if len(services) == 0 {
		return nil, errors.New("没有读取到任何网络服务，请检查 networksetup 是否可用")
	}
	return services, nil
}

// AutoSelected returns the names of the services that automatic configuration
// would touch, matching ShadowsocksX-NG's rule.
func AutoSelected(services []Service) []string {
	var out []string
	for _, s := range services {
		for _, hw := range AutoHardwarePorts {
			if strings.EqualFold(s.HardwarePort, hw) {
				out = append(out, s.Name)
				break
			}
		}
	}
	return out
}

// ReadState reads the current proxy configuration of one service.
func ReadState(service string) (ProxyState, error) {
	var st ProxyState
	var firstErr error
	ok := 0
	fail := func(err error) {
		if firstErr == nil {
			firstErr = err
		}
	}

	if out, err := runCommand(NetworksetupPath, "-getwebproxy", service); err == nil {
		st.WebEnabled, st.WebHost, st.WebPort = parseProxy(out)
		ok++
	} else {
		fail(err)
	}
	if out, err := runCommand(NetworksetupPath, "-getsecurewebproxy", service); err == nil {
		st.SecureEnabled, st.SecureHost, st.SecurePort = parseProxy(out)
		ok++
	} else {
		fail(err)
	}
	if out, err := runCommand(NetworksetupPath, "-getsocksfirewallproxy", service); err == nil {
		st.SocksEnabled, st.SocksHost, st.SocksPort = parseProxy(out)
		ok++
	} else {
		fail(err)
	}
	if out, err := runCommand(NetworksetupPath, "-getautoproxyurl", service); err == nil {
		st.AutoEnabled, st.AutoURL = parseAutoProxyURL(out)
		ok++
	} else {
		fail(err)
	}
	if out, err := runCommand(NetworksetupPath, "-getproxybypassdomains", service); err == nil {
		st.Bypass = parseBypass(out)
		ok++
	} else {
		fail(err)
	}
	if out, err := runCommand(NetworksetupPath, "-getproxyautodiscovery", service); err == nil {
		st.AutoDiscovery = strings.Contains(strings.ToLower(out), "on")
	}

	if ok == 0 && firstErr != nil {
		return st, firstErr
	}
	return st, nil
}

// ReadAllStates reads the proxy state of every service, keyed by service name.
func ReadAllStates(services []Service) map[string]ProxyState {
	out := make(map[string]ProxyState, len(services))
	for _, s := range services {
		if st, err := ReadState(s.Name); err == nil {
			out[s.Name] = st
		}
	}
	return out
}

func parseProxy(out string) (enabled bool, host string, port int) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		val = strings.TrimSpace(val)
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "enabled":
			enabled = strings.EqualFold(val, "yes")
		case "server":
			if !strings.EqualFold(val, "(null)") {
				host = val
			}
		case "port":
			port, _ = strconv.Atoi(val)
		}
	}
	return
}

func parseAutoProxyURL(out string) (enabled bool, url string) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		val = strings.TrimSpace(val)
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "url":
			// "URL: http://..." - keep everything after the first colon.
			if idx := strings.Index(line, ":"); idx >= 0 {
				url = strings.TrimSpace(line[idx+1:])
			}
			if strings.EqualFold(url, "(null)") {
				url = ""
			}
		case "enabled":
			enabled = strings.EqualFold(val, "yes")
		}
	}
	return
}

func parseBypass(out string) []string {
	var domains []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.Contains(line, "bypass domains") || strings.Contains(line, "There aren't") {
			continue
		}
		domains = append(domains, line)
	}
	if domains == nil {
		domains = []string{}
	}
	return domains
}

// run executes a command and returns its combined output.
// runCommand is a variable so tests can stub the networksetup binary.
var runCommand = run

func run(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	out := buf.String()
	if err != nil {
		msg := strings.TrimSpace(out)
		if msg == "" {
			msg = err.Error()
		}
		return out, fmt.Errorf("%s %s: %s", name, strings.Join(args, " "), msg)
	}
	// networksetup reports some failures on stdout with a zero exit status.
	if strings.Contains(out, "** Error") {
		return out, fmt.Errorf("%s %s: %s", name, strings.Join(args, " "), strings.TrimSpace(out))
	}
	return out, nil
}
