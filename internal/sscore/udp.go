package sscore

import (
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/shadowsocks/go-shadowsocks2/shadowaead"
	"github.com/shadowsocks/go-shadowsocks2/socks"
)

// SOCKS5 UDP ASSOCIATE forwarding (RFC 1928 section 7) over a Shadowsocks
// tunnel. QUIC, DNS and WebRTC all need it; without UDP those connections fall
// back to TCP or fail outright.
//
// The wire formats involved, in order:
//
//	client -> us   RSV(2) FRAG(1) ATYP DST.ADDR DST.PORT DATA   (RFC 1928)
//	us -> server   ATYP DST.ADDR DST.PORT DATA, AEAD sealed      (Shadowsocks)
//	server -> us   ATYP SRC.ADDR SRC.PORT DATA, AEAD sealed
//	us -> client   RSV(2) FRAG(1) ATYP SRC.ADDR SRC.PORT DATA
//
// So the three leading bytes are stripped on the way out and re-added on the
// way back, while the address header stays inside the encrypted payload. The
// server demultiplexes by that header, which is why one encrypted socket per
// client can carry every destination that client talks to.
const (
	// udpBufSize is the datagram buffer go-shadowsocks2's own relay uses.
	udpBufSize = 64 << 10

	// udpAEADOverhead is the authentication tag length. shadowaead's Cipher
	// interface does not expose it and both ciphers we support use 16 bytes.
	udpAEADOverhead = 16

	// udpIdleTimeout is how long a client session may stay quiet before its
	// encrypted socket is recycled. It matches the upstream default so a NAT
	// binding on the server side outlives the session that created it.
	udpIdleTimeout = 60 * time.Second

	// udpDefaultMaxSessions bounds the number of client addresses with a live
	// encrypted socket, so a misbehaving client cannot exhaust descriptors.
	udpDefaultMaxSessions = 256

	// udpSocksHeader is RSV(2) + FRAG(1).
	udpSocksHeader = 3
)

// udpReadSize is the buffer used to read one SOCKS5 UDP request. It is sized so
// that even a maximum-length datagram (header + address + a full udpBufSize of
// payload) is read whole rather than silently truncated; datagrams that would
// not fit through shadowaead are then rejected explicitly in forward.
const udpReadSize = udpSocksHeader + socks.MaxAddrLen + udpBufSize

// udpSession is one client address's encrypted socket to the Shadowsocks
// server. Every destination that client sends to is multiplexed over it.
type udpSession struct {
	e      *Engine
	key    string
	client net.Addr
	// listen is the shared relay socket, used to send replies back to client.
	listen net.PacketConn
	// server is the resolved Shadowsocks server address.
	server *net.UDPAddr
	// enc seals and opens datagrams; it owns a udpBufSize write buffer.
	enc net.PacketConn

	// maxPlain is the largest address+payload this session may hand to enc.
	maxPlain int

	// cntr feeds both the engine totals and this session's own counters.
	cntr *counters

	last atomic.Int64 // unix nanoseconds of the last activity in either direction

	// tx and rx are scratch buffers. Only the engine's single udpLoop goroutine
	// calls forward, and exactly one replyLoop goroutine calls readReply, so
	// neither buffer is ever used concurrently.
	tx []byte
	rx []byte

	closeOnce sync.Once
}

// serveUDPAssociate answers a UDP ASSOCIATE request and then holds the control
// connection open, as RFC 1928 requires clients to keep it for the lifetime of
// the association.
func (e *Engine) serveUDPAssociate(client net.Conn) {
	e.mu.Lock()
	pc, addr := e.udpPc, e.udpAddr
	e.mu.Unlock()

	if pc == nil {
		// UDP is switched off in the settings: say so in the protocol's own
		// terms rather than dropping the connection.
		_, _ = client.Write(replyBytes(repCmdNotSupported))
		e.logf("info", "socks", "%v（客户端 %s）", errSocksUDPUnsupported, client.RemoteAddr())
		return
	}
	// The bound address goes back in the reply: everything except 127.0.0.1
	// (the wildcard, or a hostname the client cannot resolve) would leave the
	// client with nowhere to send.
	if _, err := client.Write(replyBytesAddr(repSuccess, addr)); err != nil {
		e.logf("debug", "socks", "UDP ASSOCIATE 应答失败 %s: %v", client.RemoteAddr(), err)
		return
	}
	e.logf("info", "socks", "UDP ASSOCIATE %s -> %s", client.RemoteAddr(), addr)

	// Datagrams are keyed by source address rather than by this connection,
	// because every client shares one relay port and a client may hold several
	// associations. Sessions are therefore reaped by idleness, not by this
	// connection closing - the same trade-off go-shadowsocks2 makes. Draining
	// the connection keeps it alive for clients that expect that.
	buf := make([]byte, 64)
	for {
		if _, err := client.Read(buf); err != nil {
			return
		}
	}
}

