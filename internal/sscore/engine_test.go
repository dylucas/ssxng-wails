package sscore

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
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/shadowsocks/go-shadowsocks2/shadowaead"
	"github.com/shadowsocks/go-shadowsocks2/socks"

	"ssxng/internal/config"
)

func TestEvpBytesToKey(t *testing.T) {
	// Vectors computed independently with Python's hashlib.
	cases := []struct {
		password string
		keyLen   int
		want     string
	}{
		{"password", 16, "5f4dcc3b5aa765d61d8327deb882cf99"},
		{"password", 24, "5f4dcc3b5aa765d61d8327deb882cf992b95990a9151374a"},
		{"password", 32, "5f4dcc3b5aa765d61d8327deb882cf992b95990a9151374abd8ff8c5a7a0fe08"},
		{"ssxng-test", 32, "310eae516d2e81dfe8efcc37be55bc046c09fc5226f1f14ce53a33b7898040d4"},
	}
	for _, c := range cases {
		got := hex.EncodeToString(evpBytesToKey(c.password, c.keyLen))
		if got != c.want {
			t.Errorf("evpBytesToKey(%q, %d) = %s, want %s", c.password, c.keyLen, got, c.want)
		}
	}
}

func TestNewCipher(t *testing.T) {
	for _, m := range config.SupportedMethods {
		if _, err := newCipher(m, "hunter2"); err != nil {
			t.Errorf("newCipher(%q) 失败: %v", m, err)
		}
	}
	if _, err := newCipher("aes-256-cfb", "x"); err == nil {
		t.Error("不支持的方法应当报错")
	}
	if _, err := newCipher("", "x"); err == nil {
		t.Error("空方法名应当报错")
	}
}

// startSSServer runs a minimal Shadowsocks server with the same library's AEAD
// framing. It is the interop reference for our client: were our key derivation
// or address framing wrong, every test below would fail.
func startSSServer(t *testing.T, method, password string) string {
	t.Helper()
	ciph, err := newCipher(method, password)
	if err != nil {
		t.Fatalf("服务端 cipher 构造失败: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	go func() {
		for {
			c, err := ln.Accept()
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
				done := make(chan struct{}, 2)
				go func() { _, _ = io.Copy(rc, sc); done <- struct{}{} }()
				go func() { _, _ = io.Copy(sc, rc); done <- struct{}{} }()
				<-done
			}(c)
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return ln.Addr().String()
}

// socks5Dialer returns a DialContext function that goes through a SOCKS5 proxy
// with no authentication.
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
			return nil, io.ErrUnexpectedEOF
		}
		a := socks.ParseAddr(addr)
		if a == nil {
			c.Close()
			return nil, io.ErrUnexpectedEOF
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
			return nil, io.ErrUnexpectedEOF
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

type testRig struct {
	eng   *Engine
	plain *httptest.Server
	tls   *httptest.Server
	socks string
	http  string
}

func newTestRig(t *testing.T, method string) *testRig {
	t.Helper()
	serverAddr := startSSServer(t, method, "ssxng-secret")
	host, portStr, err := net.SplitHostPort(serverAddr)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("X-Echo-Path", r.URL.Path)
		_, _ = w.Write(body)
	})
	plain := httptest.NewServer(handler)
	t.Cleanup(plain.Close)
	tlsSrv := httptest.NewTLSServer(handler)
	t.Cleanup(tlsSrv.Close)

	// The engine logs from its own goroutines, which can outlive the test. Route
	// those lines into a buffer and flush them from the test goroutine: calling
	// t.Logf directly from a background goroutine panics after the test ends.
	sink := &logSink{}
	t.Cleanup(func() { sink.flush(t) })
	eng := NewEngine(sink.add)
	if err := eng.Start(Config{
		Server:    config.ServerProfile{Host: host, Port: port, Method: method, Password: "ssxng-secret"},
		SocksAddr: "127.0.0.1:0",
		HTTPAddr:  "127.0.0.1:0",
	}); err != nil {
		t.Fatalf("引擎启动失败: %v", err)
	}
	t.Cleanup(eng.Stop)

	socksAddr, httpAddr := eng.Addresses()
	return &testRig{eng: eng, plain: plain, tls: tlsSrv, socks: socksAddr, http: httpAddr}
}

// logSink collects engine log lines so they can be reported from the test
// goroutine, after the engine has been stopped.
type logSink struct {
	mu    sync.Mutex
	lines []string
}

func (l *logSink) add(level, scope, msg string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.lines) >= 200 {
		return
	}
	l.lines = append(l.lines, fmt.Sprintf("[%s] %s: %s", level, scope, msg))
}

