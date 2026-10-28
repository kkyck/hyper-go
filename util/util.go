package main

/**
 * 🔧 util.go — Base64, AES, and other helpers
 * ════════════════════════════════════════════════════════════
 */

import (
	"crypto/aes"
	"encoding/base64"
)

// b64encode encodes bytes to base64 (standard or URL-safe).
func b64encode(buf []byte, urlSafe bool) string {
	if urlSafe {
		return base64.URLEncoding.EncodeToString(buf)
	}
	return base64.StdEncoding.EncodeToString(buf)
}

// b64decode decodes base64 (standard or URL-safe, padded or unpadded).
func b64decode(s string, urlSafe bool) ([]byte, error) {
	if urlSafe {
		// Try URLEncoding then RawURLEncoding
		b, err := base64.URLEncoding.DecodeString(padBase64(s))
		if err != nil {
			return base64.RawURLEncoding.DecodeString(s)
		}
		return b, nil
	}
	b, err := base64.StdEncoding.DecodeString(padBase64(s))
	if err != nil {
		return base64.RawStdEncoding.DecodeString(s)
	}
	return b, nil
}

// padBase64 ensures base64 string has correct padding.
func padBase64(s string) string {
	if mod := len(s) % 4; mod != 0 {
		s += "===="[:4-mod]
	}
	return s
}

// aesNewCipher creates a new AES cipher block (256-bit when key is 32 bytes).
func aesNewCipher(key []byte) (aesCipher, error) {
	return aes.NewCipher(key)
}

// aesCipher is an interface to abstract over crypto/aes (which we use directly).
type aesCipher interface {
	Encrypt(dst, src []byte)
	Decrypt(dst, src []byte)
}
