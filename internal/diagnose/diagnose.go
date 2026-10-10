// Package diagnose implements the "诊断" tab: a static health check plus a real
// end-to-end self test that starts a local Shadowsocks server and pushes traffic
// through the client's own SOCKS5 and HTTP proxies.
package diagnose

import (
	"fmt"
	"net"
	"os"
	"runtime"
	"strings"
	"time"

	"ssxng/internal/config"
	"ssxng/internal/pac"
	"ssxng/internal/proxyconf"
)

// Status values for Check.
const (
	StatusOK   = "ok"
	StatusWarn = "warn"
	StatusFail = "fail"
	StatusInfo = "info"
)

// Check is one diagnostics row.
type Check struct {
	Name       string
	Status     string
	Detail     string
	Suggestion string
}

// Context is everything the diagnostics need to know about the running app.
type Context struct {
	ConfigDir     string
	Settings      config.Settings
	Servers       []config.ServerProfile
	CurrentServer *config.ServerProfile
	EngineRunning bool
	PACRunning    bool
	SocksAddr     string
	HTTPAddr      string
	HTTPEnabled   bool
	PACAddr       string
	Helper        proxyconf.HelperStatusInfo
	PACFile       string
	GFWListFile   string
	UserRuleFile  string
	LogFile       string
	// LogWriteError is the most recent failure writing the log file, and
	// LogWriteFailures how many times it has failed. Empty means "no trouble".
	LogWriteError    string
	LogWriteFailures int64
	Version          string
}

