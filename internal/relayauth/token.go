// Package relayauth signs object-scoped credentials for server-side CDN reads.
// These credentials never consume user download tickets or quota.
package relayauth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"strconv"
	"strings"
	"time"
)

const Prefix = "relay1."

func Sign(key, escapedPath string, expires time.Time) string {
	stamp := strconv.FormatInt(expires.Unix(), 10)
	return Prefix + stamp + "." + base64.RawURLEncoding.EncodeToString(mac(key, escapedPath, stamp))
}

func Verify(key, escapedPath, token string, now time.Time) bool {
	if strings.TrimSpace(key) == "" || escapedPath == "" || !strings.HasPrefix(escapedPath, "/") || !strings.HasPrefix(token, Prefix) {
		return false
	}
	stamp, signature, ok := strings.Cut(strings.TrimPrefix(token, Prefix), ".")
	if !ok {
		return false
	}
	expires, err := strconv.ParseInt(stamp, 10, 64)
	if err != nil || expires <= now.Unix() {
		return false
	}
	got, err := base64.RawURLEncoding.DecodeString(signature)
	return err == nil && hmac.Equal(got, mac(key, escapedPath, stamp))
}

func mac(key, path, stamp string) []byte {
	h := hmac.New(sha256.New, []byte(key))
	// Domain separation from auth_key and all other uses of the same secret.
	_, _ = h.Write([]byte("xph-server-relay-v1\n" + stamp + "\n" + path))
	return h.Sum(nil)
}
