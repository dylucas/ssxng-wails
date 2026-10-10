package sscore

import (
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/shadowsocks/go-shadowsocks2/shadowaead"
	"github.com/shadowsocks/go-shadowsocks2/socks"

	"ssxng/internal/config"
)

// localServerUDPIdle is how long a loopback server keeps an upstream NAT entry
// alive without traffic. The self test finishes in seconds, so this only exists
// to stop a leaked goroutine outliving the test.
const localServerUDPIdle = 60 * time.Second

// LocalServer is a minimal Shadowsocks server bound to loopback. It exists so
// the application can verify the whole client path (local proxy -> cipher ->
// relay) without needing a real server or a network connection.
//
// It serves both protocols on one port: UDP is bound first and the TCP listener
// then follows it, because a Shadowsocks profile carries a single port and the
// client sends UDP datagrams to the same one.
//
// It is deliberately small and is not used to serve real traffic.
type LocalServer struct {
	ln   net.Listener
	udp  net.PacketConn
	addr string

	mu     sync.Mutex
	closed bool
	// up holds the plaintext sockets used to talk to UDP targets, keyed by the
	// client address that asked for them.
	up map[string]net.PacketConn
}

// StartLocalServer starts a loopback Shadowsocks server for the given profile.
func StartLocalServer(p config.ServerProfile) (*LocalServer, error) {
	ciph, err := newCipher(p.Method, p.Password)
	if err != nil {
		return nil, err
	}
	// Bind UDP first so the TCP listener can adopt the port it picked: the two
	// protocols are separate namespaces, and asking for the same :0 twice would
	// usually hand back two different ports.
	var udp net.PacketConn
	var ln net.Listener
	for attempt := 0; ; attempt++ {
		udp, err = net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		ln, err = net.Listen("tcp", udp.LocalAddr().String())
		if err == nil {
			break
		}
		_ = udp.Close()
		if attempt >= 5 {
			return nil, err
		}
	}

	s := &LocalServer{ln: ln, udp: udp, addr: ln.Addr().String(), up: map[string]net.PacketConn{}}
	go s.serve(ciph)
	go s.serveUDP(ciph)
	return s, nil
}

func (s *LocalServer) serve(ciph shadowaead.Cipher) {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			defer c.Close()
			sc := shadowaead.NewConn(c, ciph)
			tgt, err := socks.ReadAddr(sc)
			if err != nil {
				return
			}
			rc, err := net.DialTimeout("tcp", tgt.String(), 5*time.Second)
			if err != nil {
				return
			}
			defer rc.Close()
			relay(rc, sc, &counters{})
		}(c)
	}
}

// serveUDP is the server half of the relay: it decrypts one datagram, reads the
// target address out of it, forwards the payload, and sends the reply back with
// the address it came from - exactly what a real Shadowsocks server does.
func (s *LocalServer) serveUDP(ciph shadowaead.Cipher) {
	pc := shadowaead.NewPacketConn(s.udp, ciph)
	buf := make([]byte, 64<<10)
	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}
		tgt := socks.SplitAddr(buf[:n])
		if tgt == nil {
			continue
		}
		tgtAddr, err := net.ResolveUDPAddr("udp", tgt.String())
		if err != nil {
			continue
		}
		out := s.upstreamFor(pc, from)
		if out == nil {
			continue
		}
		_, _ = out.WriteTo(buf[len(tgt):n], tgtAddr)
	}
}

// upstreamFor returns the plaintext socket for one client address, creating it
// (and its reply pump) on first use. replies is the encrypted relay socket the
// pump must answer through.
func (s *LocalServer) upstreamFor(replies net.PacketConn, from net.Addr) net.PacketConn {
	key := from.String()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	if pc := s.up[key]; pc != nil {
		return pc
	}
	pc, err := net.ListenPacket("udp", "")
	if err != nil {
		return nil
	}
	s.up[key] = pc
	go s.replyUDP(replies, from, pc)
	return pc
}

// replyUDP pumps target replies back to the client, prefixing each one with the
// address it came from so the client's relay can rebuild the SOCKS5 header.
func (s *LocalServer) replyUDP(replies net.PacketConn, client net.Addr, pc net.PacketConn) {
	defer func() {
		_ = pc.Close()
		s.mu.Lock()
		delete(s.up, client.String())
		s.mu.Unlock()
	}()
	buf := make([]byte, 64<<10)
	for {
		_ = pc.SetReadDeadline(time.Now().Add(localServerUDPIdle))
		n, tgt, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}
		src := socks.ParseAddr(tgt.String())
		if src == nil {
			continue
		}
		pkt := append(src, buf[:n]...)
		if _, err := replies.WriteTo(pkt, client); err != nil {
			return
		}
	}
}

// Address returns the loopback host and port it listens on. TCP and UDP share
// it, so the address is valid for both.
func (s *LocalServer) Address() (string, int) {
	host, portStr, err := net.SplitHostPort(s.addr)
	if err != nil {
		return "127.0.0.1", 0
	}
	port, _ := strconv.Atoi(portStr)
	return host, port
}

// Close stops the server.
func (s *LocalServer) Close() {
	s.mu.Lock()
	s.closed = true
	upstreams := make([]net.PacketConn, 0, len(s.up))
	for _, pc := range s.up {
		upstreams = append(upstreams, pc)
	}
	s.up = map[string]net.PacketConn{}
	s.mu.Unlock()

	if s.ln != nil {
		_ = s.ln.Close()
	}
	if s.udp != nil {
		_ = s.udp.Close()
	}
	for _, pc := range upstreams {
		_ = pc.Close()
	}
}
