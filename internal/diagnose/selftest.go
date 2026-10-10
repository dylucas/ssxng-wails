package diagnose

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
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
		DialTimeout: 5 * time.Second,
	}); err != nil {
		add("启动临时代理", false, err.Error())
		return
	}
	defer eng.Stop()
	socksAddr, httpAddr := eng.Addresses()
	add("启动临时代理", true, fmt.Sprintf("SOCKS %s，HTTP %s", socksAddr, httpAddr))

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

	// 8. payload integrity across many cipher chunks
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

	// 9. shutdown leaves nothing behind
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
	add("清理", ok, detail)
	return
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
