// Package sscore implements the Shadowsocks client side: cipher setup, the
// tunnelled dial, the local SOCKS5 and HTTP proxies, and traffic accounting.
//
// The AEAD framing itself comes from github.com/shadowsocks/go-shadowsocks2
// (Apache-2.0), which implements SIP004 (salt + HKDF-SHA1 subkey + AEAD chunks).
// We only add the key derivation, the local front ends and the bookkeeping.
package sscore

import (
	"crypto/md5"
	"fmt"
	"strings"

	"github.com/shadowsocks/go-shadowsocks2/shadowaead"
)

// Methods maps a lowercase method name onto its key size. The three AEAD
// ciphers (plus AES-192-GCM, same construction with a 24 byte key) are the only
// ones this client speaks.
var methodKeySize = map[string]int{
	"aes-128-gcm":            16,
	"aes-192-gcm":            24,
	"aes-256-gcm":            32,
	"chacha20-ietf-poly1305": 32,
}

// SupportedMethods returns the method names this client can speak.
func SupportedMethods() []string {
	out := make([]string, 0, len(methodKeySize))
	for m := range methodKeySize {
		out = append(out, m)
	}
	return out
}

// evpBytesToKey implements OpenSSL's EVP_BytesToKey with MD5, an empty salt and
// a single iteration - the key derivation every pre-2022 Shadowsocks
// implementation uses to turn a password into a master key.
//
//	D_1 = MD5(password)
//	D_i = MD5(D_{i-1} || password)
//	key = (D_1 || D_2 || ...)[0:keyLen]
func evpBytesToKey(password string, keyLen int) []byte {
	if keyLen <= 0 {
		return nil
	}
	key := make([]byte, 0, keyLen+md5.Size)
	var prev []byte
	for len(key) < keyLen {
		h := md5.New()
		h.Write(prev)
		h.Write([]byte(password))
		prev = h.Sum(nil)
		key = append(key, prev...)
	}
	return key[:keyLen]
}

// newCipher builds the AEAD cipher for the given method and password.
func newCipher(method, password string) (shadowaead.Cipher, error) {
	name := strings.ToLower(strings.TrimSpace(method))
	keyLen, ok := methodKeySize[name]
	if !ok {
		return nil, fmt.Errorf("不支持的加密方式 %q（支持：%s）", method, strings.Join(SupportedMethods(), ", "))
	}
	key := evpBytesToKey(password, keyLen)
	if name == "chacha20-ietf-poly1305" {
		return shadowaead.Chacha20Poly1305(key)
	}
	return shadowaead.AESGCM(key)
}
