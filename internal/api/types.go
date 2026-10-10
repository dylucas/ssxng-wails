// Package api defines the data transfer objects shared between the Go backend
// and the web frontend. Wails serialises these structs to JSON; the key names
// the frontend sees are exactly the json tags below.
package api

// ListenerView describes one local listener in the UI.
type ListenerView struct {
	Address string `json:"address"`
	Port    int    `json:"port"`
	Active  bool   `json:"active"`
}

// PACView describes the local PAC server.
type PACView struct {
	Address   string `json:"address"`
	Port      int    `json:"port"`
	Active    bool   `json:"active"`
	URL       string `json:"url"`
	RuleCount int    `json:"ruleCount"`
	Generated int64  `json:"generatedAt"`
	External  string `json:"externalURL"`
	Source    string `json:"source"`
	LastError string `json:"lastError"`
}

// ServerView is a server profile plus derived, UI-only fields.
type ServerView struct {
	ID        string `json:"id"`
	Host      string `json:"host"`
	Port      int    `json:"port"`
	Method    string `json:"method"`
	Password  string `json:"password"`
	Remark    string `json:"remark"`
	Plugin    string `json:"plugin"`
	Title     string `json:"title"`
	HasPlugin bool   `json:"hasPlugin"`
	Active    bool   `json:"active"`
	Connected bool   `json:"connected"`
	LastError string `json:"lastError"`
}

// HelperStatus reports the state of the privileged helper.
type HelperStatus struct {
	Path            string   `json:"path"`
	Installed       bool     `json:"installed"`
	Version         string   `json:"version"`
	RequiredVersion string   `json:"requiredVersion"`
	VersionOK       bool     `json:"versionOK"`
	Setuid          bool     `json:"setuid"`
	Owner           string   `json:"owner"`
	Mode            string   `json:"mode"`
	Applied         []string `json:"applied"`
	LastError       string   `json:"lastError"`
}

// Traffic is the live traffic counters snapshot.
type Traffic struct {
	Up        int64 `json:"up"`
	Down      int64 `json:"down"`
	UpSpeed   int64 `json:"upSpeed"`
	DownSpeed int64 `json:"downSpeed"`
	Active    int   `json:"active"`
	Sessions  int64 `json:"sessions"`
}

// NetworkService is one macOS network service as reported by the helper.
type NetworkService struct {
	Name         string `json:"name"`
	HardwarePort string `json:"hardwarePort"`
	Device       string `json:"device"`
	Selected     bool   `json:"selected"`
	HasOurProxy  bool   `json:"hasOurProxy"`
	WebProxy     string `json:"webProxy"`
	SocksProxy   string `json:"socksProxy"`
	AutoProxy    string `json:"autoProxy"`
}

// LogEntry is one line of the in-app log.
type LogEntry struct {
	Seq     int64  `json:"seq"`
	Time    int64  `json:"time"`
	Level   string `json:"level"`
	Scope   string `json:"scope"`
	Message string `json:"message"`
}

// Check is one row in the diagnostics view.
type Check struct {
	Name       string `json:"name"`
	Status     string `json:"status"` // ok | warn | fail | info
	Detail     string `json:"detail"`
	Suggestion string `json:"suggestion"`
}

// State is the single snapshot the UI renders from. It is pushed to the
// frontend on every change through the "ssxng:state" event.
type State struct {
	Version   string       `json:"version"`
	Enabled   bool         `json:"enabled"`
	Mode      string       `json:"mode"`
	Modes     []string     `json:"modes"`
	Socks     ListenerView `json:"socks"`
	HTTP      ListenerView `json:"http"`
	UDP       ListenerView `json:"udp"`
	PAC       PACView      `json:"pac"`
	Server    *ServerView  `json:"server"`
	Servers   []ServerView `json:"servers"`
	Traffic   Traffic      `json:"traffic"`
	Helper    HelperStatus `json:"helper"`
	Settings  SettingsView `json:"settings"`
	LastError string       `json:"lastError"`
	StartedAt int64        `json:"startedAt"`
}

// SettingsView mirrors config.Settings with the extra computed fields the UI
// needs (but never the raw struct, so we can evolve storage freely).
type SettingsView struct {
	SocksAddress      string   `json:"socksAddress"`
	SocksPort         int      `json:"socksPort"`
	HTTPEnabled       bool     `json:"httpEnabled"`
	HTTPAddress       string   `json:"httpAddress"`
	HTTPPort          int      `json:"httpPort"`
	HTTPFollowGlobal  bool     `json:"httpFollowGlobal"`
	UDPEnabled        bool     `json:"udpEnabled"`
	PACAddress        string   `json:"pacAddress"`
	PACPort           int      `json:"pacPort"`
	PACBindLocalhost  bool     `json:"pacBindLocalhost"`
	GFWListURL        string   `json:"gfwlistURL"`
	ExternalPACURL    string   `json:"externalPACURL"`
	Exceptions        string   `json:"exceptions"`
	AutoConfigure     bool     `json:"autoConfigureNetworkServices"`
	NetworkServices   []string `json:"networkServices"`
	EnablePAC         bool     `json:"enablePAC"`
	EnableGlobal      bool     `json:"enableGlobal"`
	EnableManual      bool     `json:"enableManual"`
	EnableExternalPAC bool     `json:"enableExternalPAC"`
	LaunchAtLogin     bool     `json:"launchAtLogin"`
	HideDockIcon      bool     `json:"hideDockIcon"`
	ClearProxyOnQuit  bool     `json:"clearProxyOnQuit"`
	RestoreOnWake     bool     `json:"restoreProxyOnWake"`
}

// SelfTestStep is one assertion of the built-in end-to-end self test.
type SelfTestStep struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// SelfTestReport is returned by App.SelfTest.
type SelfTestReport struct {
	OK        bool           `json:"ok"`
	Steps     []SelfTestStep `json:"steps"`
	ElapsedMS int64          `json:"elapsedMs"`
}