// Run performs the static checks.
func Run(c Context) []Check {
	var checks []Check
	add := func(name, status, detail, suggestion string) {
		checks = append(checks, Check{Name: name, Status: status, Detail: detail, Suggestion: suggestion})
	}

	// --- configuration folder -------------------------------------------------
	if err := dirWritable(c.ConfigDir); err != nil {
		add("配置目录", StatusFail, fmt.Sprintf("%s 不可写: %v", c.ConfigDir, err),
			"检查磁盘权限，或删除该目录让 SSXNG 重新创建")
	} else {
		add("配置目录", StatusOK, c.ConfigDir, "")
	}

	// --- servers --------------------------------------------------------------
	switch {
	case len(c.Servers) == 0:
		add("服务器", StatusFail, "还没有配置任何服务器", "在“服务器”页添加一个，或导入 ss:// 链接")
	case c.CurrentServer == nil:
		add("服务器", StatusFail, fmt.Sprintf("已配置 %d 个服务器，但没有选中任何一个", len(c.Servers)),
			"在“服务器”页点击要使用的服务器")
	default:
		srv := c.CurrentServer
		status := StatusOK
		detail := fmt.Sprintf("%s（%s:%d，%s）", srv.Title(), srv.Host, srv.Port, srv.Method)
		if err := srv.Validate(); err != nil {
			status = StatusFail
			detail += "；" + err.Error()
		}
		// Reachability is a helpful hint but never fatal: UDP-only hosts and
		// firewalled networks exist.
		addr := net.JoinHostPort(srv.Host, fmt.Sprint(srv.Port))
		start := time.Now()
		if conn, err := net.DialTimeout("tcp", addr, 3*time.Second); err != nil {
			if status == StatusOK {
				status = StatusWarn
			}
			detail += fmt.Sprintf("；TCP 连接失败: %v", err)
		} else {
			_ = conn.Close()
			detail += fmt.Sprintf("；TCP 可连接（%d ms）", time.Since(start).Milliseconds())
		}
		suggestion := ""
		if status != StatusOK {
			suggestion = "确认地址、端口和网络是否可达（部分服务器只允许特定网络访问）"
		}
		add("服务器", status, detail, suggestion)
	}

	// --- local listeners ------------------------------------------------------
	if c.EngineRunning {
		add("本地端口", StatusOK, fmt.Sprintf("代理正在运行：SOCKS %s，HTTP %s", c.SocksAddr, orOff(c.HTTPEnabled, c.HTTPAddr)), "")
	} else {
		var busy []string
		for _, addr := range listeners(c) {
			if err := canBind(addr); err != nil {
				busy = append(busy, fmt.Sprintf("%s（%v）", addr, err))
			}
		}
		if len(busy) > 0 {
			add("本地端口", StatusFail, "以下端口被占用: "+strings.Join(busy, "；"),
				"在“模式与代理”页把端口改成其它值（1024-65535）")
		} else {
			add("本地端口", StatusOK, "SOCKS/HTTP/PAC 端口均可用", "")
		}
	}

	// --- privileged helper ----------------------------------------------------
	switch {
	case !c.Helper.Installed:
		add("特权助手", StatusWarn, "尚未安装特权助手，无法修改系统代理",
			"在“状态”页点击“安装助手”（需要管理员密码）")
	case !c.Helper.VersionOK:
		add("特权助手", StatusWarn,
			fmt.Sprintf("助手版本 %s 与应用要求的 %s 不一致", c.Helper.Version, c.Helper.RequiredVersion),
			"重新点击“安装助手”以升级")
	case !c.Helper.Setuid:
		add("特权助手", StatusFail, fmt.Sprintf("%s 没有 setuid 权限（属主 %s）", c.Helper.Path, c.Helper.Owner),
			"重新安装助手：脚本需要 chown root:admin 与 chmod 4755")
	default:
		add("特权助手", StatusOK, fmt.Sprintf("%s（版本 %s，属主 %s，setuid 正常）", c.Helper.Path, c.Helper.Version, c.Helper.Owner), "")
	}

	// --- PAC ------------------------------------------------------------------
	switch c.Settings.Mode {
	case config.ModeAuto:
		if data, err := os.ReadFile(c.PACFile); err != nil {
			add("PAC 文件", StatusFail, "尚未生成 PAC 文件", "在“PAC”页点击“从 GFWList 更新”")
		} else if n := pac.RuleCountFromPAC(data); n == 0 {
			add("PAC 文件", StatusWarn, "PAC 文件里没有任何规则", "更新 GFWList，或检查 user-rule.txt")
		} else {
			detail := fmt.Sprintf("%d 条规则", n)
			if info, err := os.Stat(c.PACFile); err == nil {
				detail += "，生成于 " + info.ModTime().Format("2006-01-02 15:04")
			}
			if !c.PACRunning {
				add("PAC 文件", StatusWarn, detail+"；但 PAC 服务器没有运行",
					"确认 PAC 端口没有被占用，然后重新开启代理")
			} else {
				add("PAC 文件", StatusOK, detail+"；PAC 服务器在 "+c.PACAddr, "")
			}
		}
	case config.ModeExternalPAC:
		if strings.TrimSpace(c.Settings.PAC.ExternalPACURL) == "" {
			add("外部 PAC", StatusFail, "外部 PAC 模式没有填写地址", "在“PAC”页填写 http/https/file 地址")
		} else if err := pac.ValidateURL(c.Settings.PAC.ExternalPACURL); err != nil {
			add("外部 PAC", StatusFail, err.Error(), "PAC 地址必须是 http、https 或 file")
		} else {
			add("外部 PAC", StatusOK, c.Settings.PAC.ExternalPACURL, "")
		}
	default:
		add("PAC", StatusInfo, fmt.Sprintf("当前模式为 %s，不使用本地 PAC", c.Settings.Mode), "")
	}

	// --- gfwlist --------------------------------------------------------------
	if data, err := os.ReadFile(c.GFWListFile); err != nil {
		add("GFWList 数据", StatusInfo, "尚未下载规则", "在“PAC”页点击“从 GFWList 更新”")
	} else if _, derr := pac.DecodeGFWList(data); derr != nil {
		add("GFWList 数据", StatusFail, "下载的规则文件无法解析: "+derr.Error(), "删除它后重新下载")
	} else {
		detail := fmt.Sprintf("%d 字节", len(data))
		if info, err := os.Stat(c.GFWListFile); err == nil {
			detail += "，" + info.ModTime().Format("2006-01-02 15:04") + " 更新"
		}
		add("GFWList 数据", StatusOK, detail, "")
	}

	// --- user rules -----------------------------------------------------------
	if data, err := os.ReadFile(c.UserRuleFile); err != nil {
		add("用户规则", StatusInfo, "还没有 user-rule.txt", "打开“编辑用户规则”会创建它")
	} else {
		n := 0
		for _, line := range pac.SplitLines(string(data)) {
			line = strings.TrimSpace(line)
			if line != "" && !strings.HasPrefix(line, "!") && !strings.HasPrefix(line, "[") {
				n++
			}
		}
		status := StatusOK
		if n == 0 {
			status = StatusInfo
		}
		add("用户规则", status, fmt.Sprintf("%d 条自定义规则", n), "")
	}

	// --- system proxy ---------------------------------------------------------
	if c.EngineRunning && c.Settings.Enabled {
		add("系统代理", StatusInfo, describeSystemProxy(c), "")
	} else {
		add("系统代理", StatusOK, "代理未开启，系统设置未被改动", "")
	}

	// --- environment ----------------------------------------------------------
	add("运行环境", StatusInfo,
		fmt.Sprintf("SSXNG %s，%s %s/%s，%s", c.Version, runtime.Version(), runtime.GOOS, runtime.GOARCH, runtime.Version()), "")
	// The log file is written from a path that cannot report its own failures
	// (logging about the logger would recurse), so an unwritable file used to be
	// completely silent. Surface it here instead.
	switch {
	case c.LogWriteError != "":
		add("日志文件", StatusWarn,
			fmt.Sprintf("无法写入 %s：%s（已失败 %d 次）", c.LogFile, c.LogWriteError, c.LogWriteFailures),
			"检查磁盘空间与目录权限；界面内的日志不受影响")
	default:
		if size, err := fileSize(c.LogFile); err == nil {
			add("日志文件", StatusInfo, fmt.Sprintf("%s（%s）", c.LogFile, humanBytes(size)), "")
		}
	}
	return checks
}

