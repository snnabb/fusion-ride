package proxy

import (
	"bytes"
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

func TestHandleLoginTriesNextOnlineUpstream(t *testing.T) {
	firstServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad credentials", http.StatusUnauthorized)
	}))
	defer firstServer.Close()

	secondCalled := 0
	secondServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondCalled++
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
	defer secondServer.Close()

	database := openProxyTestDB(t)
	_, err := database.Exec(`
		INSERT INTO upstreams(id, name, url, playback_mode, spoof_mode, enabled, health_status, session_token)
		VALUES(?, ?, ?, '', 'web', 1, 'online', 'token-a')
	`, 1, "emby-a", firstServer.URL)
	if err != nil {
		t.Fatalf("insert first upstream failed: %v", err)
	}
	_, err = database.Exec(`
		INSERT INTO upstreams(id, name, url, playback_mode, spoof_mode, enabled, health_status, session_token)
		VALUES(?, ?, ?, '', 'web', 1, 'online', 'token-b')
	`, 2, "emby-b", secondServer.URL)
	if err != nil {
		t.Fatalf("insert second upstream failed: %v", err)
	}

	manager := upstream.NewManager(database, logger.New(""), "proxy")
	store := idmap.NewStore(database)
	cfg := config.Default()
	cfg.Server.ID = "fusionride"

	handler := NewHandler(cfg, manager, nil, store, logger.New(""), traffic.NewMeter(database))

	req := httptest.NewRequest(http.MethodPost, "/emby/Users/AuthenticateByName", strings.NewReader(`{"Username":"demo","Pw":"secret"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected login to succeed against second upstream, got %d with body %s", rec.Code, rec.Body.String())
	}
	if secondCalled != 1 {
		t.Fatalf("expected second upstream to be tried once, got %d", secondCalled)
	}
}

func TestHandleLoginReturnsUnauthorizedWhenAllUpstreamsFail(t *testing.T) {
	forbiddenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "denied", http.StatusForbidden)
	}))
	defer forbiddenServer.Close()

	unauthorizedServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad credentials", http.StatusUnauthorized)
	}))
	defer unauthorizedServer.Close()

	database := openProxyTestDB(t)
	_, err := database.Exec(`
		INSERT INTO upstreams(id, name, url, playback_mode, spoof_mode, enabled, health_status, session_token)
		VALUES(?, ?, ?, '', 'web', 1, 'online', 'token-a')
	`, 1, "emby-a", forbiddenServer.URL)
	if err != nil {
		t.Fatalf("insert first upstream failed: %v", err)
	}
	_, err = database.Exec(`
		INSERT INTO upstreams(id, name, url, playback_mode, spoof_mode, enabled, health_status, session_token)
		VALUES(?, ?, ?, '', 'web', 1, 'online', 'token-b')
	`, 2, "emby-b", unauthorizedServer.URL)
	if err != nil {
		t.Fatalf("insert second upstream failed: %v", err)
	}

	manager := upstream.NewManager(database, logger.New(""), "proxy")
	store := idmap.NewStore(database)
	handler := NewHandler(config.Default(), manager, nil, store, logger.New(""), traffic.NewMeter(database))

	req := httptest.NewRequest(http.MethodPost, "/emby/Users/AuthenticateByName", strings.NewReader(`{"Username":"demo","Pw":"secret"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 when all upstreams fail, got %d", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "所有上游均登录失败") {
		t.Fatalf("expected Chinese aggregate login failure, got %q", body)
	}
}

func TestSessionCleanupRemovesExpiredEntriesAndRefreshesAccess(t *testing.T) {
	handler := &Handler{
		sessionMaxAge:          24 * time.Hour,
		sessionCleanupInterval: 10 * time.Minute,
		sessions:               make(map[string]loginSession),
	}

	oldTime := time.Now().Add(-48 * time.Hour)
	handler.sessions["expired-token"] = loginSession{
		UpstreamID:     1,
		UpstreamUserID: "user-old",
		VirtualUserID:  "virtual-old",
		lastAccess:     oldTime,
	}
	handler.sessions["fresh-token"] = loginSession{
		UpstreamID:     1,
		UpstreamUserID: "user-fresh",
		VirtualUserID:  "virtual-fresh",
		lastAccess:     oldTime,
	}

	session, ok := handler.lookupSession("fresh-token")
	if !ok {
		t.Fatal("expected fresh token lookup to succeed")
	}
	if session.lastAccess.Before(oldTime) {
		t.Fatalf("expected lookup to refresh lastAccess, got %s", session.lastAccess)
	}

	removed := handler.cleanupExpiredSessions(time.Now())
	if removed != 1 {
		t.Fatalf("expected one expired session to be removed, got %d", removed)
	}
	if _, ok := handler.lookupSession("expired-token"); ok {
		t.Fatal("expected expired token to be removed")
	}
	if _, ok := handler.lookupSession("fresh-token"); !ok {
		t.Fatal("expected fresh token to remain after cleanup")
	}
}

func TestHandlePlaybackInfoRoutesToOwningUpstreamAndStoresPlaySession(t *testing.T) {
	var capturedPath string
	var capturedBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.RequestURI()
		defer r.Body.Close()
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &capturedBody)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"MediaSources": []map[string]any{{
				"Id":     "media-1",
				"ItemId": "item-original",
			}},
			"PlaySessionId": "play-1",
		})
	}))
	defer server.Close()

	database := openProxyTestDB(t)
	_, err := database.Exec(`
		INSERT INTO upstreams(id, name, url, playback_mode, spoof_mode, enabled, health_status, session_token)
		VALUES(?, ?, ?, 'proxy', 'web', 1, 'online', 'token')
	`, 2, "emby-b", server.URL)
	if err != nil {
		t.Fatalf("insert upstream failed: %v", err)
	}

	manager := upstream.NewManager(database, logger.New(""), "proxy")
	store := idmap.NewStore(database)
	cfg := config.Default()
	cfg.Server.ID = "fusionride"
	handler := NewHandler(cfg, manager, nil, store, logger.New(""), traffic.NewMeter(database))

	virtualID := store.GetOrCreate("item-original", 2, "Movie")
	req := httptest.NewRequest(http.MethodPost, "/emby/Items/"+virtualID+"/PlaybackInfo", strings.NewReader(`{"ItemId":"`+virtualID+`"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected playback info 200, got %d with body %s", rec.Code, rec.Body.String())
	}
	if capturedPath != "/emby/Items/item-original/PlaybackInfo" {
		t.Fatalf("expected playback info path to be devirtualized, got %q", capturedPath)
	}
	if got := capturedBody["ItemId"]; got != "item-original" {
		t.Fatalf("expected playback info body item id to be devirtualized, got %v", got)
	}

	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal playback info failed: %v", err)
	}
	mediaSources := payload["MediaSources"].([]any)
	if mediaSources[0].(map[string]any)["ItemId"] != virtualID {
		t.Fatalf("expected playback info response item id to be virtualized, got %v", mediaSources[0].(map[string]any)["ItemId"])
	}
	if session, ok := handler.lookupPlaybackSession("play-1"); !ok || session.UpstreamID != 2 {
		t.Fatalf("expected playback session to be recorded for upstream 2, got %#v, ok=%v", session, ok)
	}
}

func TestHandlePlaybackReportUsesRecordedUpstreamAndDevirtualizesBody(t *testing.T) {
	var called bool
	var capturedBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		defer r.Body.Close()
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &capturedBody)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	database := openProxyTestDB(t)
	_, err := database.Exec(`
		INSERT INTO upstreams(id, name, url, playback_mode, spoof_mode, enabled, health_status, session_token)
		VALUES(?, ?, ?, 'proxy', 'web', 1, 'online', 'token')
	`, 2, "emby-b", server.URL)
	if err != nil {
		t.Fatalf("insert upstream failed: %v", err)
	}

	manager := upstream.NewManager(database, logger.New(""), "proxy")
	store := idmap.NewStore(database)
	handler := NewHandler(config.Default(), manager, nil, store, logger.New(""), traffic.NewMeter(database))

	virtualID := store.GetOrCreate("item-original", 2, "Movie")
	handler.rememberPlaybackSession("play-1", playbackSession{
		UpstreamID:     2,
		OriginalItemID: "item-original",
		VirtualItemID:  virtualID,
		lastAccess:     time.Now(),
	})

	req := httptest.NewRequest(http.MethodPost, "/emby/Sessions/Playing/Progress", strings.NewReader(`{"PlaySessionId":"play-1","ItemId":"`+virtualID+`"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected playback report 204, got %d with body %s", rec.Code, rec.Body.String())
	}
	if !called {
		t.Fatal("expected playback report to be forwarded to recorded upstream")
	}
	if got := capturedBody["ItemId"]; got != "item-original" {
		t.Fatalf("expected playback report item id to be devirtualized, got %v", got)
	}
}

func TestHandleFallbackVirtualizesJSONAndDevirtualizesRequestBody(t *testing.T) {
	var capturedBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &capturedBody)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ItemId": "item-original",
		})
	}))
	defer server.Close()

	database := openProxyTestDB(t)
	_, err := database.Exec(`
		INSERT INTO upstreams(id, name, url, playback_mode, spoof_mode, enabled, health_status, session_token)
		VALUES(?, ?, ?, 'proxy', 'web', 1, 'online', 'session-token')
	`, 2, "emby-b", server.URL)
	if err != nil {
		t.Fatalf("insert upstream failed: %v", err)
	}

	manager := upstream.NewManager(database, logger.New(""), "proxy")
	store := idmap.NewStore(database)
	handler := NewHandler(config.Default(), manager, nil, store, logger.New(""), traffic.NewMeter(database))

	virtualID := store.GetOrCreate("item-original", 2, "Movie")
	handler.rememberSession("client-token", loginSession{
		UpstreamID:     2,
		UpstreamUserID: "user-2",
		VirtualUserID:  "virtual-user",
	})

	req := httptest.NewRequest(http.MethodPost, "/emby/Favorites", strings.NewReader(`{"ItemId":"`+virtualID+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Emby-Token", "client-token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected fallback 200, got %d with body %s", rec.Code, rec.Body.String())
	}
	if got := capturedBody["ItemId"]; got != "item-original" {
		t.Fatalf("expected fallback body item id to be devirtualized, got %v", got)
	}

	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal fallback response failed: %v", err)
	}
	if payload["ItemId"] != virtualID {
		t.Fatalf("expected fallback response item id to be virtualized, got %v", payload["ItemId"])
	}
}

