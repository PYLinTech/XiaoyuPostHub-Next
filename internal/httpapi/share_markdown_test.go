package httpapi

import (
	"context"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/settings"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/store"
	"net/http"
	"strings"
	"testing"
)

func TestShareMarkdownPersistenceAndLimits(t *testing.T) {
	env := newTestEnv(t)
	token := env.login(t)
	seedArchiveFixture(t, env)
	ctx := context.Background()
	content := "# 说明\n\n你好😀"
	status, payload := env.do(t, http.MethodPost, "/api/share", map[string]any{"path": "/d/a.txt", "customMarkdown": content}, token)
	if status != 200 {
		t.Fatalf("create: %d %v", status, payload)
	}
	share := payload["data"].(map[string]any)["share"].(map[string]any)
	id := share["id"].(string)
	saved, err := store.GetShare(ctx, env.db.R(), id)
	if err != nil || saved.CustomMarkdown != content {
		t.Fatalf("persistence: %+v %v", saved, err)
	}
	status, payload = env.do(t, http.MethodPost, "/api/s/"+id+"/resolve", map[string]any{}, "")
	if status != 200 || payload["data"].(map[string]any)["share"].(map[string]any)["customMarkdown"] != content {
		t.Fatalf("guest: %d %v", status, payload)
	}

	status, payload = env.do(t, http.MethodPatch, "/api/share/"+id, map[string]any{"accessMode": "password", "password": "secret"}, token)
	if status != 200 {
		t.Fatalf("password config: %d %v", status, payload)
	}
	status, payload = env.do(t, http.MethodPost, "/api/s/"+id+"/resolve", map[string]any{"password": "secret"}, "")
	if status != 200 || payload["data"].(map[string]any)["share"].(map[string]any)["customMarkdown"] != content {
		t.Fatalf("authorized content: %d %v", status, payload)
	}
	status, payload = env.do(t, http.MethodPost, "/api/s/"+id+"/resolve", map[string]any{}, "")
	if status != http.StatusForbidden || payload["data"] != nil {
		t.Fatalf("protected content leaked: %d %v", status, payload)
	}
	config := settings.NewStore(env.db, nil)
	if config.Runtime(ctx).Share.MarkdownMaxChars != 524288 {
		t.Fatal("wrong default")
	}
	if err := config.Set(ctx, settings.KeyShareMarkdownMaxChars, "3", 1); err != nil {
		t.Fatal(err)
	}
	env.settings.Invalidate()
	for _, method := range []string{http.MethodPost, http.MethodPatch} {
		endpoint := "/api/share"
		if method == http.MethodPatch {
			endpoint += "/" + id
		}
		for _, tc := range []struct {
			value string
			want  int
		}{{"中😀文", 200}, {"中😀文字", 400}} {
			status, payload = env.do(t, method, endpoint, map[string]any{"path": "/d/a.txt", "customMarkdown": tc.value}, token)
			if status != tc.want {
				t.Fatalf("%s: %d %v", method, status, payload)
			}
		}
	}
	status, _ = env.do(t, http.MethodPatch, "/api/share/"+id, map[string]any{"maxVisits": 5}, token)
	if status != 200 {
		t.Fatal("unrelated update failed")
	}
	status, _ = env.do(t, http.MethodPatch, "/api/share/"+id, map[string]any{"customMarkdown": ""}, token)
	if status != 200 {
		t.Fatal("clear failed")
	}
	saved, _ = store.GetShare(ctx, env.db.R(), id)
	if saved.CustomMarkdown != "" {
		t.Fatal("clear not persisted")
	}
	if err := config.Set(ctx, settings.KeyShareMarkdownMaxChars, "524288", 1); err != nil {
		t.Fatal(err)
	}
	env.settings.Invalidate()
	// UTF-8 Chinese content at the default boundary exceeds the normal 1 MiB body limit.
	status, payload = env.do(t, http.MethodPost, "/api/share", map[string]any{"path": "/d/a.txt", "customMarkdown": strings.Repeat("中", 524288)}, token)
	if status != 200 {
		t.Fatalf("default boundary: %d %v", status, payload)
	}
}