// describeSystemProxy reads back what macOS currently has for the services we
// manage, so the user can see whether our settings really landed.
func describeSystemProxy(c Context) string {
	services, err := proxyconf.ListServices()
	if err != nil {
		return "无法读取网络服务: " + err.Error()
	}
	targets := c.Settings.NetworkServices
	if c.Settings.AutoConfigureNetworkServices {
		targets = proxyconf.AutoSelected(services)
	}
	if len(targets) == 0 {
		return "没有选择任何网络服务"
	}
	match := proxyconf.Match{SocksHost: c.Settings.SOCKS.Address, SocksPort: c.Settings.SOCKS.Port}
	if c.Settings.Mode == config.ModeAuto || c.Settings.Mode == config.ModeExternalPAC {
		match.PACURL = c.Settings.PAC.ExternalPACURL
	}
	var parts []string
	for _, name := range targets {
		st, err := proxyconf.ReadState(name)
		if err != nil {
			parts = append(parts, name+"：读取失败")
			continue
		}
		switch {
		case proxyconf.Matches(st, match):
			parts = append(parts, name+"：已指向 SSXNG")
		case st.AutoEnabled && st.AutoURL != "":
			parts = append(parts, fmt.Sprintf("%s：PAC %s", name, st.AutoURL))
		case st.SocksEnabled:
			parts = append(parts, fmt.Sprintf("%s：SOCKS %s:%d", name, st.SocksHost, st.SocksPort))
		default:
			if st.WebEnabled {
				parts = append(parts, fmt.Sprintf("%s：HTTP %s:%d（未指向 SSXNG）", name, st.WebHost, st.WebPort))
			} else {
				parts = append(parts, name+"：未设置代理")
			}
		}
	}
	return strings.Join(parts, "；")
}

func listeners(c Context) []string {
	out := []string{c.SocksAddr}
	if c.HTTPEnabled {
		out = append(out, c.HTTPAddr)
	}
	if c.Settings.Mode == config.ModeAuto {
		out = append(out, c.PACAddr)
	}
	if c.Settings.Mode == config.ModeExternalPAC {
		out = out[:1]
	}
	return out
}

func orOff(enabled bool, value string) string {
	if enabled {
		return value
	}
	return "关闭"
}

func dirWritable(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".write-test-*")
	if err != nil {
		return err
	}
	name := f.Name()
	f.Close()
	return os.Remove(name)
}

func canBind(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return ln.Close()
}

func fileSize(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KiB", "MiB", "GiB", "TiB"}
	f := float64(n)
	i := -1
	for f >= unit && i < len(units)-1 {
		f /= unit
		i++
	}
	return fmt.Sprintf("%.1f %s", f, units[i])
}
