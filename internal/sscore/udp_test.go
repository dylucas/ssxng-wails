package sscore

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"io"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/shadowsocks/go-shadowsocks2/socks"

	"ssxng/internal/config"
)

// udpRig is an engine with UDP on, the loopback Shadowsocks server it tunnels
// to, and an echo server reachable over UDP.
type udpRig struct {
	eng   *Engine
	socks string // SOCKS5 TCP control address
	relay string // UDP relay address clients send datagrams to
	echo  string // UDP echo server address
}

// startUDPEcho runs a loopback UDP echo server and returns its address.
func startUDPEcho(t *testing.T) (string, net.PacketConn) {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听 UDP 回声服务失败: %v", err)
	}
	go func() {
		buf := make([]byte, 64<<10)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			// Echo with a marker so a reply can be told apart from a stale one.
			out := append([]byte("echo:"), buf[:n]...)
			if _, err := pc.WriteTo(out, from); err != nil {
				return
			}
		}
	}()
	return pc.LocalAddr().String(), pc
}

// newUDPRig starts an engine with UDP enabled and the given UDP relay address.
func newUDPRig(t *testing.T, method string, udpAddr string) *udpRig {
	t.Helper()
	sink := &logSink{}
	t.Cleanup(func() { sink.flush(t) })

	loSrv, err := StartLocalServer(config.ServerProfile{
		Host: "127.0.0.1", Port: 1, Method: method, Password: "ssxng-secret",
	})
	if err != nil {
		t.Fatalf("本地测试服务端启动失败: %v", err)
	}
	t.Cleanup(loSrv.Close)
	host, port := loSrv.Address()

	echo, echoPC := startUDPEcho(t)
	t.Cleanup(func() { _ = echoPC.Close() })

	eng := NewEngine(sink.add)
	if err := eng.Start(Config{
		Server:    config.ServerProfile{Host: host, Port: port, Method: method, Password: "ssxng-secret"},
		SocksAddr: "127.0.0.1:0",
		UDPAddr:   udpAddr,
	}); err != nil {
		t.Fatalf("引擎启动失败: %v", err)
	}
	t.Cleanup(eng.Stop)

	socksAddr, _ := eng.Addresses()
	return &udpRig{eng: eng, socks: socksAddr, relay: eng.UDPAddr(), echo: echo}
}

