package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/snnabb/fusion-ride/internal/aggregator"
	"github.com/snnabb/fusion-ride/internal/config"
	"github.com/snnabb/fusion-ride/internal/db"
	"github.com/snnabb/fusion-ride/internal/idmap"
	"github.com/snnabb/fusion-ride/internal/logger"
	"github.com/snnabb/fusion-ride/internal/traffic"
	"github.com/snnabb/fusion-ride/internal/upstream"
)

func openProxyTestDB(t *testing.T) *db.DB {
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

func TestHandleLoginForwardsHeadersAndCreatesUserMapping(t *testing.T) {
	type capturedRequest struct {
		Header http.Header
		Body   string
	}

	var captured capturedRequest
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.Header = r.Header.Clone()
		defer r.Body.Close()
		body, _ := io.ReadAll(r.Body)
		captured.Body = string(body)

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ServerId": "upstream-server",
			"User": map[string]any{
				"Id":   "upstream-user",
				"Name": "demo",
			},
			"AccessToken": "client-token",
		})
	}))
	defer upstreamServer.Close()

	database := openProxyTestDB(t)
	_, err := database.Exec(`
		INSERT INTO upstreams(name, url, playback_mode, spoof_mode, enabled, health_status, session_token)
		VALUES(?, ?, '', 'web', 1, 'online', 'session-token')
	`, "emby-a", upstreamServer.URL)
	if err != nil {
		t.Fatalf("insert upstream failed: %v", err)
	}

	manager := upstream.NewManager(database, logger.New(""), "proxy")
	store := idmap.NewStore(database)
	cfg := config.Default()
	cfg.Server.ID = "fusionride"

	handler := NewHandler(cfg, manager, nil, store, logger.New(""), traffic.NewMeter(database))

	req := httptest.NewRequest(http.MethodPost, "/emby/Users/AuthenticateByName", strings.NewReader(`{"Username":"demo","Pw":"secret"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Emby-Authorization", `MediaBrowser Client="Legacy", Device="Old TV", DeviceId="old-device", Version="1.0.0"`)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d with body %s", rec.Code, rec.Body.String())
	}
	if captured.Body != `{"Username":"demo","Pw":"secret"}` {
		t.Fatalf("expected upstream to receive original login body, got %q", captured.Body)
	}
	if got := captured.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("expected Content-Type to be forwarded, got %q", got)
	}
	if got := captured.Header.Get("X-Emby-Token"); got != "session-token" {
		t.Fatalf("expected upstream session token, got %q", got)
	}
	if got := captured.Header.Get("User-Agent"); !strings.Contains(got, "Emby Theater") {
		t.Fatalf("expected spoofed web User-Agent, got %q", got)
	}
	if got := captured.Header.Get("X-Emby-Authorization"); !strings.Contains(got, `Client="Emby Web"`) {
		t.Fatalf("expected spoofed web authorization client, got %q", got)
	}
	if got := captured.Header.Get("X-Emby-Authorization"); !strings.Contains(got, `Version="4.9.0.42"`) {
		t.Fatalf("expected spoofed web authorization version, got %q", got)
	}

	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal login response failed: %v", err)
	}
	user := payload["User"].(map[string]any)
	if user["Id"] == "upstream-user" {
		t.Fatalf("expected proxied login response to rewrite user id, got %q", user["Id"])
	}
	if payload["ServerId"] != "fusionride" {
		t.Fatalf("expected server id to be rewritten, got %q", payload["ServerId"])
	}

	total, _ := store.Stats()
	if total == 0 {
		t.Fatal("expected login flow to create at least one id mapping")
	}
}

func TestUsersMeUsesLoginSessionWhenUpstreamMeEndpointIsBroken(t *testing.T) {
	var paths []string
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)

		switch r.URL.Path {
		case "/Users/AuthenticateByName":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"AccessToken": "client-token",
				"ServerId":    "upstream-server",
				"User": map[string]any{
					"Id":   "upstream-user",
					"Name": "demo",
				},
			})
		case "/Users/upstream-user":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Id":       "upstream-user",
				"Name":     "demo",
				"ServerId": "upstream-server",
			})
		case "/Users/Me", "/emby/Users/Me":
			http.Error(w, "Unrecognized Guid format.", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstreamServer.Close()

	database := openProxyTestDB(t)
	_, err := database.Exec(`
		INSERT INTO upstreams(name, url, playback_mode, spoof_mode, enabled, health_status, session_token)
		VALUES(?, ?, '', 'web', 1, 'online', 'session-token')
	`, "emby-a", upstreamServer.URL)
	if err != nil {
		t.Fatalf("insert upstream failed: %v", err)
	}

	manager := upstream.NewManager(database, logger.New(""), "proxy")
	store := idmap.NewStore(database)
	cfg := config.Default()
	cfg.Server.ID = "fusionride"
	agg := aggregator.New(manager, store, logger.New(""), time.Second, nil)
	handler := NewHandler(cfg, manager, agg, store, logger.New(""), traffic.NewMeter(database))

	loginReq := httptest.NewRequest(http.MethodPost, "/emby/Users/AuthenticateByName", strings.NewReader(`{"Username":"demo","Pw":"secret"}`))
	loginReq.Header.Set("Content-Type", "application/json")
	loginRec := httptest.NewRecorder()
	handler.ServeHTTP(loginRec, loginReq)
	if loginRec.Code != http.StatusOK {
		t.Fatalf("expected login 200, got %d with body %s", loginRec.Code, loginRec.Body.String())
	}

	var loginPayload map[string]any
	if err := json.Unmarshal(loginRec.Body.Bytes(), &loginPayload); err != nil {
		t.Fatalf("unmarshal login response failed: %v", err)
	}
	token, _ := loginPayload["AccessToken"].(string)
	if token == "" {
		t.Fatal("expected login token to be present")
	}

	req := httptest.NewRequest(http.MethodGet, "/emby/Users/Me", nil)
	req.Header.Set("X-Emby-Token", token)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d with body %s", rec.Code, rec.Body.String())
	}
	if strings.Join(paths, ",") != "/Users/AuthenticateByName,/Users/upstream-user" {
		t.Fatalf("expected login plus /Users/{id} fallback, got %q", strings.Join(paths, ","))
	}

	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal /Users/Me response failed: %v", err)
	}
	if payload["Id"] == "upstream-user" {
		t.Fatalf("expected /Users/Me to return rewritten virtual id, got %q", payload["Id"])
	}
	if payload["ServerId"] != "fusionride" {
		t.Fatalf("expected server id to be rewritten, got %q", payload["ServerId"])
	}
}

func TestIsAggregatablePathExcludesUsersMe(t *testing.T) {
	if isAggregatablePath("/emby/Users/Me") {
		t.Fatal("expected /emby/Users/Me to bypass aggregation")
	}
	if !isAggregatablePath("/emby/Items") {
		t.Fatal("expected /emby/Items to remain aggregatable")
	}
}
