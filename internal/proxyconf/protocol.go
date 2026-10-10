package proxyconf

import (
	"strconv"
	"strings"
)

// HelperVersion is the version of the privileged helper protocol. The GUI
// refuses to use an older helper (and offers to reinstall it), exactly like
// ShadowsocksX-NG's proxy_conf_helper_version.h.
const HelperVersion = "1.0.0"

// HelperInstallDir is where the setuid helper lives.
const HelperInstallDir = "/Library/Application Support/SSXNG"

// HelperInstallPath is the absolute path of the installed helper.
const HelperInstallPath = HelperInstallDir + "/ssxng-helper"

// HelperStatePath is the root-owned snapshot file the helper keeps.
const HelperStatePath = HelperInstallDir + "/proxy-state.json"

// ServiceInfo is one row of --list-services.
type ServiceInfo struct {
	Service
	Proxy ProxyState `json:"proxy"`
	Ours  bool       `json:"ours"`
	Auto  bool       `json:"autoSelected"`
}

// Response is the helper's single-line JSON reply.
type Response struct {
	OK       bool          `json:"ok"`
	Version  string        `json:"version,omitempty"`
	Applied  []string      `json:"applied,omitempty"`
	Skipped  []string      `json:"skipped,omitempty"`
	Commands []string      `json:"commands,omitempty"`
	Warnings []string      `json:"warnings,omitempty"`
	Services []ServiceInfo `json:"services,omitempty"`
	Error    string        `json:"error,omitempty"`
}

// VersionAtLeast compares dotted numeric versions. Non-numeric suffixes are
// ignored ("1.2.3-beta" == "1.2.3").
func VersionAtLeast(have, need string) bool {
	hv := versionParts(have)
	nv := versionParts(need)
	for i := 0; i < len(nv); i++ {
		var h int
		if i < len(hv) {
			h = hv[i]
		}
		if h != nv[i] {
			return h > nv[i]
		}
	}
	return true
}

func versionParts(v string) []int {
	fields := strings.FieldsFunc(strings.TrimSpace(v), func(r rune) bool { return r == '.' || r == '-' || r == '+' })
	out := make([]int, 0, len(fields))
	for _, f := range fields {
		n, err := strconv.Atoi(f)
		if err != nil {
			break
		}
		out = append(out, n)
	}
	return out
}
