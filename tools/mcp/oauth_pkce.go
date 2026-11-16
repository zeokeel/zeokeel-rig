package mcp

// PKCE（RFC 7636，OAuth 2.1 强制）与 state 随机数。

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
)

// randomToken 返回 n 字节随机数的 base64url（无填充）编码。
func randomToken(n int) (string, error) {
	if n <= 0 {
		n = 32
	}
	buffer := make([]byte, n)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buffer), nil
}

// newPKCE 生成 verifier 与 S256 challenge。
func newPKCE() (verifier, challenge string, err error) {
	verifier, err = randomToken(32) // 43 个字符，满足 RFC 7636 的 43–128 长度
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}
