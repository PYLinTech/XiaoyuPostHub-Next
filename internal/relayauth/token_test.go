package relayauth

import (
	"strings"
	"testing"
	"time"
)

func TestObjectScopedRelayCredential(t *testing.T) {
	now := time.Unix(1700000000, 0)
	token := Sign("server-secret", "/objects/a%20b.xph", now.Add(time.Minute))
	if !Verify("server-secret", "/objects/a%20b.xph", token, now) {
		t.Fatal("valid credential denied")
	}
	for _, tc := range []struct {
		name, key, path, token string
		now                    time.Time
	}{
		{"other object", "server-secret", "/objects/other.xph", token, now},
		{"decoded path", "server-secret", "/objects/a b.xph", token, now},
		{"wrong key", "other-secret", "/objects/a%20b.xph", token, now},
		{"missing key", "", "/objects/a%20b.xph", token, now},
		{"missing path", "server-secret", "", token, now},
		{"expired", "server-secret", "/objects/a%20b.xph", token, now.Add(time.Minute)},
		{"tampered expiry", "server-secret", "/objects/a%20b.xph", strings.Replace(token, "1700000060", "1700000660", 1), now},
		{"tampered signature", "server-secret", "/objects/a%20b.xph", token + "x", now},
		{"user ticket", "server-secret", "/objects/a%20b.xph", "tk_user", now},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if Verify(tc.key, tc.path, tc.token, tc.now) {
				t.Fatal("invalid credential accepted")
			}
		})
	}
}
