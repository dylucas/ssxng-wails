package sscore

import (
	"net"
	"strconv"
	"time"

	"github.com/shadowsocks/go-shadowsocks2/shadowaead"
	"github.com/shadowsocks/go-shadowsocks2/socks"

	"ssxng/internal/config"
)

// LocalServer is a minimal Shadowsocks server bound to loopback. It exists so
// the application can verify the whole client path (local proxy -> cipher ->
// relay) without needing a real server or a network connection.
//
// It is deliberately small and is not used to serve real traffic.
type LocalServer struct {
	ln   net.Listener
	addr string
}

// StartLocalServer starts a loopback Shadowsocks server for the given profile.
func StartLocalServer(p config.ServerProfile) (*LocalServer, error) {
	ciph, err := newCipher(p.Method, p.Password)
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s := &LocalServer{ln: ln, addr: ln.Addr().String()}
	go s.serve(ciph)
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

// Address returns the loopback host and port it listens on.
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
	if s.ln != nil {
		_ = s.ln.Close()
	}
}