// udpLoop is the single reader on the shared relay socket. It forwards each
// SOCKS5 UDP request it receives.
//
// buf is reused for every datagram; handleUDPDatagram must not retain it. That
// holds because forwarding seals the payload into the encrypted socket's own
// buffer before returning.
func (e *Engine) udpLoop(done <-chan struct{}) {
	e.mu.Lock()
	pc := e.udpPc
	e.mu.Unlock()
	if pc == nil {
		return
	}
	buf := make([]byte, udpReadSize)
	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			if isClosedErr(err) {
				return
			}
			e.logf("debug", "udp", "读取失败: %v", err)
			// A permanently failing socket must not spin this loop.
			select {
			case <-time.After(20 * time.Millisecond):
			case <-done:
				return
			}
			continue
		}
		e.handleUDPDatagram(from, buf[:n])
	}
}

// handleUDPDatagram parses one SOCKS5 UDP request and forwards its payload.
func (e *Engine) handleUDPDatagram(from net.Addr, pkt []byte) {
	if len(pkt) < udpSocksHeader {
		return
	}
	if pkt[2] != 0 {
		// Fragmentation is optional for a SOCKS5 server and no mainstream client
		// emits it; honouring it would mean reassembling datagrams here.
		e.logf("debug", "udp", "丢弃分片数据报（FRAG=%d）", pkt[2])
		return
	}
	addr := socks.SplitAddr(pkt[udpSocksHeader:])
	if addr == nil {
		e.logf("debug", "udp", "丢弃地址无法解析的数据报（%d 字节）", len(pkt))
		return
	}
	payload := pkt[udpSocksHeader+len(addr):]

	sess := e.udpSessionFor(from)
	if sess == nil {
		return
	}
	sess.forward(addr, payload)
}

// udpSessionFor returns the session for a client address, creating it (and its
// encrypted socket) on first use.
func (e *Engine) udpSessionFor(from net.Addr) *udpSession {
	e.mu.Lock()
	ciph, serverAddr, listen := e.cipher, e.serverAddr, e.udpPc
	e.mu.Unlock()
	if ciph == nil || listen == nil {
		return nil
	}

	key := from.String()
	e.udpMu.Lock()
	// Stop may have run between the check above and this lock; refuse then, so a
	// session cannot be added to the map after it was already drained.
	if !e.udpUp {
		e.udpMu.Unlock()
		return nil
	}
	if s := e.udpSessions[key]; s != nil {
		e.udpMu.Unlock()
		return s
	}
	if len(e.udpSessions) >= e.udpMax {
		e.udpMu.Unlock()
		e.logf("warn", "udp", "UDP 会话数已达上限 %d，丢弃来自 %s 的数据报", e.udpMax, key)
		return nil
	}
	// Bind the outbound socket before taking the map entry, so a failure here
	// cannot leave a half-built session behind.
	raw, err := net.ListenPacket("udp", "")
	if err != nil {
		e.udpMu.Unlock()
		e.logf("warn", "udp", "无法创建 UDP 会话套接字: %v", err)
		return nil
	}
	server, err := net.ResolveUDPAddr("udp", serverAddr)
	if err != nil {
		e.udpMu.Unlock()
		_ = raw.Close()
		e.logf("warn", "udp", "无法解析服务器地址 %s: %v", serverAddr, err)
		return nil
	}
	s := &udpSession{
		e:        e,
		key:      key,
		client:   from,
		listen:   listen,
		server:   server,
		enc:      shadowaead.NewPacketConn(raw, ciph),
		maxPlain: udpBufSize - ciph.SaltSize() - udpAEADOverhead,
		cntr:     &counters{globUp: &e.up, globDown: &e.down},
		tx:       make([]byte, 0, udpBufSize),
		rx:       make([]byte, 0, udpBufSize+udpSocksHeader),
	}
	s.last.Store(time.Now().UnixNano())
	e.udpSessions[key] = s
	e.udpMu.Unlock()

	e.active.Add(1)
	e.sessions.Add(1)
	go s.replyLoop()
	return s
}

