package sscore

import (
	"io"
	"net"
	"sync"
)

// halfCloser is implemented by *net.TCPConn and lets us propagate a half close
// so that protocols relying on EOF (plain HTTP proxying) work.
type halfCloser interface{ CloseWrite() error }

func closeWrite(c net.Conn) {
	if cw, ok := c.(halfCloser); ok {
		_ = cw.CloseWrite()
	}
}

// tunnelConn is the Shadowsocks-encrypted stream. It keeps a handle on the
// underlying TCP connection so relay can half-close it: the AEAD wrapper only
// exposes the net.Conn interface, which has no CloseWrite.
type tunnelConn struct {
	net.Conn
	raw net.Conn
}

func (t *tunnelConn) CloseWrite() error {
	if cw, ok := t.raw.(halfCloser); ok {
		return cw.CloseWrite()
	}
	return nil
}

// countingReader counts plaintext bytes as they are copied, so the UI can show
// live throughput instead of only updating when a connection ends.
type countingReader struct {
	r   io.Reader
	add func(int64)
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 && c.add != nil {
		c.add(int64(n))
	}
	return n, err
}

// relay copies data in both directions until both sides are done.
//
// up counts bytes the client sent, down counts bytes sent back to it. Counting
// the plaintext on the client side of the tunnel keeps the numbers meaningful:
// the encrypted side carries a little more.
func relay(client net.Conn, tunnel net.Conn, c *counters) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(tunnel, &countingReader{r: client, add: c.addUp})
		closeWrite(tunnel)
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(client, &countingReader{r: tunnel, add: c.addDown})
		closeWrite(client)
	}()
	wg.Wait()
	_ = client.Close()
	_ = tunnel.Close()
}
