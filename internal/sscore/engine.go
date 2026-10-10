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
	Server    config.ServerProfile
	SocksAddr string // host:port the SOCKS5 proxy listens on
	HTTPAddr  string // host:port the HTTP proxy listens on, empty to disable
	// UDPAddr is the host:port the SOCKS5 UDP ASSOCIATE relay listens on. Empty
	// disables UDP entirely. TCP and UDP are separate port namespaces, so the
	// usual value is the same host:port as SocksAddr.
	UDPAddr     string
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

	// udpPc is the single relay socket every UDP ASSOCIATE client shares; udpAddr
	// is the address clients are told to send to. udpMu guards udpSessions, which
	// is kept separate from mu so datagram forwarding never contends with the
	// configuration lock.
	udpPc       net.PacketConn
	udpAddr     string
	udpMu       sync.Mutex
	udpSessions map[string]*udpSession
	udpUp       bool
	// udpMax is the session cap; a field rather than the constant so tests can
	// exercise the refusal path without opening hundreds of sockets.
	udpMax int

	// done is closed by Stop to release goroutines that are waiting rather than
	// blocked on a socket (the accept backoff, the UDP relay's idle sweep).
	// It is created by Start, so a Stop/Start cycle gets a fresh one.
	done chan struct{}

	up       atomic.Int64
	down     atomic.Int64
	active   atomic.Int64
	sessions atomic.Int64

	connMu sync.Mutex
	open   map[net.Conn]struct{}
}

// NewEngine creates an engine. log may be nil.
func NewEngine(log Logger) *Engine {
	return &Engine{
		log:         log,
		dialTimeout: 15 * time.Second,
		open:        make(map[net.Conn]struct{}),
		udpMax:      udpDefaultMaxSessions,
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
	// UDP is bound on its own AF_INET socket, so binding the same host:port as
	// the SOCKS listener is not a conflict; a failure here means something else
	// already owns the UDP port.
	var udpPc net.PacketConn
	if cfg.UDPAddr != "" {
		udpPc, err = net.ListenPacket("udp", cfg.UDPAddr)
		if err != nil {
			if httpLn != nil {
				_ = httpLn.Close()
			}
			_ = socksLn.Close()
			return fmt.Errorf("无法监听 UDP 端口 %s: %v", cfg.UDPAddr, err)
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
	e.udpPc = udpPc
	if udpPc != nil {
		e.udpAddr = udpPc.LocalAddr().String()
	} else {
		e.udpAddr = ""
	}
	e.udpMu.Lock()
	e.udpSessions = make(map[string]*udpSession)
	e.udpUp = udpPc != nil
	e.udpMu.Unlock()

	e.running = true
	e.done = make(chan struct{})
	done := e.done

	go e.acceptLoop(socksLn, e.handleSOCKS, done)
	if httpLn != nil {
		go e.acceptLoop(httpLn, e.handleHTTP, done)
	}
	if udpPc != nil {
		go e.udpLoop(done)
	}
	e.logf("info", "proxy", "已启动：SOCKS5 %s，HTTP %s，UDP %s，出口 %s（%s）",
		e.socksAddr, orNone(e.httpAddr), orNone(e.udpAddr), e.serverAddr, cfg.Server.Method)
	return nil
}

// Stop closes the listeners and every open proxied connection.
func (e *Engine) Stop() {
	e.mu.Lock()
	socksLn, httpLn := e.socksLn, e.httpLn
	e.socksLn, e.httpLn = nil, nil
	udpPc := e.udpPc
	e.udpPc = nil
	e.udpAddr = ""
	wasRunning := e.running
	e.running = false
	done := e.done
	e.done = nil
	e.mu.Unlock()

	// Release goroutines that are sleeping in a backoff before closing the
	// listeners, so they do not have to wait out their timer.
	if done != nil {
		close(done)
	}
	if socksLn != nil {
		_ = socksLn.Close()
	}
	if httpLn != nil {
		_ = httpLn.Close()
	}
	// Closing the relay socket unblocks udpLoop; closing the sessions unblocks
	// their reply loops, which also drop themselves from the session map.
	if udpPc != nil {
		_ = udpPc.Close()
	}
	e.closeUDPSessions(true)
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
	// UDP sessions hold a resolved copy of the old server address, so they have
	// to go; the next datagram rebuilds them against the new one. TCP tunnels
	// already established keep their old destination, as before.
	e.closeUDPSessions(false)
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

// UDPAddr returns the resolved UDP relay address, or "" when UDP is off.
func (e *Engine) UDPAddr() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.udpAddr
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

// acceptLoop accepts connections until the listener is closed.
//
// A persistent Accept error (EMFILE/ENFILE when the process is out of file
// descriptors, or a transient kernel failure) would otherwise spin this loop at
// full speed and write a log line per iteration. Consecutive failures therefore
// back off and are logged on a decreasing cadence instead of every time; the
// loop still recovers immediately once a connection arrives.
func (e *Engine) acceptLoop(ln net.Listener, handle func(net.Conn), done <-chan struct{}) {
	const (
		minBackoff = 5 * time.Millisecond
		maxBackoff = time.Second
	)
	backoff := time.Duration(0)
	failures := 0
	for {
		c, err := ln.Accept()
		if err != nil {
			if isClosedErr(err) {
				return
			}
			failures++
			if backoff == 0 {
				backoff = minBackoff
			} else if backoff *= 2; backoff > maxBackoff {
				backoff = maxBackoff
			}
			// Log the first failure and then roughly once per second, so a long
			// outage stays visible without flooding the log file.
			if failures == 1 || backoff == maxBackoff {
				e.logf("warn", "proxy", "accept 失败（连续 %d 次）: %v", failures, err)
			}
			select {
			case <-time.After(backoff):
			case <-done:
				return
			}
			continue
		}
		backoff, failures = 0, 0
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
		e.logf("debug", "socks", "握手失败 %s: %v", client.RemoteAddr(), err)
		return
	}
	if req.Cmd == cmdUDPAssoc {
		e.serveUDPAssociate(client)
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
	cnt := &counters{globUp: &e.up, globDown: &e.down}

	tunnel, err := e.tunnel(target)
	if reply != nil {
		reply(replyForError(err))
	}
	if err != nil {
		e.logf("warn", scope, "%s -> %s 失败: %v", client.RemoteAddr(), target, err)
		return
	}

	e.active.Add(1)
	e.sessions.Add(1)
	e.logf("info", scope, "代理 %s -> %s", target, e.upstream())

	relay(client, tunnel, cnt)

	e.active.Add(-1)
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