func (l *logSink) flush(t *testing.T) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, line := range l.lines {
		t.Log(line)
	}
	l.lines = nil
}

func (r *testRig) socksClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{DialContext: socks5Dialer(r.socks)},
		Timeout:   15 * time.Second,
	}
}

func (r *testRig) httpClient() *http.Client {
	u, _ := url.Parse("http://" + r.http)
	return &http.Client{
		Transport: &http.Transport{
			Proxy:           http.ProxyURL(u),
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
		Timeout: 15 * time.Second,
	}
}

func TestSOCKS5Proxy(t *testing.T) {
	for _, method := range config.SupportedMethods {
		t.Run(method, func(t *testing.T) {
			rig := newTestRig(t, method)
			resp, err := rig.socksClient().Get(rig.plain.URL + "/socks")
			if err != nil {
				t.Fatalf("经 SOCKS5 请求失败: %v", err)
			}
			defer resp.Body.Close()
			if got := resp.Header.Get("X-Echo-Path"); got != "/socks" {
				t.Fatalf("路径异常: %q", got)
			}
		})
	}
}

func TestHTTPProxyAbsoluteForm(t *testing.T) {
	for _, method := range config.SupportedMethods {
		t.Run(method, func(t *testing.T) {
			rig := newTestRig(t, method)
			body := bytes.Repeat([]byte("ssxng"), 1000)
			resp, err := rig.httpClient().Post(rig.plain.URL+"/http", "application/octet-stream", bytes.NewReader(body))
			if err != nil {
				t.Fatalf("经 HTTP 代理请求失败: %v", err)
			}
			defer resp.Body.Close()
			got, _ := io.ReadAll(resp.Body)
			if resp.Header.Get("X-Echo-Path") != "/http" || !bytes.Equal(got, body) {
				t.Fatalf("HTTP 代理转发异常: path=%q len=%d", resp.Header.Get("X-Echo-Path"), len(got))
			}
		})
	}
}

func TestHTTPProxyConnect(t *testing.T) {
	rig := newTestRig(t, "aes-256-gcm")
	resp, err := rig.httpClient().Get(rig.tls.URL + "/connect")
	if err != nil {
		t.Fatalf("经 CONNECT 的 HTTPS 请求失败: %v", err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("X-Echo-Path"); got != "/connect" {
		t.Fatalf("CONNECT 转发异常: %q", got)
	}
}

func TestLargePayloadIntegrity(t *testing.T) {
	rig := newTestRig(t, "aes-256-gcm")
	payload := make([]byte, 1<<20) // 1 MiB, well past the 16 KiB AEAD chunk size
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	resp, err := rig.socksClient().Post(rig.plain.URL+"/echo", "application/octet-stream", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("大载荷请求失败: %v", err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取响应失败: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("大载荷校验失败: 发送 %d 字节，收到 %d 字节", len(payload), len(got))
	}
	if st := rig.eng.Stats(); st.Up < int64(len(payload)) {
		t.Fatalf("上传统计异常: %d", st.Up)
	}
}

func TestConcurrentConnections(t *testing.T) {
	rig := newTestRig(t, "chacha20-ietf-poly1305")
	const n = 8
	var wg sync.WaitGroup
	errCh := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := rig.socksClient().Get(rig.plain.URL + "/concurrent")
			if err != nil {
				errCh <- err
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("并发请求失败: %v", err)
	}
	if st := rig.eng.Stats(); st.Sessions < n {
		t.Errorf("会话计数偏低: %d", st.Sessions)
	}
}

// TestSOCKS5UnreachableServer checks the one failure the client *can* report:
// when the Shadowsocks server itself cannot be reached, the SOCKS5 request is
// answered with an error reply. (A failure to reach the *target* cannot be
// reported - the Shadowsocks protocol has no acknowledgement for it, which is
// why ss-local behaves the same way.)
func TestSOCKS5UnreachableServer(t *testing.T) {
	// The engine logs from its own goroutines, which can outlive the test. Route
	// those lines into a buffer and flush them from the test goroutine: calling
	// t.Logf directly from a background goroutine panics after the test ends.
	sink := &logSink{}
	t.Cleanup(func() { sink.flush(t) })
	eng := NewEngine(sink.add)
	// Port 1 on loopback is never listening.
	if err := eng.Start(Config{
		Server:    config.ServerProfile{Host: "127.0.0.1", Port: 1, Method: "aes-128-gcm", Password: "x"},
		SocksAddr: "127.0.0.1:0",
		HTTPAddr:  "",
	}); err != nil {
		t.Fatal(err)
	}
	defer eng.Stop()
	socksAddr, _ := eng.Addresses()

	c, err := net.Dial("tcp", socksAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(15 * time.Second))
	if _, err := c.Write([]byte{5, 1, 0}); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(c, make([]byte, 2)); err != nil {
		t.Fatal(err)
	}
	a := socks.ParseAddr("127.0.0.1:9")
	if _, err := c.Write(append([]byte{5, 1, 0}, a...)); err != nil {
		t.Fatal(err)
	}
	reply := make([]byte, 10)
	if _, err := io.ReadFull(c, reply); err != nil {
		t.Fatalf("没有收到 SOCKS5 应答: %v", err)
	}
	if reply[1] == 0 {
		t.Fatalf("服务器不可达时不应答成功: %v", reply)
	}
}

// TestSOCKS5TargetRefused documents the unavoidable case: the server is
// reachable, so the reply is a success, and the connection ends right after
// because the remote end could not reach the target.
func TestSOCKS5TargetRefused(t *testing.T) {
	rig := newTestRig(t, "aes-128-gcm")
	c, err := net.Dial("tcp", rig.socks)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(15 * time.Second))
	if _, err := c.Write([]byte{5, 1, 0}); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(c, make([]byte, 2)); err != nil {
		t.Fatal(err)
	}
	a := socks.ParseAddr("127.0.0.1:1")
	if _, err := c.Write(append([]byte{5, 1, 0}, a...)); err != nil {
		t.Fatal(err)
	}
	reply := make([]byte, 10)
	if _, err := io.ReadFull(c, reply); err != nil {
		t.Fatalf("没有收到 SOCKS5 应答: %v", err)
	}
	if reply[1] != 0 {
		t.Fatalf("服务器可达时应答成功，实际: %v", reply)
	}
	buf := make([]byte, 1)
	if _, err := c.Read(buf); err == nil {
		t.Fatal("目标不可达时不应有数据返回")
	}
}

func TestUDPAssociateRejected(t *testing.T) {
	rig := newTestRig(t, "aes-128-gcm")
	c, err := net.Dial("tcp", rig.socks)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write([]byte{5, 1, 0}); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(c, make([]byte, 2)); err != nil {
		t.Fatal(err)
	}
	// CMD=3 (UDP ASSOCIATE), ATYP=1, 0.0.0.0:0
	if _, err := c.Write([]byte{5, 3, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
		t.Fatal(err)
	}
	reply := make([]byte, 10)
	if _, err := io.ReadFull(c, reply); err != nil {
		t.Fatalf("没有收到应答: %v", err)
	}
	if reply[1] == 0 {
		t.Fatalf("UDP ASSOCIATE 不应答成功: %v", reply)
	}
}

func TestEngineStopReleasesPorts(t *testing.T) {
	rig := newTestRig(t, "aes-128-gcm")
	rig.eng.Stop()
	if rig.eng.Running() {
		t.Fatal("停止后仍在运行")
	}
	for _, addr := range []string{rig.socks, rig.http} {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			t.Fatalf("端口 %s 未释放: %v", addr, err)
		}
		_ = ln.Close()
	}
}

func TestSetServerSwapsUpstream(t *testing.T) {
	rig := newTestRig(t, "aes-128-gcm")
	// A second server with a different password: switching to it must be
	// picked up by new connections without restarting the listeners.
	serverAddr := startSSServer(t, "aes-256-gcm", "another-secret")
	host, portStr, _ := net.SplitHostPort(serverAddr)
	port, _ := strconv.Atoi(portStr)
	if err := rig.eng.SetServer(config.ServerProfile{Host: host, Port: port, Method: "aes-256-gcm", Password: "another-secret"}); err != nil {
		t.Fatalf("切换服务器失败: %v", err)
	}
	if socks, _ := rig.eng.Addresses(); socks != rig.socks {
		t.Fatalf("切换服务器不应改变监听地址: %s -> %s", rig.socks, socks)
	}
	resp, err := rig.socksClient().Get(rig.plain.URL + "/after-switch")
	if err != nil {
		t.Fatalf("切换后请求失败: %v", err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("X-Echo-Path"); got != "/after-switch" {
		t.Fatalf("切换后路径异常: %q", got)
	}
}

// TestMain disables go-shadowsocks2's process-global salt replay filter.
//
// shadowaead deliberately keeps a singleton bloom filter of every salt the
// process has used as a writer, and rejects any salt seen again. That is the
// right behaviour for a server, but here the interop server and the client run
// in the same process, so the client's own outgoing salts would be flagged as
// replays. A negative capacity turns the filter off (see internal/saltfilter.go
// in go-shadowsocks2). Production code keeps the default.
func TestMain(m *testing.M) {
	_ = os.Setenv("SHADOWSOCKS_SF_CAPACITY", "-1")
	os.Exit(m.Run())
}
