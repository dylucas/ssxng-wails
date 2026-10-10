// Command ssxng-helper configures the macOS system proxy on behalf of the GUI.
//
// It is installed setuid-root (mode 4755, owner root:admin) into
// /Library/Application Support/SSXNG/ by the GUI, using a single administrator
// prompt. Every invocation is restricted to a whitelist of networksetup
// operations: there is no way to run arbitrary commands through it.
//
// The command line mirrors ShadowsocksX-NG's proxy_conf_helper:
//
//	ssxng-helper --version
//	ssxng-helper --list-services
//	ssxng-helper --mode auto   --pac-url <url> [--network-service <svc>]...
//	ssxng-helper --mode global --port <n> --socks-listen-address <addr>
//	            [--http-port <n> --http-listen-address <addr>] [--network-service <svc>]...
//	ssxng-helper --mode off    --pac-url <url> --port <n> --socks-listen-address <addr> [...]
//
// All output is a single JSON line (proxyconf.Response) so the caller can
// report precise errors.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/user"
	"strconv"
	"strings"

	"ssxng/internal/proxyconf"
)

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error {
	v = strings.TrimSpace(v)
	if v != "" {
		*s = append(*s, v)
	}
	return nil
}

func main() {
	var (
		showVersion  = flag.Bool("version", false, "print the helper version and exit")
		listServices = flag.Bool("list-services", false, "print the network services and their proxy state as JSON")
		dryRun       = flag.Bool("dry-run", false, "print the commands that would run instead of running them")
		mode         = flag.String("mode", "", "proxy mode: auto, global or off")
		pacURL       = flag.String("pac-url", "", "PAC URL for auto mode")
		port         = flag.Int("port", 0, "local SOCKS5 port for global mode")
		socksAddr    = flag.String("socks-listen-address", "127.0.0.1", "local SOCKS5 listen address")
		httpPort     = flag.Int("http-port", 0, "local HTTP proxy port for global mode")
		httpAddr     = flag.String("http-listen-address", "127.0.0.1", "local HTTP proxy listen address")
		statePath    = flag.String("state-path", proxyconf.HelperStatePath, "path of the bypass domain snapshot file")
		services     stringList
		exceptions   stringList
	)
	flag.Var(&services, "network-service", "network service to configure (repeatable; default: auto)")
	flag.Var(&exceptions, "proxy-exception", "proxy bypass domain (repeatable)")
	flag.Parse()

	resp := proxyconf.Response{Version: proxyconf.HelperVersion}

	if *showVersion {
		resp.OK = true
		emit(resp, 0)
	}
	if *listServices {
		emit(listAll(), 0)
	}

	if *mode == "" {
		resp.Error = "必须指定 --mode auto|global|off（或使用 --version / --list-services）"
		emit(resp, 2)
	}
	if err := authorize(); err != nil && !*dryRun {
		resp.Error = err.Error()
		emit(resp, 1)
	}

	code := apply(*mode, applyOptions{
		pacURL:     *pacURL,
		port:       *port,
		socksAddr:  *socksAddr,
		httpPort:   *httpPort,
		httpAddr:   *httpAddr,
		services:   services,
		exceptions: exceptions,
		statePath:  *statePath,
		dryRun:     *dryRun,
	}, &resp)
	emit(resp, code)
}

type applyOptions struct {
	pacURL     string
	port       int
	socksAddr  string
	httpPort   int
	httpAddr   string
	services   []string
	exceptions []string
	statePath  string
	dryRun     bool
}

