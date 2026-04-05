package upstream

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/snnabb/fusion-ride/internal/auth"
	"github.com/snnabb/fusion-ride/internal/identity"
)

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
