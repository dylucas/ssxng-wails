package sscore

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
)

// SOCKS5 constants (RFC 1928).
const (
	socksVersion        = 5
	cmdConnect          = 1
	cmdUDPAssoc         = 3
	atypIPv4            = 1
	atypDomain          = 3
	atypIPv6            = 4
	repSuccess          = 0
	repNotAllowed       = 2
	repHostUnreach      = 4
	repConnRefused      = 5
	repCmdNotSupported  = 7
	repAddrNotSupported = 8
)

// errSocksUDPUnsupported is returned when the client asks for UDP ASSOCIATE but
// the relay is switched off in the settings.
var errSocksUDPUnsupported = errors.New("SOCKS5 UDP ASSOCIATE 已关闭")

// socksRequest is a parsed SOCKS5 request.
type socksRequest struct {
	Cmd byte // cmdConnect or cmdUDPAssoc

	// Target is the address in the request. For CONNECT it is the destination;
	// for UDP ASSOCIATE it is the address the client claims it will send
	// datagrams from, which RFC 1928 allows to be all zeros and which almost
	// every client leaves empty. Ignored for UDP.
	Target string // host:port, IPv6 hosts are wrapped in brackets
}

// readSocksRequest performs the SOCKS5 greeting and request handshake. It
// replies to the method negotiation but leaves the request reply to the
// caller, so a failed upstream dial can be reported with a real SOCKS error
// instead of a silent disconnect.
func readSocksRequest(rw io.ReadWriter) (socksRequest, error) {
	var req socksRequest
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(rw, hdr); err != nil {
		return req, err
	}
	if hdr[0] != socksVersion {
		return req, fmt.Errorf("不支持的 SOCKS 版本 %d", hdr[0])
	}
	n := int(hdr[1])
	if n == 0 {
		return req, errors.New("SOCKS5 客户端没有提供认证方式")
	}
	methods := make([]byte, n)
	if _, err := io.ReadFull(rw, methods); err != nil {
		return req, err
	}
	// We accept "no authentication" only; the listener is loopback-only.
	if _, err := rw.Write([]byte{socksVersion, 0}); err != nil {
		return req, err
	}

	head := make([]byte, 4)
	if _, err := io.ReadFull(rw, head); err != nil {
		return req, err
	}
	if head[0] != socksVersion {
		return req, fmt.Errorf("不支持的 SOCKS 版本 %d", head[0])
	}
	req.Cmd = head[1]
	if req.Cmd != cmdConnect && req.Cmd != cmdUDPAssoc {
		if _, err := rw.Write(replyBytes(repCmdNotSupported)); err != nil {
			return req, err
		}
		return req, fmt.Errorf("不支持的 SOCKS5 命令 %d", head[1])
	}

	host, err := readSocksHost(rw, head[3])
	if err != nil {
		_, _ = rw.Write(replyBytes(repAddrNotSupported))
		return req, err
	}
	portBuf := make([]byte, 2)
	if _, err := io.ReadFull(rw, portBuf); err != nil {
		return req, err
	}
	port := binary.BigEndian.Uint16(portBuf)
	// A CONNECT to port 0 is meaningless; a UDP ASSOCIATE carrying 0.0.0.0:0 is
	// the normal case (the client cannot know its source port in advance).
	if port == 0 && req.Cmd == cmdConnect {
		_, _ = rw.Write(replyBytes(repHostUnreach))
		return req, errors.New("目标端口为 0")
	}
	req.Target = net.JoinHostPort(host, strconv.Itoa(int(port)))
	return req, nil
}

func readSocksHost(r io.Reader, atyp byte) (string, error) {
	switch atyp {
	case atypIPv4:
		b := make([]byte, 4)
		if _, err := io.ReadFull(r, b); err != nil {
			return "", err
		}
		return net.IP(b).String(), nil
	case atypIPv6:
		b := make([]byte, 16)
		if _, err := io.ReadFull(r, b); err != nil {
			return "", err
		}
		return net.IP(b).String(), nil
	case atypDomain:
		l := make([]byte, 1)
		if _, err := io.ReadFull(r, l); err != nil {
			return "", err
		}
		b := make([]byte, int(l[0]))
		if _, err := io.ReadFull(r, b); err != nil {
			return "", err
		}
		return string(b), nil
	default:
		return "", fmt.Errorf("不支持的 SOCKS5 地址类型 %d", atyp)
	}
}

// replyBytes builds a SOCKS5 reply with an all-zero BND.ADDR/BND.PORT, which is
// all CONNECT needs: the client never reads the bound address back.
func replyBytes(rep byte) []byte { return replyBytesAddr(rep, "") }

// replyBytesAddr builds a SOCKS5 reply whose BND.ADDR/BND.PORT is addr. UDP
// ASSOCIATE must use this: the address in the reply is where the client has to
// send its datagrams, so an all-zero reply leaves it with nowhere to go.
//
// An unparseable or empty addr falls back to 0.0.0.0:0.
func replyBytesAddr(rep byte, addr string) []byte {
	out := []byte{socksVersion, rep, 0, atypIPv4, 0, 0, 0, 0, 0, 0}
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return out
	}
	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		return out
	}
	if ip4 := net.ParseIP(host).To4(); ip4 != nil {
		copy(out[4:8], ip4)
		out[8], out[9] = byte(port>>8), byte(port)
		return out
	}
	if ip6 := net.ParseIP(host).To16(); ip6 != nil {
		out = []byte{socksVersion, rep, 0, atypIPv6}
		out = append(out, ip6...)
		return append(out, byte(port>>8), byte(port))
	}
	return out
}

// replyForError maps a dial error onto a SOCKS5 reply code.
func replyForError(err error) byte {
	if err == nil {
		return repSuccess
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "refused"):
		return repConnRefused
	case strings.Contains(msg, "no such host"), strings.Contains(msg, "not known"):
		return repHostUnreach
	case strings.Contains(msg, "permission"), strings.Contains(msg, "not allowed"):
		return repNotAllowed
	default:
		return repHostUnreach
	}
}
