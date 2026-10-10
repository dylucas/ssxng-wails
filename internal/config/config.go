// Package config holds the persisted settings and server profiles for SSXNG.
//
// State lives in ~/Library/Application Support/SSXNG/ :
//
//	config.json   user settings (mode 0600)
//	servers.json  server profiles (mode 0600)
//
// Both files are written atomically (temp file in the same directory +
// rename) so a crash can never leave a half-written configuration behind.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Modes understood by the application. They mirror ShadowsocksX-NG's
// "ShadowsocksRunningMode" values.
const (
	ModeAuto        = "auto"        // system PAC URL pointing at our PAC server
	ModeGlobal      = "global"      // system proxy set to our local listeners
	ModeManual      = "manual"      // leave the system alone
	ModeExternalPAC = "externalPAC" // system PAC URL pointing at a user supplied URL
	ModeOff         = "off"         // clear the system proxy
)

// ModeOrder is the canonical order used by the UI and the menu bar.
var ModeOrder = []string{ModeAuto, ModeGlobal, ModeManual, ModeExternalPAC, ModeOff}

// ValidMode reports whether m is a known mode.
func ValidMode(m string) bool {
	for _, v := range ModeOrder {
		if v == m {
			return true
		}
	}
	return false
}

// DefaultExceptions is the bypass list used by ShadowsocksX-NG out of the box.
const DefaultExceptions = "127.0.0.1, localhost, 192.168.0.0/16, 10.0.0.0/8, FE80::/64, ::1, FD00::/8"

// DefaultGFWListURL is the list ShadowsocksX-NG ships with.
const DefaultGFWListURL = "https://raw.githubusercontent.com/gfwlist/gfwlist/master/gfwlist.txt"

// DefaultGFWListMirror is reachable in networks where raw.githubusercontent.com
// is blocked; used when the user has not changed the URL.
const DefaultGFWListMirror = "https://cdn.jsdelivr.net/gh/gfwlist/gfwlist@master/gfwlist.txt"

// Listener is one local listening socket.
type Listener struct {
	Address string `json:"address"`
	Port    int    `json:"port"`
}

// HTTPListener adds the "follow global mode" switch SSX-NG exposes for its
// privoxy instance.
type HTTPListener struct {
	Enabled      bool   `json:"enabled"`
	Address      string `json:"address"`
	Port         int    `json:"port"`
	FollowGlobal bool   `json:"followGlobal"`
}

// PACServer describes the local PAC HTTP server.
type PACServer struct {
	Address         string `json:"address"`
	Port            int    `json:"port"`
	BindToLocalhost bool   `json:"bindToLocalhost"`
	GFWListURL      string `json:"gfwlistURL"`
	ExternalPACURL  string `json:"externalPACURL"`
}

// SwitchModes mirrors "EnableSwitchMode.*": which modes are offered when
// cycling through modes from the menu bar.
type SwitchModes struct {
	PAC         bool `json:"pac"`
	Global      bool `json:"global"`
	Manual      bool `json:"manual"`
	ExternalPAC bool `json:"externalPAC"`
}

// Settings is config.json.
type Settings struct {
	Version int    `json:"version"`
	Enabled bool   `json:"enabled"`
	Mode    string `json:"mode"`

	SOCKS Listener     `json:"socks"`
	HTTP  HTTPListener `json:"http"`
	PAC   PACServer    `json:"pac"`

	Exceptions                   string      `json:"exceptions"`
	AutoConfigureNetworkServices bool        `json:"autoConfigureNetworkServices"`
	NetworkServices              []string    `json:"networkServices"`
	SwitchModes                  SwitchModes `json:"enableSwitchMode"`

	LaunchAtLogin bool `json:"launchAtLogin"`
	HideDockIcon  bool `json:"hideDockIcon"`

	ClearProxyOnQuit   bool `json:"clearProxyOnQuit"`
	RestoreProxyOnWake bool `json:"restoreProxyOnWake"`

	CurrentServerID string `json:"currentServerID"`
}

