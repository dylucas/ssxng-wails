package proxyconf

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// HelperStatusInfo describes the installed helper binary.
type HelperStatusInfo struct {
	Path            string
	Installed       bool
	Version         string
	RequiredVersion string
	VersionOK       bool
	Setuid          bool
	Owner           string
	LastError       string
}

// Status inspects the installed helper without changing anything.
func Status() HelperStatusInfo {
	info := HelperStatusInfo{Path: HelperInstallPath, RequiredVersion: HelperVersion}
	fi, err := os.Stat(HelperInstallPath)
	if err != nil {
		info.LastError = "助手未安装"
		return info
	}
	info.Installed = true
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		info.Setuid = st.Mode&syscall.S_ISUID != 0
		info.Owner = fmt.Sprintf("%d:%d", st.Uid, st.Gid)
	}
	resp, err := Call("--version")
	if err != nil {
		info.LastError = err.Error()
		return info
	}
	info.Version = resp.Version
	info.VersionOK = VersionAtLeast(resp.Version, HelperVersion)
	if !info.VersionOK {
		info.LastError = fmt.Sprintf("助手版本 %s 过旧，需要 %s", resp.Version, HelperVersion)
	}
	if !info.Setuid {
		info.LastError = "助手缺少 setuid 权限，请重新安装"
		info.VersionOK = false
	}
	return info
}

// Call runs the installed helper and parses its JSON reply.
func Call(args ...string) (Response, error) {
	var resp Response
	out, err := exec.Command(HelperInstallPath, args...).Output()
	if len(out) > 0 {
		if jerr := json.Unmarshal(lastJSONLine(out), &resp); jerr != nil {
			return resp, fmt.Errorf("解析助手输出失败: %v (原始输出: %s)", jerr, strings.TrimSpace(string(out)))
		}
	}
	if err != nil && resp.Error == "" {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			resp.Error = strings.TrimSpace(string(ee.Stderr))
		}
		if resp.Error == "" {
			resp.Error = err.Error()
		}
	}
	if resp.Error != "" && resp.OK {
		// Warnings only: keep going.
		return resp, nil
	}
	if resp.Error != "" {
		return resp, errors.New(resp.Error)
	}
	return resp, nil
}

// lastJSONLine tolerates stray output before the JSON payload.
func lastJSONLine(out []byte) []byte {
	lines := bytes.Split(bytes.TrimSpace(out), []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		l := bytes.TrimSpace(lines[i])
		if len(l) > 0 && l[0] == '{' {
			return l
		}
	}
	return bytes.TrimSpace(out)
}

// InstallHelper copies the helper binary at src into /Library/Application
// Support/SSXNG/ as root:admin with the setuid bit set, asking for
// administrator rights exactly once (the same UX as ShadowsocksX-NG).
//
// The helper must already carry a valid code signature; Go's linker ad-hoc
// signs darwin/arm64 binaries automatically, and we verify it here before
// installing because a setuid binary without a signature will not run on
// Apple Silicon.
func InstallHelper(src string) (HelperStatusInfo, error) {
	fi, err := os.Stat(src)
	if err != nil {
		return Status(), fmt.Errorf("找不到助手文件 %s: %w", src, err)
	}
	if fi.Size() == 0 {
		return Status(), fmt.Errorf("助手文件 %s 是空的", src)
	}
	if err := verifySignature(src); err != nil {
		return Status(), err
	}

	script := fmt.Sprintf(`set -e
mkdir -p %s
cp -f %s %s
chown root:admin %s
chmod 4755 %s
`,
		shellQuote(HelperInstallDir),
		shellQuote(src),
		shellQuote(HelperInstallPath),
		shellQuote(HelperInstallPath),
		shellQuote(HelperInstallPath),
	)
	if err := runAsAdmin(script); err != nil {
		return Status(), err
	}

	// Re-read the signature so macOS registers the new inode, then verify.
	_ = exec.Command("/usr/bin/codesign", "--verify", "--verbose=0", HelperInstallPath).Run()

	st := Status()
	if !st.Installed {
		return st, errors.New("助手安装后仍不可见，请检查 /Library/Application Support/SSXNG/")
	}
	if !st.Setuid {
		return st, errors.New("助手安装完成但没有 setuid 权限")
	}
	if !st.VersionOK {
		return st, errors.New(st.LastError)
	}
	return st, nil
}

// verifySignature makes sure codesign considers the binary valid.
func verifySignature(path string) error {
	out, err := exec.Command("/usr/bin/codesign", "--verify", "--verbose=0", path).CombinedOutput()
	if err != nil {
		return fmt.Errorf("助手没有有效签名（setuid 二进制在 Apple Silicon 上必须签名）: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// runAsAdmin runs a shell script through osascript with administrator
// privileges, which is the only supported way to trigger the macOS
// authentication dialog from a normal process.
func runAsAdmin(script string) error {
	appleScript := fmt.Sprintf(`do shell script %s with administrator privileges`, appleScriptQuote(script))
	out, err := exec.Command("/usr/bin/osascript", "-e", appleScript).CombinedOutput()
	msg := strings.TrimSpace(string(out))
	if err != nil {
		if msg == "" {
			msg = err.Error()
		}
		if strings.Contains(msg, "User canceled") || strings.Contains(msg, "-128") {
			return errors.New("已取消管理员授权")
		}
		return fmt.Errorf("管理员操作失败: %s", msg)
	}
	return nil
}

// shellQuote single-quotes a path for /bin/sh.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// appleScriptQuote produces a double-quoted AppleScript string literal.
func appleScriptQuote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}

// ExtractHelper writes helperBytes to a private cache path and returns it.
func ExtractHelper(helperBytes []byte) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, "Library", "Caches", "SSXNG")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	dst := filepath.Join(dir, "ssxng-helper")
	if err := os.WriteFile(dst, helperBytes, 0o755); err != nil {
		return "", err
	}
	if err := os.Chmod(dst, 0o755); err != nil {
		return "", err
	}
	return dst, nil
}

// FormatPort is a tiny helper used by the UI to render "host:port".
func FormatPort(host string, port int) string {
	return host + ":" + strconv.Itoa(port)
}