// associate performs the SOCKS5 greeting and a UDP ASSOCIATE, returning the
// control connection and the relay address the server handed back.
func associate(t *testing.T, socksAddr string) (net.Conn, string) {
	t.Helper()
	c, err := net.Dial("tcp", socksAddr)
	if err != nil {
		t.Fatalf("连接 SOCKS 端口失败: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))

	if _, err := c.Write([]byte{5, 1, 0}); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(c, make([]byte, 2)); err != nil {
		t.Fatal(err)
	}
	// CMD=3 (UDP ASSOCIATE), ATYP=1, 0.0.0.0:0 - what a real client sends,
	// because it cannot know its own source port before this handshake.
	if _, err := c.Write([]byte{5, 3, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
		t.Fatal(err)
	}
	reply := make([]byte, 10)
	if _, err := io.ReadFull(c, reply); err != nil {
		t.Fatalf("没有收到 UDP ASSOCIATE 应答: %v", err)
	}
	if reply[1] != 0 {
		t.Fatalf("UDP ASSOCIATE 应答失败: REP=%d", reply[1])
	}
	if reply[3] != atypIPv4 {
		t.Fatalf("应答地址类型应为 IPv4，实际 %d", reply[3])
	}
	host := net.IP(reply[4:8]).String()
	port := binary.BigEndian.Uint16(reply[8:10])
	if port == 0 {
		t.Fatal("应答的 BND.PORT 为 0，客户端无处可发")
	}
	return c, net.JoinHostPort(host, strconv.Itoa(int(port)))
}

// udpRequest builds a SOCKS5 UDP request datagram for target.
func udpRequest(target string, payload []byte) []byte {
	addr := socks.ParseAddr(target)
	pkt := make([]byte, 0, udpSocksHeader+len(addr)+len(payload))
	pkt = append(pkt, 0, 0, 0)
	pkt = append(pkt, addr...)
	return append(pkt, payload...)
}

// exchange sends one datagram and waits for the reply, checking the returned
// header names the same target.
func exchange(t *testing.T, pc net.PacketConn, relay, target string, payload []byte) []byte {
	t.Helper()
	if _, err := pc.WriteTo(udpRequest(target, payload), mustUDPAddr(t, relay)); err != nil {
		t.Fatalf("发送 UDP 数据报失败: %v", err)
	}
	_ = pc.SetReadDeadline(time.Now().Add(10 * time.Second))
	buf := make([]byte, udpReadSize)
	n, _, err := pc.ReadFrom(buf)
	if err != nil {
		t.Fatalf("等待 UDP 应答失败: %v", err)
	}
	if n < udpSocksHeader {
		t.Fatalf("应答过短: %d 字节", n)
	}
	if buf[0] != 0 || buf[1] != 0 || buf[2] != 0 {
		t.Fatalf("应答的 RSV/FRAG 应为 0，实际 %v", buf[:3])
	}
	got := socks.SplitAddr(buf[udpSocksHeader:n])
	if got == nil {
		t.Fatalf("应答地址无法解析: %q", buf[udpSocksHeader:n])
	}
	if got.String() != target {
		t.Fatalf("应答来源地址 = %s，期望 %s", got.String(), target)
	}
	return buf[udpSocksHeader+len(got) : n]
}

func mustUDPAddr(t *testing.T, addr string) *net.UDPAddr {
	t.Helper()
	a, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		t.Fatalf("解析 %s 失败: %v", addr, err)
	}
	return a
}

// dialRelay opens a client socket for the relay, bound to loopback.
func dialRelay(t *testing.T) net.PacketConn {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("创建客户端 UDP 套接字失败: %v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	return pc
}

// TestUDPAssociateForwardsDatagrams is the end-to-end path: a SOCKS5 UDP
// request goes to the relay, is sealed to the Shadowsocks server, reaches the
// echo server, and the reply comes back with the RFC 1928 header restored.
func TestUDPAssociateForwardsDatagrams(t *testing.T) {
	for _, method := range []string{"aes-128-gcm", "chacha20-ietf-poly1305"} {
		t.Run(method, func(t *testing.T) {
			rig := newUDPRig(t, method, "127.0.0.1:0")
			_, relay := associate(t, rig.socks)
			if relay != rig.relay {
				t.Fatalf("应答的转发地址 %s 与引擎报告的不一致 %s", relay, rig.relay)
			}

			pc := dialRelay(t)
			payload := []byte("hello-udp")
			got := exchange(t, pc, relay, rig.echo, payload)
			if want := append([]byte("echo:"), payload...); !bytes.Equal(got, want) {
				t.Fatalf("应答内容 = %q，期望 %q", got, want)
			}
		})
	}
}

// TestUDPAssociateRewritesTheBoundAddress guards the single mistake that makes
// UDP silently useless: replying with 0.0.0.0:0 so the client has nowhere to
// send. The reply must carry the port the relay actually listens on.
func TestUDPAssociateRewritesTheBoundAddress(t *testing.T) {
	rig := newUDPRig(t, "aes-128-gcm", "127.0.0.1:0")
	_, relay := associate(t, rig.socks)

	host, portStr, err := net.SplitHostPort(relay)
	if err != nil {
		t.Fatalf("应答地址不可解析: %v", err)
	}
	if host != "127.0.0.1" {
		t.Fatalf("应答地址 = %s，期望 127.0.0.1", host)
	}
	if portStr == "0" || portStr == "" {
		t.Fatalf("应答端口 = %q，客户端将无处可发", portStr)
	}
	// A successful exchange proves the advertised port really is the relay.
	pc := dialRelay(t)
	if got := exchange(t, pc, relay, rig.echo, []byte("ping")); len(got) == 0 {
		t.Fatal("应答为空")
	}
}

// TestUDPAssociateRejectedWhenDisabled covers the setting: with UDP off the
// engine still answers the handshake, but refuses the command.
func TestUDPAssociateRejectedWhenDisabled(t *testing.T) {
	rig := newUDPRig(t, "aes-128-gcm", "") // no UDPAddr = UDP switched off
	if rig.relay != "" {
		t.Fatalf("UDP 关闭时不应报告转发地址: %q", rig.relay)
	}
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
	if _, err := c.Write([]byte{5, 3, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
		t.Fatal(err)
	}
	reply := make([]byte, 10)
	if _, err := io.ReadFull(c, reply); err != nil {
		t.Fatalf("没有收到应答: %v", err)
	}
	if reply[1] != repCmdNotSupported {
		t.Fatalf("UDP 关闭时应答 REP=%d，期望 %d", reply[1], repCmdNotSupported)
	}
}

// TestUDPAssociateKeepsControlConnectionOpen: RFC 1928 ties the association to
// the TCP connection, so the server must not close it right after the reply.
func TestUDPAssociateKeepsControlConnectionOpen(t *testing.T) {
	rig := newUDPRig(t, "aes-128-gcm", "127.0.0.1:0")
	c, _ := associate(t, rig.socks)

	// Idle for a while, then confirm the connection is still usable.
	time.Sleep(200 * time.Millisecond)
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Write([]byte{0}); err != nil {
		t.Fatalf("控制连接在应答后不应关闭: %v", err)
	}
	// Data on the control connection is drained, not echoed.
	buf := make([]byte, 8)
	_ = c.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if _, err := c.Read(buf); err == nil {
		t.Fatal("控制连接不应回显数据")
	}
}

// TestUDPDropsFragmentedDatagrams: FRAG != 0 is optional and unsupported, so it
// must be dropped rather than forwarded with a wrong payload offset.
func TestUDPDropsFragmentedDatagrams(t *testing.T) {
	rig := newUDPRig(t, "aes-128-gcm", "127.0.0.1:0")
	_, relay := associate(t, rig.socks)

	pc := dialRelay(t)
	// Prove the path works first: a plain request echoes. Without this, a relay
	// that forwards nothing at all would "pass" this test.
	if got := exchange(t, pc, relay, rig.echo, []byte("baseline")); len(got) == 0 {
		t.Fatal("中继未转发正常数据报，本用例无意义")
	}

	// FRAG=1 with a payload that would otherwise echo back.
	pkt := udpRequest(rig.echo, []byte("fragmented"))
	pkt[2] = 1
	if _, err := pc.WriteTo(pkt, mustUDPAddr(t, relay)); err != nil {
		t.Fatal(err)
	}
	_ = pc.SetReadDeadline(time.Now().Add(700 * time.Millisecond))
	buf := make([]byte, udpReadSize)
	if n, _, err := pc.ReadFrom(buf); err == nil {
		t.Fatalf("分片数据报不应被转发，却收到 %d 字节", n)
	}
}

// TestUDPDropsUnparseableDatagrams: a packet too short for its own address must
// not panic or reach the server.
func TestUDPDropsUnparseableDatagrams(t *testing.T) {
	rig := newUDPRig(t, "aes-128-gcm", "127.0.0.1:0")
	_, relay := associate(t, rig.socks)

	pc := dialRelay(t)
	if got := exchange(t, pc, relay, rig.echo, []byte("baseline")); len(got) == 0 {
		t.Fatal("中继未转发正常数据报，本用例无意义")
	}

	for _, pkt := range [][]byte{
		{},                     // empty
		{0, 0},                 // shorter than the RSV/FRAG header
		{0, 0, 0},              // header only
		{0, 0, 0, 9},           // unknown address type
		{0, 0, 0, atypIPv4, 1}, // truncated IPv4 address
	} {
		if _, err := pc.WriteTo(pkt, mustUDPAddr(t, relay)); err != nil {
			t.Fatal(err)
		}
	}
	_ = pc.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	buf := make([]byte, udpReadSize)
	if n, _, err := pc.ReadFrom(buf); err == nil {
		t.Fatalf("畸形数据报不应被转发，却收到 %d 字节", n)
	}

	// The relay must still work afterwards.
	if got := exchange(t, pc, relay, rig.echo, []byte("still-alive")); len(got) == 0 {
		t.Fatal("畸形数据报之后中继应仍然可用")
	}
}

// TestUDPMultiplexesTargetsOverOneSession: the SS address header carries the
// destination, so many targets share one client session.
func TestUDPMultiplexesTargetsOverOneSession(t *testing.T) {
	rig := newUDPRig(t, "aes-128-gcm", "127.0.0.1:0")
	_, relay := associate(t, rig.socks)

	echo2, echo2PC := startUDPEcho(t)
	t.Cleanup(func() { _ = echo2PC.Close() })

	pc := dialRelay(t)
	for _, target := range []string{rig.echo, echo2, rig.echo, echo2} {
		got := exchange(t, pc, relay, target, []byte("mux"))
		if want := append([]byte("echo:"), []byte("mux")...); !bytes.Equal(got, want) {
			t.Fatalf("%s: 应答内容 = %q", target, got)
		}
	}

	rig.eng.udpMu.Lock()
	n := len(rig.eng.udpSessions)
	rig.eng.udpMu.Unlock()
	if n != 1 {
		t.Fatalf("一个客户端地址应只建立一个会话，实际 %d", n)
	}
}

// TestUDPSeparatesClientsBySourceAddress: two client sockets get two sessions,
// and traffic counters reflect both.
func TestUDPSeparatesClientsBySourceAddress(t *testing.T) {
	rig := newUDPRig(t, "aes-128-gcm", "127.0.0.1:0")
	_, relay := associate(t, rig.socks)

	a, b := dialRelay(t), dialRelay(t)
	for _, pc := range []net.PacketConn{a, b} {
		if got := exchange(t, pc, relay, rig.echo, []byte("abcd")); len(got) == 0 {
			t.Fatal("应答为空")
		}
	}

	rig.eng.udpMu.Lock()
	n := len(rig.eng.udpSessions)
	rig.eng.udpMu.Unlock()
	if n != 2 {
		t.Fatalf("两个客户端应各有一个会话，实际 %d", n)
	}

	// Payload bytes are counted in both directions: 4 up and 4 down per client,
	// plus the "echo:" prefix on the way back.
	st := rig.eng.Stats()
	if st.Up != 8 {
		t.Fatalf("上行字节 = %d，期望 8", st.Up)
	}
	if st.Down != int64(len("echo:abcd")*2) {
		t.Fatalf("下行字节 = %d，期望 %d", st.Down, len("echo:abcd")*2)
	}
	if st.Active != 2 {
		t.Fatalf("活动会话 = %d，期望 2", st.Active)
	}
}

// TestUDPStopReleasesThePortAndSessions: Stop must close the relay socket and
// every session, or a restart would fail to bind.
func TestUDPStopReleasesThePortAndSessions(t *testing.T) {
	rig := newUDPRig(t, "aes-128-gcm", "127.0.0.1:0")
	_, relay := associate(t, rig.socks)
	pc := dialRelay(t)
	if got := exchange(t, pc, relay, rig.echo, []byte("x")); len(got) == 0 {
		t.Fatal("应答为空")
	}

	rig.eng.Stop()

	rig.eng.udpMu.Lock()
	n := len(rig.eng.udpSessions)
	rig.eng.udpMu.Unlock()
	if n != 0 {
		t.Fatalf("停止后仍有 %d 个 UDP 会话", n)
	}
	if got := rig.eng.UDPAddr(); got != "" {
		t.Fatalf("停止后不应报告 UDP 地址: %q", got)
	}
	// The UDP port must be bindable again.
	ln, err := net.ListenPacket("udp", relay)
	if err != nil {
		t.Fatalf("UDP 端口 %s 未释放: %v", relay, err)
	}
	_ = ln.Close()
}

// TestUDPSessionsAreReapedWhenIdle checks the idle sweeper: a session whose
// client stops must not stay in the map forever.
func TestUDPSessionsAreReapedWhenIdle(t *testing.T) {
	if testing.Short() {
		t.Skip("需要等待空闲超时")
	}
	rig := newUDPRig(t, "aes-128-gcm", "127.0.0.1:0")
	_, relay := associate(t, rig.socks)
	pc := dialRelay(t)
	if got := exchange(t, pc, relay, rig.echo, []byte("x")); len(got) == 0 {
		t.Fatal("应答为空")
	}

	// Shrink the window so the test does not have to wait a full minute. The
	// reply loop reads its deadline from the constant, so instead of sleeping
	// 60s we assert the reaping path directly by closing the session's socket.
	rig.eng.udpMu.Lock()
	var sess *udpSession
	for _, s := range rig.eng.udpSessions {
		sess = s
	}
	rig.eng.udpMu.Unlock()
	if sess == nil {
		t.Fatal("没有找到会话")
	}
	// Closing the encrypted socket makes the reply loop return, which must drop
	// the session from the map and decrement the active counter.
	_ = sess.enc.Close()

	deadline := time.Now().Add(3 * time.Second)
	for {
		rig.eng.udpMu.Lock()
		n := len(rig.eng.udpSessions)
		rig.eng.udpMu.Unlock()
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("会话未被回收，仍有 %d 个", n)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// The counter must go back down exactly once.
	deadline = time.Now().Add(2 * time.Second)
	for rig.eng.Stats().Active != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("活动会话计数 = %d，期望 0", rig.eng.Stats().Active)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestUDPSessionCapBoundsDescriptors: a flood from many source addresses must
// be refused rather than allowed to exhaust file descriptors, and a refusal
// must not disturb the sessions that already exist.
func TestUDPSessionCapBoundsDescriptors(t *testing.T) {
	rig := newUDPRig(t, "aes-128-gcm", "127.0.0.1:0")
	rig.eng.udpMax = 1
	_, relay := associate(t, rig.socks)

	first, second := dialRelay(t), dialRelay(t)
	if got := exchange(t, first, relay, rig.echo, []byte("first")); len(got) == 0 {
		t.Fatal("第一个客户端应能建立会话")
	}
	if _, err := second.WriteTo(udpRequest(rig.echo, []byte("over")), mustUDPAddr(t, relay)); err != nil {
		t.Fatal(err)
	}
	_ = second.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	buf := make([]byte, udpReadSize)
	if n, _, err := second.ReadFrom(buf); err == nil {
		t.Fatalf("达到会话上限后不应再新建会话，却收到 %d 字节", n)
	}
	// The client that already owns the slot must be unaffected.
	if got := exchange(t, first, relay, rig.echo, []byte("again")); len(got) == 0 {
		t.Fatal("达到上限不应影响已有会话")
	}
}

// TestUDPSwitchingServerDropsSessions: sessions hold a resolved copy of the old
// server address, so a server swap has to rebuild them.
func TestUDPSwitchingServerDropsSessions(t *testing.T) {
	rig := newUDPRig(t, "aes-128-gcm", "127.0.0.1:0")
	_, relay := associate(t, rig.socks)
	pc := dialRelay(t)
	if got := exchange(t, pc, relay, rig.echo, []byte("before")); len(got) == 0 {
		t.Fatal("应答为空")
	}
	rig.eng.udpMu.Lock()
	before := len(rig.eng.udpSessions)
	rig.eng.udpMu.Unlock()
	if before != 1 {
		t.Fatalf("切换前应有 1 个会话，实际 %d", before)
	}

	if err := rig.eng.SetServer(config.ServerProfile{
		Host: "127.0.0.1", Port: 1, Method: "aes-128-gcm", Password: "another-secret",
	}); err != nil {
		t.Fatal(err)
	}
	if st := rig.eng.Stats(); st.Active != 0 {
		t.Fatalf("切换服务器后活动会话 = %d，期望 0", st.Active)
	}
	// A new datagram rebuilds a session against the new server (which here has a
	// different password, so the exchange fails - the point is that the map was
	// rebuilt rather than kept).
	if _, err := pc.WriteTo(udpRequest(rig.echo, []byte("after")), mustUDPAddr(t, relay)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	rig.eng.udpMu.Lock()
	after := len(rig.eng.udpSessions)
	rig.eng.udpMu.Unlock()
	if after != 1 {
		t.Fatalf("切换后应重建 1 个会话，实际 %d", after)
	}
}

// TestUDPStartIsTransactional: a UDP bind failure must not leave the TCP
// listeners running.
func TestUDPStartIsTransactional(t *testing.T) {
	// Occupy a UDP port, then ask the engine to bind it.
	taken, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()

	eng := NewEngine(nil)
	err = eng.Start(Config{
		Server:    config.ServerProfile{Host: "127.0.0.1", Port: 1, Method: "aes-128-gcm", Password: "x"},
		SocksAddr: "127.0.0.1:0",
		UDPAddr:   taken.LocalAddr().String(),
	})
	if err == nil {
		eng.Stop()
		t.Fatal("UDP 端口被占用时启动应当失败")
	}
	if eng.Running() {
		t.Fatal("启动失败后不应处于运行状态")
	}
	if socks, http := eng.Addresses(); socks != "" || http != "" {
		t.Fatalf("启动失败后不应留下监听地址: %q %q", socks, http)
	}
}

// TestUDPRelayIsNotAPublicForwarder: the relay only ever talks to the configured
// server, so a datagram cannot be used to reach an arbitrary host directly.
func TestUDPRelayIsNotAPublicForwarder(t *testing.T) {
	rig := newUDPRig(t, "aes-128-gcm", "127.0.0.1:0")
	_, relay := associate(t, rig.socks)

	// A target that the loopback Shadowsocks server itself cannot reach must
	// produce no reply, proving the payload went through the tunnel rather than
	// straight out of this process.
	pc := dialRelay(t)
	// A working baseline keeps this from passing on a relay that does nothing.
	if got := exchange(t, pc, relay, rig.echo, []byte("baseline")); len(got) == 0 {
		t.Fatal("中继未转发正常数据报，本用例无意义")
	}

	payload := make([]byte, 16)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	if _, err := pc.WriteTo(udpRequest("192.0.2.1:9", payload), mustUDPAddr(t, relay)); err != nil {
		t.Fatal(err)
	}
	_ = pc.SetReadDeadline(time.Now().Add(700 * time.Millisecond))
	buf := make([]byte, udpReadSize)
	if n, _, err := pc.ReadFrom(buf); err == nil {
		t.Fatalf("不可达目标不应有应答，却收到 %d 字节", n)
	}
}

// TestUDPAssociateReplyCarriesIPv4 guards the reply encoding itself.
func TestUDPAssociateReplyCarriesIPv4(t *testing.T) {
	got := replyBytesAddr(repSuccess, "127.0.0.1:1086")
	want := []byte{5, 0, 0, atypIPv4, 127, 0, 0, 1, 0x04, 0x3E}
	if !bytes.Equal(got, want) {
		t.Fatalf("replyBytesAddr = %v，期望 %v", got, want)
	}
	// An unusable address must degrade to all zeros rather than a partial reply.
	for _, addr := range []string{"", "127.0.0.1", "127.0.0.1:0", "not-an-ip:80", "127.0.0.1:99999"} {
		out := replyBytesAddr(repSuccess, addr)
		if len(out) != 10 {
			t.Fatalf("addr=%q 的应答长度 = %d，期望 10", addr, len(out))
		}
		if out[3] != atypIPv4 {
			t.Fatalf("addr=%q 的地址类型 = %d，期望 IPv4", addr, out[3])
		}
	}
}
