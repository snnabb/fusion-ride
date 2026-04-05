package upstream

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/snnabb/fusion-ride/internal/auth"
	"github.com/snnabb/fusion-ride/internal/db"
	"github.com/snnabb/fusion-ride/internal/identity"
	"github.com/snnabb/fusion-ride/internal/logger"
)

func openManagerTestDB(t *testing.T) *db.DB {
	t.Helper()

	database, err := db.Open(filepath.Join(t.TempDir(), "fusionride.db"))
	if err != nil {
		t.Fatalf("open test db failed: %v", err)
	}
	t.Cleanup(func() {
		_ = database.Close()
	})
	return database
}

func TestDoAPIWithHeadersPrefersClientToken(t *testing.T) {
	var captured http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer server.Close()

	upstreamInstance := &Upstream{
		Name:    "emby-a",
		URL:     server.URL,
		Session: &auth.UpstreamSession{Token: "session-token"},
		Spoofer: identity.NewSpoofer("web", "", "", "", "", ""),
		Client:  server.Client(),
	}

	headers := make(http.Header)
	headers.Set("X-Emby-Token", "client-token")
	headers.Set("X-Emby-Authorization", `MediaBrowser Client="Legacy", Device="Old TV", DeviceId="old-device", Version="1.0.0"`)

	resp, err := upstreamInstance.DoAPIWithHeaders(context.Background(), http.MethodGet, "/emby/Users/Me", nil, headers)
	if err != nil {
		t.Fatalf("DoAPIWithHeaders failed: %v", err)
	}
	defer resp.Body.Close()

	if got := captured.Get("X-Emby-Token"); got != "client-token" {
		t.Fatalf("expected client token to win, got %q", got)
	}
	if got := captured.Get("X-Emby-Authorization"); !strings.Contains(got, `Client="Emby Web"`) {
		t.Fatalf("expected spoofed authorization header, got %q", got)
	}
}

func TestUpdateReturnsChineseFieldErrors(t *testing.T) {
	database := openManagerTestDB(t)
	_, err := database.Exec(`
		INSERT INTO upstreams(id, name, url, playback_mode, spoof_mode, enabled, health_status, session_token)
		VALUES(?, ?, ?, 'proxy', 'infuse', 1, 'online', 'token')
	`, 1, "emby-a", "https://example.com")
	if err != nil {
		t.Fatalf("insert upstream failed: %v", err)
	}

	manager := NewManager(database, logger.New(""), "proxy")

	if err := manager.Update(99, map[string]any{"name": "demo"}); err == nil || err.Error() != "上游 99 不存在" {
		t.Fatalf("expected missing upstream error, got %v", err)
	}
	if err := manager.Update(1, map[string]any{"priority": "high"}); err == nil || err.Error() != "上游 1 的优先级类型错误" {
		t.Fatalf("expected priority type error, got %v", err)
	}
	if err := manager.Update(1, map[string]any{"unknown": "value"}); err == nil || err.Error() != "不支持的字段 unknown" {
		t.Fatalf("expected unsupported field error, got %v", err)
	}
}

func TestDoAPIJSONReturnsChineseHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "downstream failed", http.StatusBadGateway)
	}))
	defer server.Close()

	upstreamInstance := &Upstream{
		Name:    "emby-a",
		URL:     server.URL,
		Session: &auth.UpstreamSession{Token: "session-token"},
		Spoofer: identity.NewSpoofer("web", "", "", "", "", ""),
		Client:  server.Client(),
	}

	err := upstreamInstance.DoAPIJSON(context.Background(), http.MethodGet, "/emby/Items", nil, &map[string]any{})
	if err == nil {
		t.Fatal("expected DoAPIJSON to fail on non-200 response")
	}
	if err.Error() != "上游返回 HTTP 502: downstream failed\n" {
		t.Fatalf("expected Chinese HTTP error, got %q", err.Error())
	}
}