func apply(mode string, o applyOptions, resp *proxyconf.Response) int {
	all, err := proxyconf.ListServices()
	if err != nil {
		resp.Error = err.Error()
		return 1
	}
	targets := o.services
	if len(targets) == 0 {
		targets = proxyconf.AutoSelected(all)
	}
	if len(targets) == 0 {
		resp.Error = "没有可配置的网络服务（自动选择只匹配 Wi-Fi / Ethernet 硬件端口）"
		return 1
	}
	for _, name := range targets {
		if err := proxyconf.ValidateServiceName(name, all); err != nil {
			resp.Error = err.Error()
			return 1
		}
	}

	switch mode {
	case "auto", "global", "off":
	default:
		resp.Error = fmt.Sprintf("未知模式 %q", mode)
		return 2
	}

	// A missing or damaged snapshot is not fatal: the helper can still install
	// the requested proxy, it just cannot restore what was there before. Losing
	// that ability is worth a loud warning, not a failure that would leave the
	// user unable to turn the proxy off.
	snap, err := proxyconf.LoadSnapshot(o.statePath)
	if err != nil {
		resp.Warnings = append(resp.Warnings, fmt.Sprintf("无法读取代理状态快照（%v）；将跳过恢复原始设置", err))
		snap = proxyconf.Snapshot{}
	}

	var cmds []proxyconf.Command
	switch mode {
	case "auto":
		if strings.TrimSpace(o.pacURL) == "" {
			resp.Error = "auto 模式需要 --pac-url"
			return 2
		}
		cmds = proxyconf.PlanAuto(targets, o.pacURL)
		resp.Applied = targets

	case "global":
		if o.port < 1024 || o.port > 65535 {
			resp.Error = fmt.Sprintf("SOCKS 端口 %d 不合法", o.port)
			return 2
		}
		if !isLoopback(o.socksAddr) || (o.httpPort != 0 && !isLoopback(o.httpAddr)) {
			resp.Error = "只允许把系统代理指向本机回环地址"
			return 2
		}
		// Remember the bypass list before we overwrite it.
		for _, name := range targets {
			st, err := proxyconf.ReadState(name)
			if err != nil {
				continue
			}
			if !proxyconf.Matches(st, proxyconf.Match{PACURL: o.pacURL, SocksHost: o.socksAddr, SocksPort: o.port}) {
				snap.Record(name, st.Bypass)
			}
		}
		cmds = proxyconf.PlanGlobal(targets, o.socksAddr, o.port, o.httpAddr, o.httpPort, o.httpPort != 0, o.exceptions)
		resp.Applied = targets

	case "off":
		match := proxyconf.Match{PACURL: o.pacURL, SocksHost: o.socksAddr, SocksPort: o.port}
		for _, name := range targets {
			st, err := proxyconf.ReadState(name)
			if err != nil {
				resp.Skipped = append(resp.Skipped, name)
				continue
			}
			if !proxyconf.Matches(st, match) {
				// Someone else's configuration: leave it alone.
				resp.Skipped = append(resp.Skipped, name)
				continue
			}
			// restores the user's original bypass list; nil means we never
			// recorded one, in which case we leave the list alone.
			cmds = append(cmds, proxyconf.PlanOffFor(name, snap.Take(name))...)
			resp.Applied = append(resp.Applied, name)
		}
	}

	for _, c := range cmds {
		resp.Commands = append(resp.Commands, c.String())
	}
	if o.dryRun {
		resp.OK = true
		return 0
	}

	if err := proxyconf.RunAll(cmds); err != nil {
		resp.Error = err.Error()
		resp.OK = false
		return 1
	}
	if err := snap.Save(o.statePath); err != nil {
		// Non fatal: the proxy is configured, we just cannot restore later.
		resp.Warnings = append(resp.Warnings, fmt.Sprintf("写入快照失败: %v", err))
	}
	resp.OK = true
	return 0
}

func listAll() proxyconf.Response {
	resp := proxyconf.Response{Version: proxyconf.HelperVersion}
	all, err := proxyconf.ListServices()
	if err != nil {
		resp.Error = err.Error()
		return resp
	}
	auto := map[string]bool{}
	for _, n := range proxyconf.AutoSelected(all) {
		auto[n] = true
	}
	for _, s := range all {
		info := proxyconf.ServiceInfo{Service: s, Auto: auto[s.Name]}
		if st, err := proxyconf.ReadState(s.Name); err == nil {
			info.Proxy = st
			info.Ours = proxyconf.Matches(st, proxyconf.Match{})
		}
		resp.Services = append(resp.Services, info)
	}
	resp.OK = true
	return resp
}

// authorize makes sure the caller is allowed to change system settings. In a
// setuid-root binary os.Getuid() is the *real* uid of the invoking user and
// os.Getgroups() the invoking user's groups, so membership of "admin" is what
// we check - the same bar macOS uses for privileged settings changes.
func authorize() error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("助手没有 setuid root 权限（当前 euid=%d），请重新安装助手", os.Geteuid())
	}
	if os.Getuid() == 0 {
		return nil
	}
	if gid, err := adminGID(); err == nil {
		for _, g := range groupsOf() {
			if g == gid {
				return nil
			}
		}
	}
	return fmt.Errorf("调用者 uid=%d 不在 admin 组，拒绝修改系统代理", os.Getuid())
}

func adminGID() (int, error) {
	g, err := user.LookupGroup("admin")
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(g.Gid)
}

// groupsOf returns the supplementary groups of the current process. For a
// setuid binary these are inherited from the invoking session, so they are the
// invoking user's groups.
func groupsOf() []int {
	gs, err := os.Getgroups()
	if err != nil {
		return nil
	}
	return gs
}

func isLoopback(addr string) bool {
	a := strings.Trim(strings.TrimSpace(addr), "[]")
	return a == "localhost" || a == "127.0.0.1" || a == "::1"
}

func emit(resp proxyconf.Response, code int) {
	enc, err := json.Marshal(resp)
	if err != nil {
		fmt.Printf("{\"ok\":false,\"error\":%q}\n", err.Error())
		os.Exit(1)
	}
	fmt.Println(string(enc))
	os.Exit(code)
}
