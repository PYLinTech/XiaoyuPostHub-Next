package httpapi

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/backend"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/relayauth"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/settings"
)

func TestRelayCDNAuthIsObjectScopedAndDoesNotConsumeTickets(t *testing.T) {
	env := newTestEnv(t)
	const key = "relay-unit-test-secret"
	if err := env.settings.Set(context.Background(), settings.KeyPan123PrivateKey, key, 0); err != nil {
		t.Fatal(err)
	}
	path := "/objects/a%20b%2Fc.xph"
	token := relayauth.Sign(key, path, time.Now().Add(time.Minute))
	for _, encoded := range []bool{false, true} {
		uri := path + "?auth_key=test&xph=" + token
		if encoded {
			uri = url.QueryEscape(uri)
		}
		callback := "/api/cdn/auth?remote_addr=1.2.3.4&request_uri=" + uri
		for i := 0; i < 8; i++ {
			status, _ := env.do(t, http.MethodGet, callback, nil, "")
			if status != 200 {
				t.Fatalf("encoded=%t request=%d status=%d", encoded, i, status)
			}
		}
	}
	for _, uri := range []string{
		"/other.xph?xph=" + token,
		path + "?xph=" + token + "x",
		path + "?xph=" + relayauth.Sign(key, path, time.Now().Add(-time.Second)),
	} {
		status, _ := env.do(t, http.MethodGet, "/api/cdn/auth?request_uri="+url.QueryEscape(uri), nil, "")
		if status != 403 {
			t.Fatalf("invalid credential status=%d", status)
		}
	}
	status, _ := env.do(t, http.MethodGet, "/api/cdn/auth?xph="+token, nil, "")
	if status != 403 {
		t.Fatalf("missing path status=%d", status)
	}
	var tickets int
	if err := env.db.R().QueryRowContext(context.Background(), "SELECT COUNT(*) FROM download_tickets").Scan(&tickets); err != nil {
		t.Fatal(err)
	}
	if tickets != 0 {
		t.Fatalf("relay created/consumed user tickets: %d", tickets)
	}
}

// Exercise the production Pan123 signer -> HEAD and Range -> real callback route,
// including the unescaped '&' shape observed in production. No self-download API
// may be used when remote authentication is enabled.
func TestPan123RelaySurvivesRealCDNCallback(t *testing.T) {
	env := newTestEnv(t)
	const key = "relay-integration-secret"
	if err := env.settings.Set(context.Background(), settings.KeyPan123PrivateKey, key, 0); err != nil {
		t.Fatal(err)
	}
	var fallback, callbacks atomic.Int64
	content := strings.Repeat("encrypted-data", 1000)
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callback := env.server.URL + "/api/cdn/auth?remote_addr=111.229.227.9&request_uri=" + r.URL.RequestURI()
		res, err := http.Get(callback)
		if err != nil {
			http.Error(w, "callback unavailable", 502)
			return
		}
		_ = res.Body.Close()
		callbacks.Add(1)
		if res.StatusCode != 200 {
			http.Error(w, "denied", 403)
			return
		}
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", fmt.Sprint(len(content)))
			return
		}
		var start, end int
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil {
			http.Error(w, "bad range", 416)
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(content)))
		w.WriteHeader(206)
		_, _ = io.WriteString(w, content[start:end+1])
	}))
	defer cdn.Close()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/access_token":
			fmt.Fprintf(w, `{"code":0,"data":{"accessToken":"token","expiredAt":%q}}`, time.Now().Add(time.Hour).Format(time.RFC3339))
		case "/api/v1/user/info":
			fmt.Fprint(w, `{"code":0,"data":{"uid":123}}`)
		case "/api/v1/direct-link/url":
			fmt.Fprintf(w, `{"code":0,"data":{"url":%q}}`, cdn.URL+"/objects/file.xph")
		case "/api/v1/file/download_info":
			fallback.Add(1)
			http.Error(w, "self-download forbidden in test", 500)
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()
	b, err := backend.NewPan123(backend.Pan123Config{ClientID: "id", ClientSecret: "secret", PrivateKey: key, APIBase: api.URL, DirectLinkTTL: time.Minute, UploadThreads: 1, PollInterval: time.Second, PollAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	reader, err := b.OpenRange(context.Background(), "123", 7, 123)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	got, err := io.ReadAll(reader)
	if err != nil || string(got) != content[7:130] {
		t.Fatalf("read size=%d err=%v", len(got), err)
	}
	if fallback.Load() != 0 || callbacks.Load() != 2 {
		t.Fatalf("fallback=%d callbacks=%d", fallback.Load(), callbacks.Load())
	}
}