// DefaultSettings returns the factory configuration. Ports match
// ShadowsocksX-NG (socks 1086, http 1087, pac 1089).
func DefaultSettings() Settings {
	return Settings{
		Version: 1,
		Enabled: false, // stays off until at least one server exists
		Mode:    ModeAuto,
		SOCKS:   Listener{Address: "127.0.0.1", Port: 1086},
		HTTP:    HTTPListener{Enabled: true, Address: "127.0.0.1", Port: 1087, FollowGlobal: false},
		PAC: PACServer{
			Address:         "127.0.0.1",
			Port:            1089,
			BindToLocalhost: true,
			GFWListURL:      DefaultGFWListURL,
			ExternalPACURL:  "",
		},
		Exceptions:                   DefaultExceptions,
		AutoConfigureNetworkServices: true,
		NetworkServices:              []string{},
		SwitchModes:                  SwitchModes{PAC: true, Global: true, Manual: false, ExternalPAC: false},
		LaunchAtLogin:                false,
		HideDockIcon:                 false,
		ClearProxyOnQuit:             true,
		RestoreProxyOnWake:           true,
		CurrentServerID:              "",
	}
}

// ExceptionList splits the free-form bypass list the same way SSX-NG does:
// on commas, the ideographic comma and whitespace.
// ExceptionList splits the user's bypass domains. Separators are commas,
// semicolons, ideographic commas and whitespace. An empty result falls back to
// DefaultExceptions.
func (s Settings) ExceptionList() []string {
	fields := strings.FieldsFunc(s.Exceptions, func(r rune) bool {
		switch r {
		case ',', ';', '、', ' ', '\t', '\n', '\r':
			return true
		}
		return false
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		f = strings.TrimSpace(f)
		// Drop separator-only leftovers such as ";;" that users produce by
		// accident: passing those to networksetup would add a junk domain.
		if f == "" || strings.Trim(f, ",;、") == "" {
			continue
		}
		out = append(out, f)
	}
	if len(out) == 0 {
		// Never hand an empty list to the helper: in global mode that means
		// "bypass nothing", so loopback and LAN traffic would be sent through
		// the proxy. Falling back keeps the client usable.
		return defaultExceptionList()
	}
	return out
}

func defaultExceptionList() []string {
	return strings.FieldsFunc(DefaultExceptions, func(r rune) bool { return r == ',' })
}

// Loopback reports whether addr only accepts local connections. The privileged
// helper refuses anything else, so the UI must reject it up front too.
func Loopback(addr string) bool {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return false
	}
	if addr == "localhost" {
		return true
	}
	ip := net.ParseIP(strings.Trim(addr, "[]"))
	return ip != nil && ip.IsLoopback()
}

// Validate rejects configurations the privileged helper would refuse anyway.
func (s *Settings) Validate() error {
	if !ValidMode(s.Mode) {
		return fmt.Errorf("未知模式 %q", s.Mode)
	}
	for name, l := range map[string]Listener{
		"SOCKS": s.SOCKS,
		"HTTP":  {s.HTTP.Address, s.HTTP.Port},
		"PAC":   {s.PAC.Address, s.PAC.Port},
	} {
		if !Loopback(l.Address) {
			return fmt.Errorf("%s 监听地址必须是本机回环地址（127.0.0.1 / localhost / ::1），当前为 %q", name, l.Address)
		}
		if l.Port < 1024 || l.Port > 65535 {
			return fmt.Errorf("%s 端口必须在 1024-65535 之间，当前为 %d", name, l.Port)
		}
	}
	if s.SOCKS.Port == s.HTTP.Port && s.HTTP.Enabled {
		return errors.New("SOCKS 与 HTTP 端口不能相同")
	}
	if s.PAC.Port == s.SOCKS.Port || (s.HTTP.Enabled && s.PAC.Port == s.HTTP.Port) {
		return errors.New("PAC 端口不能与 SOCKS/HTTP 端口相同")
	}
	if s.Mode == ModeExternalPAC && strings.TrimSpace(s.PAC.ExternalPACURL) == "" {
		return errors.New("外部 PAC 模式需要填写 PAC URL")
	}
	if s.PAC.GFWListURL == "" {
		s.PAC.GFWListURL = DefaultGFWListURL
	}
	if s.Version == 0 {
		s.Version = 1
	}
	return nil
}

// Store owns config.json and servers.json.
type Store struct {
	dir string

	mu      sync.RWMutex
	sett    Settings
	servers []ServerProfile
}

// DefaultDir is ~/Library/Application Support/SSXNG. The SSXNG_CONFIG_DIR
// environment variable overrides it, which keeps the app testable in sandboxes
// and lets a user keep several configurations around.
func DefaultDir() (string, error) {
	if dir := strings.TrimSpace(os.Getenv("SSXNG_CONFIG_DIR")); dir != "" {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "Application Support", "SSXNG"), nil
}