// forward seals one destination address plus payload and sends it to the
// server. Only the engine's udpLoop goroutine calls it.
func (s *udpSession) forward(addr socks.Addr, payload []byte) {
	s.touch()
	plain := len(addr) + len(payload)
	if plain > s.maxPlain {
		s.e.logf("debug", "udp", "丢弃过大或截断的数据报（%d 字节，上限 %d）", plain, s.maxPlain)
		return
	}
	pkt := append(s.tx[:0], addr...)
	pkt = append(pkt, payload...)
	if _, err := s.enc.WriteTo(pkt, s.server); err != nil {
		s.e.logf("debug", "udp", "发送到 %s 失败: %v", s.server, err)
		return
	}
	s.cntr.addUp(int64(len(payload)))
}

// replyLoop pushes server replies back to the client, adding the RSV/FRAG
// header RFC 1928 requires, until the session goes idle or is closed.
func (s *udpSession) replyLoop() {
	defer s.e.dropUDPSession(s)

	buf := make([]byte, udpBufSize)
	for {
		// A deadline equal to the idle window both paces the loop and reaps
		// sessions whose server never answers.
		_ = s.enc.SetReadDeadline(time.Now().Add(udpIdleTimeout))
		n, _, err := s.enc.ReadFrom(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				// Keep sessions that are still sending but not receiving:
				// one-way flows (syslog, some media) are legitimate.
				if time.Since(time.Unix(0, s.last.Load())) < udpIdleTimeout {
					continue
				}
				s.e.logf("debug", "udp", "会话 %s 空闲超时，回收（↑%s ↓%s）",
					s.client, humanBytes(s.cntr.connUp.Load()), humanBytes(s.cntr.connDown.Load()))
			}
			return
		}
		// The decrypted packet carries its own source address; without one the
		// client would receive a header it cannot interpret.
		addr := socks.SplitAddr(buf[:n])
		if addr == nil {
			s.e.logf("debug", "udp", "丢弃来自服务器的畸形数据报（%d 字节）", n)
			continue
		}
		out := append(s.rx[:0], 0, 0, 0)
		out = append(out, buf[:n]...)
		if _, err := s.listen.WriteTo(out, s.client); err != nil {
			return
		}
		s.touch()
		s.cntr.addDown(int64(n - len(addr)))
	}
}

func (s *udpSession) touch() { s.last.Store(time.Now().UnixNano()) }

// close releases the session's socket exactly once. It tolerates a session
// whose socket was never created, so teardown can never panic on a half-built
// entry.
func (s *udpSession) close() {
	s.closeOnce.Do(func() {
		if s.enc != nil {
			_ = s.enc.Close()
		}
	})
}

// dropUDPSession removes a session from the engine and releases it.
func (e *Engine) dropUDPSession(s *udpSession) {
	e.udpMu.Lock()
	// Only claim our own entry: a newer session for the same client address may
	// already have replaced it, and closeUDPSessions may already have drained
	// the map. Whoever removes the entry owns the counter decrement.
	mine := e.udpSessions[s.key] == s
	if mine {
		delete(e.udpSessions, s.key)
	}
	e.udpMu.Unlock()

	s.close()
	if mine {
		e.active.Add(-1)
	}
}

// closeUDPSessions tears down every session. stopRelay distinguishes the two
// callers: Stop passes true, so nothing can create a session afterwards, while
// a server swap passes false because the next datagram must rebuild one against
// the new server.
func (e *Engine) closeUDPSessions(stopRelay bool) {
	e.udpMu.Lock()
	if stopRelay {
		e.udpUp = false
	}
	sessions := make([]*udpSession, 0, len(e.udpSessions))
	for _, s := range e.udpSessions {
		sessions = append(sessions, s)
	}
	e.udpSessions = make(map[string]*udpSession)
	e.udpMu.Unlock()

	// close() unblocks each reply loop, which then calls dropUDPSession; that
	// call no longer finds its entry and so leaves the counter alone.
	for _, s := range sessions {
		s.close()
		e.active.Add(-1)
	}
}
