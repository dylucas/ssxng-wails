package diagnose

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"time"

	"github.com/shadowsocks/go-shadowsocks2/socks"

	"ssxng/internal/config"
	"ssxng/internal/sscore"
)

// Step is one self test assertion.
type Step struct {
	Name   string
	OK     bool
	Detail string
}

// Report is the self test result.
type Report struct {
	OK        bool
	Steps     []Step
	ElapsedMS int64
}

const echoBody = "ssxng-selftest"

// SelfTest runs the client end to end: it starts a throwaway Shadowsocks server
// and an HTTP echo server on loopback, brings up a temporary copy of the real
// engine, and pushes traffic through the SOCKS5 and HTTP front ends.
func SelfTest(c Context) (rep Report) {
	started := time.Now()
	rep.OK = true

	var logMu sync.Mutex
	var logLines []string
	record := func(level, scope, msg string) {
		if level != "error" && level != "warn" {
			return
		}
		logMu.Lock()
		defer logMu.Unlock()
		logLines = append(logLines, scope+": "+msg)
		if len(logLines) > 20 {
			logLines = logLines[len(logLines)-20:]
		}
	}
	lastLog := func() string {
		logMu.Lock()
		defer logMu.Unlock()
		if len(logLines) == 0 {
			return ""
		}
		return logLines[len(logLines)-1]
	}
	add := func(name string, ok bool, detail string) {
		if !ok {
			rep.OK = false
			if extra := lastLog(); extra != "" {
				detail += "；引擎日志: " + extra
			}
		}
		rep.Steps = append(rep.Steps, Step{Name: name, OK: ok, Detail: detail})
	}
	defer func() { rep.ElapsedMS = time.Since(started).Milliseconds() }()

	// 1. configuration folder
	if err := dirWritable(c.ConfigDir); err != nil {
		add("配置目录可写", false, err.Error())
		return
	}
	add("配置目录可写", true, c.ConfigDir)

	// 2. throwaway Shadowsocks server
	password, err := randomHex(16)
	if err != nil {
		add("启动本地测试服务端", false, err.Error())
		return
	}
	profile := config.ServerProfile{Host: "127.0.0.1", Port: 1, Method: "aes-256-gcm", Password: password}
	srv, err := sscore.StartLocalServer(profile)
	if err != nil {
		add("启动本地测试服务端", false, err.Error())
		return
	}
	defer srv.Close()
	host, port := srv.Address()
	add("启动本地测试服务端", true, fmt.Sprintf("127.0.0.1:%d（aes-256-gcm，一次性密码）", port))

	// 3. echo servers
	echo := httptest.NewServer(http.HandlerFunc(echoHandler))
	defer echo.Close()
	tlsEcho := httptest.NewTLSServer(http.HandlerFunc(echoHandler))
	defer tlsEcho.Close()

	// 4. a temporary copy of the real engine
	profile.Host, profile.Port = host, port
	eng := sscore.NewEngine(record)
	if err := eng.Start(sscore.Config{
		Server:      profile,
		SocksAddr:   "127.0.0.1:0",
		HTTPAddr:    "127.0.0.1:0",
		UDPAddr:     "127.0.0.1:0",
		DialTimeout: 5 * time.Second,
	}); err != nil {
		add("启动临时代理", false, err.Error())
		return
	}
	defer eng.Stop()
	socksAddr, httpAddr := eng.Addresses()
	udpAddr := eng.UDPAddr()
	add("启动临时代理", true, fmt.Sprintf("SOCKS %s，HTTP %s，UDP %s", socksAddr, httpAddr, udpAddr))

	socksClient := &http.Client{
		Transport: &http.Transport{DialContext: socks5Dialer(socksAddr)},
		Timeout:   15 * time.Second,
	}
	proxyURL, _ := url.Parse("http://" + httpAddr)
	proxyClient := &http.Client{
		Transport: &http.Transport{
			Proxy:           http.ProxyURL(proxyURL),
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
		Timeout: 15 * time.Second,
	}

	// 5. SOCKS5
	body, err := fetch(socksClient, echo.URL+"/socks")
	add("SOCKS5 转发", err == nil && body == echoBody, describe(err, fmt.Sprintf("收到 %q", truncate(body))))

	// 6. HTTP proxy, absolute form
	body, err = fetch(proxyClient, echo.URL+"/http")
	add("HTTP 代理转发", err == nil && body == echoBody, describe(err, fmt.Sprintf("收到 %q", truncate(body))))

	// 7. HTTP proxy, CONNECT tunnel to a TLS origin
	body, err = fetch(proxyClient, tlsEcho.URL+"/connect")
	add("HTTPS CONNECT 隧道", err == nil && body == echoBody, describe(err, fmt.Sprintf("收到 %q", truncate(body))))

	// 8. UDP ASSOCIATE: the relay must carry a datagram to a UDP echo server
	// through the tunnel and bring the reply back with its SOCKS5 header.
	udpEcho, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		add("启动 UDP 回声服务端", false, err.Error())
	} else {
		defer udpEcho.Close()
		go udpEchoLoop(udpEcho)
		add("启动 UDP 回声服务端", true, udpEcho.LocalAddr().String())
		body, err := udpRoundTrip(socksAddr, udpEcho.LocalAddr().String(), []byte(echoBody))
		add("UDP 转发", err == nil && body == "udp:"+echoBody,
			describe(err, fmt.Sprintf("收到 %q", truncate(body))))
	}

	// 9. payload integrity across many cipher chunks
	payload := make([]byte, 256<<10)
	if _, err := rand.Read(payload); err != nil {
		add("大数据完整性（256 KiB）", false, err.Error())
	} else {
		got, perr := post(socksClient, echo.URL+"/echo", payload)
		switch {
		case perr != nil:
			add("大数据完整性（256 KiB）", false, perr.Error())
		case !bytes.Equal(got, payload):
			add("大数据完整性（256 KiB）", false, fmt.Sprintf("发送 %d 字节，收到 %d 字节", len(payload), len(got)))
		default:
			add("大数据完整性（256 KiB）", true, fmt.Sprintf("%d 字节校验一致", len(got)))
		}
	}

	// 10. shutdown leaves nothing behind
	eng.Stop()
	srv.Close()
	ok := true
	detail := "临时服务端与代理已停止"
	for _, addr := range []string{socksAddr, httpAddr} {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			ok = false
			detail = fmt.Sprintf("端口 %s 未释放: %v", addr, err)
			break
		}
		_ = ln.Close()
	}
	// The UDP relay shares the SOCKS port; it must be released too, or a restart
	// would fail to bind.
	if ok {
		pc, err := net.ListenPacket("udp", udpAddr)
		if err != nil {
			ok = false
			detail = fmt.Sprintf("UDP 端口 %s 未释放: %v", udpAddr, err)
		} else {
			_ = pc.Close()
		}
	}
	add("清理", ok, detail)
	return
}

