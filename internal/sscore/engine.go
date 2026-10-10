package sscore

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/shadowsocks/go-shadowsocks2/shadowaead"
	"github.com/shadowsocks/go-shadowsocks2/socks"

	"ssxng/internal/config"
)

// Logger receives human readable engine messages (already localised).
type Logger func(level, scope, message string)

// Config is the runtime configuration of the engine.
type Config struct {
	Server      config.ServerProfile
	SocksAddr   string // host:port the SOCKS5 proxy listens on
	HTTPAddr    string // host:port the HTTP proxy listens on, empty to disable
	DialTimeout time.Duration
}

// Stats is a cumulative traffic snapshot.
type Stats struct {
	Up       int64
	Down     int64
	Active   int64
	Sessions int64
}

// Engine runs the local SOCKS5/HTTP proxies and tunnels through one server.
type Engine struct {
	log         Logger
	dialTimeout time.Duration

	mu         sync.Mutex
	serverAddr string
	cipher     shadowaead.Cipher
	socksLn    net.Listener
	httpLn     net.Listener
	httpAddr   string
	socksAddr  string
	running    bool
	lastErr    string

	up       atomic.Int64
	down     atomic.Int64
	active   atomic.Int64
	sessions atomic.Int64

	conns *connTable

	connMu sync.Mutex
	open   map[net.Conn]struct{}
}

// NewEngine creates an engine. log may be nil.
func NewEngine(log Logger) *Engine {
	return &Engine{
		log:         log,
		dialTimeout: 15 * time.Second,
		conns:       newConnTable(200),
		open:        make(map[net.Conn]struct{}),
	}
}

func (e *Engine) logf(level, scope, format string, args ...any) {
	if e.log == nil {
		return
	}
	e.log(level, scope, fmt.Sprintf(format, args...))
}

// Start binds the listeners and starts serving. It is transactional: if either
// listener cannot be bound, nothing is left running and the error names the
// conflicting port.
func (e *Engine) Start(cfg Config) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.running {
		return errors.New("代理已经在运行")
	}
	ciph, err := newCipher(cfg.Server.Method, cfg.Server.Password)
	if err != nil {
		return err
	}
	if cfg.DialTimeout > 0 {
		e.dialTimeout = cfg.DialTimeout
	}

	var socksLn, httpLn net.Listener
	socksLn, err = net.Listen("tcp", cfg.SocksAddr)
	if err != nil {
		return fmt.Errorf("无法监听 SOCKS5 端口 %s: %v", cfg.SocksAddr, err)
	}
	if cfg.HTTPAddr != "" {
		httpLn, err = net.Listen("tcp", cfg.HTTPAddr)
		if err != nil {
			_ = socksLn.Close()
			return fmt.Errorf("无法监听 HTTP 端口 %s: %v", cfg.HTTPAddr, err)
		}
	}

	e.cipher = ciph
	e.serverAddr = net.JoinHostPort(cfg.Server.Host, fmt.Sprint(cfg.Server.Port))
	e.socksLn = socksLn
	e.httpLn = httpLn
	// Record the addresses the kernel actually bound (port 0 means "any
	// free port", which the UI and the self test need resolved).
	e.socksAddr = socksLn.Addr().String()
	if httpLn != nil {
		e.httpAddr = httpLn.Addr().String()
	} else {
		e.httpAddr = ""
	}
	e.running = true
	e.lastErr = ""

	go e.acceptLoop(socksLn, e.handleSOCKS)
	if httpLn != nil {
		go e.acceptLoop(httpLn, e.handleHTTP)
	}
	e.logf("info", "proxy", "已启动：SOCKS5 %s，HTTP %s，出口 %s（%s）",
		e.socksAddr, orNone(e.httpAddr), e.serverAddr, cfg.Server.Method)
	return nil
}

// Stop closes the listeners and every open proxied connection.
func (e *Engine) Stop() {
	e.mu.Lock()
	socksLn, httpLn := e.socksLn, e.httpLn
	e.socksLn, e.httpLn = nil, nil
	wasRunning := e.running
	e.running = false
	e.mu.Unlock()

	if socksLn != nil {
		_ = socksLn.Close()
	}
	if httpLn != nil {
		_ = httpLn.Close()
	}
	e.closeOpen()
	if wasRunning {
		e.logf("info", "proxy", "已停止本地代理")
	}
}

// SetServer swaps the upstream server without dropping the listeners or the
// connections that are already established.
func (e *Engine) SetServer(p config.ServerProfile) error {
	ciph, err := newCipher(p.Method, p.Password)
	if err != nil {
		return err
	}
	e.mu.Lock()
	e.cipher = ciph
	e.serverAddr = net.JoinHostPort(p.Host, fmt.Sprint(p.Port))
	e.mu.Unlock()
	e.logf("info", "proxy", "已切换出口服务器：%s（%s）", e.serverAddr, p.Method)
	return nil
}

// Running reports whether the listeners are up.
func (e *Engine) Running() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.running
}

// Addresses returns the listeners currently in use.
func (e *Engine) Addresses() (socks string, http string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.socksAddr, e.httpAddr
}

// LastError returns the most recent listener level error.
func (e *Engine) LastError() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.lastErr
}

// Stats returns the cumulative counters.
func (e *Engine) Stats() Stats {
	return Stats{
		Up:       e.up.Load(),
		Down:     e.down.Load(),
		Active:   e.active.Load(),
		Sessions: e.sessions.Load(),
	}
}

