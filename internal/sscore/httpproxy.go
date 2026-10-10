package sscore

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// handleHTTP serves one HTTP proxy client. Both CONNECT (HTTPS) and plain
// HTTP (absolute-form request line) are supported. We handle a single request
// per connection and close it, which keeps the accounting honest and avoids
// half-closed keep-alive edge cases.
func (e *Engine) handleHTTP(client net.Conn) {
	_ = client.SetReadDeadline(time.Now().Add(30 * time.Second))
	br := bufio.NewReader(client)
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	_ = client.SetReadDeadline(time.Time{})

	if req.Method == http.MethodConnect {
		target := hostPort(req.Host, "443")
		e.serveTunnel(client, target, func(rep byte) {
			if rep == repSuccess {
				_, _ = io.WriteString(client, "HTTP/1.1 200 Connection established\r\n\r\n")
				return
			}
			writeHTTPError(client, http.StatusBadGateway, "无法连接目标服务器")
		}, "http")
		return
	}

	if err := e.forwardHTTP(client, req); err != nil {
		e.logf("debug", "http", "转发失败: %v", err)
	}
}

// forwardHTTP performs an absolute-form (plain HTTP) proxied request.
func (e *Engine) forwardHTTP(client net.Conn, req *http.Request) error {
	target := req.URL.Host
	if target == "" {
		target = req.Host
	}
	target = hostPort(target, "80")

	cnt := &counters{globUp: &e.up, globDown: &e.down}

	tunnel, err := e.tunnel(target)
	if err != nil {
		writeHTTPError(client, http.StatusBadGateway, err.Error())
		return err
	}
	defer tunnel.Close()

	e.active.Add(1)
	e.sessions.Add(1)
	defer e.active.Add(-1)

	// Rewrite the request into origin form for the origin server.
	req.RequestURI = ""
	req.URL.Scheme = ""
	req.URL.Host = ""
	req.Header.Del("Proxy-Connection")
	req.Header.Del("Proxy-Authorization")
	req.Close = true
	req.Header.Set("Connection", "close")

	if err := req.Write(&countingWriter{w: tunnel, add: cnt.addUp}); err != nil {
		return err
	}

	resp, err := http.ReadResponse(bufio.NewReader(tunnel), req)
	if err != nil {
		writeHTTPError(client, http.StatusBadGateway, err.Error())
		return err
	}
	defer resp.Body.Close()
	if err := resp.Write(&countingWriter{w: client, add: cnt.addDown}); err != nil {
		return err
	}
	return nil
}

// countingWriter counts the bytes written through it.
type countingWriter struct {
	w   io.Writer
	add func(int64)
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	if n > 0 && c.add != nil {
		c.add(int64(n))
	}
	return n, err
}

// hostPort adds the default port when the target has none, and rejects
// anything that is not a usable host:port pair.
func hostPort(hostport, defPort string) string {
	hostport = strings.TrimSpace(hostport)
	if hostport == "" {
		return ":" + defPort
	}
	if _, _, err := net.SplitHostPort(hostport); err == nil {
		return hostport
	}
	// Bare IPv6 address (no port) or host without port.
	if strings.Count(hostport, ":") > 1 && !strings.HasPrefix(hostport, "[") {
		return "[" + hostport + "]:" + defPort
	}
	return hostport + ":" + defPort
}

func writeHTTPError(w io.Writer, code int, msg string) {
	body := msg + "\n"
	fmt.Fprintf(w, "HTTP/1.1 %d %s\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Length: %d\r\nConnection: close\r\nX-SSXNG-Error: 1\r\n\r\n%s",
		code, http.StatusText(code), len(body), body)
}