// udpEchoLoop answers every datagram with a "udp:" prefix, so the self test can
// tell a reply apart from its own request.
func udpEchoLoop(pc net.PacketConn) {
	buf := make([]byte, 64<<10)
	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}
		out := append([]byte("udp:"), buf[:n]...)
		if _, err := pc.WriteTo(out, from); err != nil {
			return
		}
	}
}

// udpRoundTrip runs a complete SOCKS5 UDP ASSOCIATE exchange: greeting,
// associate, one datagram to target, and the reply payload back.
//
// It is a plain, independent client implementation on purpose. Routing the self
// test through the engine's own UDP client code would let a bug there hide
// itself, which is exactly what this test exists to catch.
func udpRoundTrip(socksAddr, target string, payload []byte) (string, error) {
	ctrl, err := net.DialTimeout("tcp", socksAddr, 5*time.Second)
	if err != nil {
		return "", err
	}
	// The association lives on this connection, so it stays open for the whole
	// exchange.
	defer ctrl.Close()
	_ = ctrl.SetDeadline(time.Now().Add(10 * time.Second))

	// Greeting: version 5, one method, "no authentication".
	if _, err := ctrl.Write([]byte{5, 1, 0}); err != nil {
		return "", err
	}
	method := make([]byte, 2)
	if _, err := io.ReadFull(ctrl, method); err != nil {
		return "", err
	}
	if method[0] != 5 || method[1] != 0 {
		return "", fmt.Errorf("SOCKS5 方法协商失败: %v", method)
	}
	// UDP ASSOCIATE with 0.0.0.0:0: our source port does not exist yet.
	if _, err := ctrl.Write([]byte{5, 3, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
		return "", err
	}
	// VER, REP, RSV: the address type is left for socks.ReadAddr, which expects
	// to read it itself.
	head := make([]byte, 3)
	if _, err := io.ReadFull(ctrl, head); err != nil {
		return "", err
	}
	if head[0] != 5 {
		return "", fmt.Errorf("SOCKS5 应答版本错误: %d", head[0])
	}
	if head[1] != 0 {
		return "", fmt.Errorf("UDP ASSOCIATE 被拒绝: REP=%d", head[1])
	}
	bound, err := socks.ReadAddr(ctrl)
	if err != nil {
		return "", fmt.Errorf("读取应答地址失败: %w", err)
	}
	relay, err := net.ResolveUDPAddr("udp", bound.String())
	if err != nil {
		return "", fmt.Errorf("应答地址无效 %q: %w", bound.String(), err)
	}
	if relay.Port == 0 {
		return "", errors.New("应答的 BND.PORT 为 0，客户端无处可发")
	}

	tgt := socks.ParseAddr(target)
	if tgt == nil {
		return "", fmt.Errorf("目标地址无效: %s", target)
	}
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	defer pc.Close()

	pkt := make([]byte, 0, 3+len(tgt)+len(payload))
	pkt = append(pkt, 0, 0, 0) // RSV(2) + FRAG(1)
	pkt = append(pkt, tgt...)
	pkt = append(pkt, payload...)
	if _, err := pc.WriteTo(pkt, relay); err != nil {
		return "", err
	}
	_ = pc.SetReadDeadline(time.Now().Add(10 * time.Second))
	buf := make([]byte, 64<<10)
	n, _, err := pc.ReadFrom(buf)
	if err != nil {
		return "", err
	}
	if n < 4 {
		return "", fmt.Errorf("UDP 应答过短: %d 字节", n)
	}
	if buf[0] != 0 || buf[1] != 0 || buf[2] != 0 {
		return "", fmt.Errorf("UDP 应答的 RSV/FRAG 应为 0: %v", buf[:3])
	}
	src := socks.SplitAddr(buf[3:n])
	if src == nil {
		return "", errors.New("UDP 应答的地址无法解析")
	}
	return string(buf[3+len(src) : n]), nil
}

func echoHandler(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	w.Header().Set("X-Echo-Path", r.URL.Path)
	if len(body) == 0 {
		body = []byte(echoBody)
	}
	_, _ = w.Write(body)
}

func fetch(client *http.Client, raw string) (string, error) {
	resp, err := client.Get(raw)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return string(body), fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return string(body), nil
}

func post(client *http.Client, raw string, payload []byte) ([]byte, error) {
	resp, err := client.Post(raw, "application/octet-stream", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

func describe(err error, ok string) string {
	if err != nil {
		return err.Error()
	}
	return ok
}

func truncate(s string) string {
	if len(s) > 60 {
		return s[:60] + "…"
	}
	return s
}

func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// socks5Dialer returns a DialContext that goes through a SOCKS5 proxy with no
// authentication.
func socks5Dialer(proxyAddr string) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(_ context.Context, _, addr string) (net.Conn, error) {
		c, err := net.DialTimeout("tcp", proxyAddr, 5*time.Second)
		if err != nil {
			return nil, err
		}
		if _, err := c.Write([]byte{5, 1, 0}); err != nil {
			c.Close()
			return nil, err
		}
		reply := make([]byte, 2)
		if _, err := io.ReadFull(c, reply); err != nil {
			c.Close()
			return nil, err
		}
		if reply[0] != 5 || reply[1] != 0 {
			c.Close()
			return nil, fmt.Errorf("SOCKS5 代理拒绝了认证方式")
		}
		a := socks.ParseAddr(addr)
		if a == nil {
			c.Close()
			return nil, fmt.Errorf("无法编码目标地址 %q", addr)
		}
		if _, err := c.Write(append([]byte{5, 1, 0}, a...)); err != nil {
			c.Close()
			return nil, err
		}
		head := make([]byte, 4)
		if _, err := io.ReadFull(c, head); err != nil {
			c.Close()
			return nil, err
		}
		if head[1] != 0 {
			c.Close()
			return nil, fmt.Errorf("SOCKS5 代理返回错误码 %d", head[1])
		}
		var rest int
		switch head[3] {
		case 1:
			rest = 4 + 2
		case 4:
			rest = 16 + 2
		case 3:
			l := make([]byte, 1)
			if _, err := io.ReadFull(c, l); err != nil {
				c.Close()
				return nil, err
			}
			rest = int(l[0]) + 2
		}
		if _, err := io.ReadFull(c, make([]byte, rest)); err != nil {
			c.Close()
			return nil, err
		}
		return c, nil
	}
}