func TestHandleImageCachesSmallResponses(t *testing.T) {
	requests := 0
	imageBytes := []byte("small-image")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(imageBytes)
	}))
	defer server.Close()

	database := openProxyTestDB(t)
	_, err := database.Exec(`
		INSERT INTO upstreams(id, name, url, playback_mode, spoof_mode, enabled, health_status, session_token)
		VALUES(?, ?, ?, 'proxy', 'web', 1, 'online', 'token')
	`, 2, "emby-b", server.URL)
	if err != nil {
		t.Fatalf("insert upstream failed: %v", err)
	}

	manager := upstream.NewManager(database, logger.New(""), "proxy")
	store := idmap.NewStore(database)
	handler := NewHandler(config.Default(), manager, nil, store, logger.New(""), traffic.NewMeter(database))

	virtualID := store.GetOrCreate("item-original", 2, "Movie")
	path := "/emby/Items/" + virtualID + "/Images/Primary"

	firstReq := httptest.NewRequest(http.MethodGet, path, nil)
	firstRec := httptest.NewRecorder()
	handler.ServeHTTP(firstRec, firstReq)

	secondReq := httptest.NewRequest(http.MethodGet, path, nil)
	secondRec := httptest.NewRecorder()
	handler.ServeHTTP(secondRec, secondReq)

	if firstRec.Code != http.StatusOK || secondRec.Code != http.StatusOK {
		t.Fatalf("expected image requests to succeed, got %d and %d", firstRec.Code, secondRec.Code)
	}
	if requests != 1 {
		t.Fatalf("expected second image request to hit cache, got %d upstream requests", requests)
	}
	if !bytes.Equal(firstRec.Body.Bytes(), secondRec.Body.Bytes()) {
		t.Fatal("expected cached image bytes to match original response")
	}
}
