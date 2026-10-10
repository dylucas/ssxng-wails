package pac

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ContentType is the MIME type browsers expect for a PAC file.
const ContentType = "application/x-ns-proxy-autoconfig"

// Server serves the generated PAC file over loopback HTTP so that the system
// proxy settings and third party PAC clients can fetch it.
//
// ShadowsocksX-NG shells out to privoxy for the same job; serving it in Go
// avoids the dependency and keeps the file on loopback only.
type Server struct {
	source func() ([]byte, error)
	log    func(level, message string)

	mu    sync.Mutex
	srv   *http.Server
	ln    net.Listener
	url   string
	hits  atomic.Int64
	lastE atomic.Value // string
}

// NewServer creates a PAC server. source is called for every request, so an
// updated PAC file takes effect immediately.
func NewServer(source func() ([]byte, error), log func(level, message string)) *Server {
	return &Server{source: source, log: log}
}

// Start binds addr and begins serving /proxy.pac.
func (s *Server) Start(addr string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.srv != nil {
		return errors.New("PAC 服务器已经在运行")
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("无法监听 PAC 端口 %s: %v", addr, err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/proxy.pac", s.handle)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		s.handle(w, r)
	})
	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
	}
	s.srv = srv
	s.ln = ln
	s.url = ServerURL(ln.Addr().String(), 0)
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			if s.log != nil {
				s.log("warn", "PAC 服务器异常退出: "+err.Error())
			}
		}
	}()
	return nil
}

// Stop shuts the server down.
func (s *Server) Stop() {
	s.mu.Lock()
	srv, ln := s.srv, s.ln
	s.srv, s.ln, s.url = nil, nil, ""
	s.mu.Unlock()
	if srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}
	if ln != nil {
		_ = ln.Close()
	}
}

// ServerURL builds the PAC URL for a listen address. host may either be a bare
// host ("127.0.0.1", port taken from port) or an already joined "host:port"
// address (port ignored), so the same helper serves both the running server and
// the settings-derived URL used when deciding whether an installed PAC entry is
// ours.
func ServerURL(host string, port int) string {
	addr := host
	if _, _, err := net.SplitHostPort(host); err != nil {
		addr = net.JoinHostPort(host, strconv.Itoa(port))
	}
	return "http://" + addr + "/proxy.pac"
}

// URL returns the PAC URL, or an empty string when the server is stopped.
func (s *Server) URL() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.url
}

// Running reports whether the server is up.
func (s *Server) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.srv != nil
}

// Hits is the number of PAC requests served.
func (s *Server) Hits() int64 { return s.hits.Load() }

// LastError returns the error of the most recent failed request.
func (s *Server) LastError() string {
	if v, ok := s.lastE.Load().(string); ok {
		return v
	}
	return ""
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	// Browsers and macOS cache PAC files aggressively; the app appends ?v=<epoch>
	// to the URL it installs, and we also tell clients not to cache.
	w.Header().Set("Content-Type", ContentType)
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.source == nil {
		http.Error(w, "PAC 文件尚未生成", http.StatusServiceUnavailable)
		return
	}
	content, err := s.source()
	if err != nil {
		s.lastE.Store(err.Error())
		http.Error(w, "PAC 文件不可用: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	s.hits.Add(1)
	if r.Method == http.MethodHead {
		w.Header().Set("Content-Length", fmt.Sprint(len(content)))
		w.WriteHeader(http.StatusOK)
		return
	}
	// Write the bytes straight out. The previous io.Copy(strings.NewReader(
	// string(content))) converted the whole file — hundreds of kilobytes — to a
	// string just to copy it, on every request.
	if _, err := w.Write(content); err != nil {
		s.lastE.Store(err.Error())
	}
}

// RuleCountFromPAC is a helper for the UI: it counts the entries of the JSON
// array embedded in a generated PAC file.
//
// It searches the raw bytes rather than converting the file to a string, which
// for a real gfwlist means avoiding a copy of several hundred kilobytes on
// every state push.
func RuleCountFromPAC(content []byte) int {
	marker := []byte("var rules = ")
	i := bytes.Index(content, marker)
	if i < 0 {
		return 0
	}
	rest := content[i+len(marker):]
	end := bytes.Index(rest, []byte("];"))
	if end < 0 {
		return 0
	}
	var rules []string
	if err := json.Unmarshal(rest[:end+1], &rules); err != nil {
		return 0
	}
	return len(rules)
}

// ParseCacheBustedURL returns the PAC URL without its cache busting query,
// used when deciding whether the installed settings belong to us.
func ParseCacheBustedURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	u.RawQuery = ""
	u.Fragment = ""
	return strings.TrimSuffix(u.String(), "/")
}