// Open loads the configuration from dir, creating it (and defaults) if needed.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("创建配置目录失败: %w", err)
	}
	s := &Store{dir: dir, sett: DefaultSettings()}

	raw, err := os.ReadFile(s.ConfigPath())
	switch {
	case err == nil:
		if err := json.Unmarshal(raw, &s.sett); err != nil {
			return nil, fmt.Errorf("解析 %s 失败: %w", s.ConfigPath(), err)
		}
	case !os.IsNotExist(err):
		return nil, err
	}
	// Fill in anything a older/partial file left out, then normalise.
	if s.sett.Version == 0 || s.sett.Mode == "" {
		def := DefaultSettings()
		def.Enabled = s.sett.Enabled
		def.CurrentServerID = s.sett.CurrentServerID
		s.sett = def
	}
	servers, err := loadServers(s.ServersPath())
	if err != nil {
		return nil, err
	}
	s.servers = servers

	if err := s.persistLocked(); err != nil {
		return nil, err
	}
	return s, nil
}

// Dir returns the state directory.
func (s *Store) Dir() string { return s.dir }

// ConfigPath returns the settings file path.
func (s *Store) ConfigPath() string { return filepath.Join(s.dir, "config.json") }

// ServersPath returns the server profile file path.
func (s *Store) ServersPath() string { return filepath.Join(s.dir, "servers.json") }

// PACFilePath returns the generated PAC file path.
func (s *Store) PACFilePath() string { return filepath.Join(s.dir, "gfwlist.js") }

// GFWListPath returns the cached (base64) gfwlist download path.
func (s *Store) GFWListPath() string { return filepath.Join(s.dir, "gfwlist.txt") }

// UserRulePath returns the user rule file path.
func (s *Store) UserRulePath() string { return filepath.Join(s.dir, "user-rule.txt") }

// LogFilePath returns the log file path.
func (s *Store) LogFilePath() string { return filepath.Join(s.dir, "ssxng.log") }

// Settings returns a copy of the current settings.
func (s *Store) Settings() Settings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sett
}

// SaveSettings validates, stores and persists the given settings.
func (s *Store) SaveSettings(sett Settings) error {
	if err := sett.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sett = sett
	return s.persistLocked()
}

// Servers returns a copy of the server list.
func (s *Store) Servers() []ServerProfile {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]ServerProfile, len(s.servers))
	copy(out, s.servers)
	return out
}

// ServerByID returns the profile with the given id.
func (s *Store) ServerByID(id string) (ServerProfile, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, p := range s.servers {
		if p.ID == id {
			return p, true
		}
	}
	return ServerProfile{}, false
}

// CurrentServer returns the selected profile, if any.
func (s *Store) CurrentServer() (ServerProfile, bool) {
	s.mu.RLock()
	id := s.sett.CurrentServerID
	s.mu.RUnlock()
	return s.ServerByID(id)
}

// MutateServers runs fn under the write lock and persists the result.
func (s *Store) MutateServers(fn func(list []ServerProfile) ([]ServerProfile, error)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next, err := fn(append([]ServerProfile(nil), s.servers...))
	if err != nil {
		return err
	}
	s.servers = next
	return s.persistLocked()
}

// SetCurrentServerID selects a server (empty string clears the selection).
func (s *Store) SetCurrentServerID(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id != "" {
		found := false
		for _, p := range s.servers {
			if p.ID == id {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("服务器 %s 不存在", id)
		}
	}
	s.sett.CurrentServerID = id
	return s.persistLocked()
}

// persistLocked writes both files. Callers must hold the write lock.
func (s *Store) persistLocked() error {
	if err := writeJSONAtomic(s.ConfigPath(), s.sett); err != nil {
		return err
	}
	return writeJSONAtomic(s.ServersPath(), s.servers)
}

func loadServers(path string) ([]ServerProfile, error) {
	raw, err := os.ReadFile(path)
	switch {
	case os.IsNotExist(err):
		return []ServerProfile{}, nil
	case err != nil:
		return nil, err
	}
	var list []ServerProfile
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("解析 %s 失败: %w", path, err)
	}
	if list == nil {
		list = []ServerProfile{}
	}
	return list, nil
}

// writeJSONAtomic marshals v and writes it via a temp file + rename with 0600.
func writeJSONAtomic(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