// ResetStats clears the traffic counters.
func (e *Engine) ResetStats() {
	e.up.Store(0)
	e.down.Store(0)
	e.sessions.Store(0)
}

// Connections returns the most recent connections, newest first.
func (e *Engine) Connections(max int) []ConnInfo {
	return e.conns.snapshot(max)
}

// ClearConnections drops the connection history.
func (e *Engine) ClearConnections() { e.conns.clear() }

// acceptLoop accepts connections until the listener is closed.
func (e *Engine) acceptLoop(ln net.Listener, handle func(net.Conn)) {
	for {
		c, err := ln.Accept()
		if err != nil {
			if isClosedErr(err) {
				return
			}
			e.logf("warn", "proxy", "accept 失败: %v", err)
			continue
		}
		if tc, ok := c.(*net.TCPConn); ok {
			_ = tc.SetKeepAlive(true)
			_ = tc.SetKeepAlivePeriod(30 * time.Second)
		}
		e.track(c)
		go func() {
			defer e.untrack(c)
			handle(c)
		}()
	}
}

func (e *Engine) track(c net.Conn) {
	e.connMu.Lock()
	e.open[c] = struct{}{}
	e.connMu.Unlock()
}

func (e *Engine) untrack(c net.Conn) {
	e.connMu.Lock()
	delete(e.open, c)
	e.connMu.Unlock()
	_ = c.Close()
}

func (e *Engine) closeOpen() {
	e.connMu.Lock()
	conns := make([]net.Conn, 0, len(e.open))
	for c := range e.open {
		conns = append(conns, c)
	}
	e.open = make(map[net.Conn]struct{})
	e.connMu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
}

// handleSOCKS serves one SOCKS5 client connection.
func (e *Engine) handleSOCKS(client net.Conn) {
	req, err := readSocksRequest(client)
	if err != nil {
		if !errors.Is(err, errSocksUDPUnsupported) {
			e.logf("debug", "socks", "握手失败 %s: %v", client.RemoteAddr(), err)
		} else {
			e.logf("info", "socks", "客户端请求 UDP ASSOCIATE，本版本暂不支持")
		}
		return
	}
	e.serveTunnel(client, req.Target, func(rep byte) {
		_, _ = client.Write(replyBytes(rep))
		if rep != repSuccess {
			_ = client.Close()
		}
	}, "socks")
}

// serveTunnel dials the upstream and relays until either side finishes.
// reply is called exactly once, before any payload is exchanged.
func (e *Engine) serveTunnel(client net.Conn, target string, reply func(rep byte), scope string) {
	id, _ := e.conns.begin(target)
	cnt := &counters{globUp: &e.up, globDown: &e.down}

	tunnel, err := e.tunnel(target)
	if reply != nil {
		reply(replyForError(err))
	}
	if err != nil {
		e.active.Add(0)
		e.conns.end(id, 0, 0, err.Error())
		e.logf("warn", scope, "%s -> %s 失败: %v", client.RemoteAddr(), target, err)
		return
	}

	e.active.Add(1)
	e.sessions.Add(1)
	e.logf("info", scope, "代理 %s -> %s", target, e.upstream())

	relay(client, tunnel, cnt)

	e.active.Add(-1)
	e.conns.end(id, cnt.connUp.Load(), cnt.connDown.Load(), "")
	e.logf("debug", scope, "结束 %s，↑%s ↓%s", target, humanBytes(cnt.connUp.Load()), humanBytes(cnt.connDown.Load()))
}

// tunnel opens a Shadowsocks stream to target ("host:port").
func (e *Engine) tunnel(target string) (net.Conn, error) {
	e.mu.Lock()
	ciph, serverAddr, timeout := e.cipher, e.serverAddr, e.dialTimeout
	e.mu.Unlock()

	if ciph == nil {
		return nil, errors.New("没有可用的服务器")
	}
	addr := socks.ParseAddr(target)
	if addr == nil {
		return nil, fmt.Errorf("无法解析目标地址 %q", target)
	}
	raw, err := net.DialTimeout("tcp", serverAddr, timeout)
	if err != nil {
		return nil, fmt.Errorf("连接服务器 %s 失败: %w", serverAddr, err)
	}
	if tc, ok := raw.(*net.TCPConn); ok {
		_ = tc.SetKeepAlive(true)
		_ = tc.SetKeepAlivePeriod(30 * time.Second)
	}
	enc := shadowaead.NewConn(raw, ciph)
	conn := &tunnelConn{Conn: enc, raw: raw}
	if _, err := conn.Write(addr); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("发送目标地址失败: %w", err)
	}
	return conn, nil
}

func (e *Engine) upstream() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.serverAddr
}

func isClosedErr(err error) bool {
	return errors.Is(err, net.ErrClosed) || strings.Contains(err.Error(), "use of closed network connection")
}

func orNone(s string) string {
	if s == "" {
		return "关闭"
	}
	return s
}

// humanBytes renders a byte count for logs.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	units := []string{"KiB", "MiB", "GiB", "TiB"}
	f := float64(n)
	i := -1
	for f >= unit && i < len(units)-1 {
		f /= unit
		i++
	}
	return fmt.Sprintf("%.1f%s", f, units[i])
}
