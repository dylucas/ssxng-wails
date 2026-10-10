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

var errSocksUDPUnsupported = errors.New("暂不支持 SOCKS5 UDP ASSOCIATE")

// socksRequest is a parsed SOCKS5 CONNECT request.
type socksRequest struct {
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
	if head[1] != cmdConnect {
		if _, err := rw.Write(replyBytes(repCmdNotSupported)); err != nil {
			return req, err
		}
		if head[1] == cmdUDPAssoc {
			return req, errSocksUDPUnsupported
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
	if port == 0 {
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

// replyBytes builds a SOCKS5 reply for the CONNECT command.
func replyBytes(rep byte) []byte {
	return []byte{socksVersion, rep, 0, atypIPv4, 0, 0, 0, 0, 0, 0}
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
