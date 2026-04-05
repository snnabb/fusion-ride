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
	"github.com/snnabb/fusion-ride/internal/auth"
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

func setupProxyAdmin(t *testing.T, database *db.DB, username, password string) *auth.AdminAuth {
	t.Helper()

	adminAuth := auth.NewAdminAuth(database, "")
	if err := adminAuth.Setup(username, password); err != nil {
		t.Fatalf("setup proxy admin failed: %v", err)
	}
	return adminAuth
}

func TestNewHandlerPersistsStableProxyUserID(t *testing.T) {
	database := openProxyTestDB(t)
	manager := upstream.NewManager(database, logger.New(""), "proxy")
	store := idmap.NewStore(database)
	cfg := config.Default()

	first := NewHandler(cfg, database, manager, nil, store, logger.New(""), traffic.NewMeter(database))
	second := NewHandler(cfg, database, manager, nil, idmap.NewStore(database), logger.New(""), traffic.NewMeter(database))

	if first.proxyUserID == "" {
		t.Fatal("expected proxy user id to be initialized")
	}
	if second.proxyUserID != first.proxyUserID {
		t.Fatalf("expected proxy user id to persist, got %q and %q", first.proxyUserID, second.proxyUserID)
	}

	var stored string
	if err := database.QueryRow(`SELECT value FROM meta WHERE key = 'proxy_user_id'`).Scan(&stored); err != nil {
		t.Fatalf("query proxy user id failed: %v", err)
	}
	if stored != first.proxyUserID {
		t.Fatalf("expected proxy user id %q to be persisted, got %q", first.proxyUserID, stored)
	}
}

func TestHandleUsersPublicReturnsProxyIdentity(t *testing.T) {
	database := openProxyTestDB(t)
	manager := upstream.NewManager(database, logger.New(""), "proxy")
	store := idmap.NewStore(database)
	cfg := config.Default()
	cfg.Server.ID = "fusionride"
	cfg.Admin.Username = "admin-user"

	handler := NewHandler(cfg, database, manager, nil, store, logger.New(""), traffic.NewMeter(database))

	req := httptest.NewRequest(http.MethodGet, "/Users/Public", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d with body %s", rec.Code, rec.Body.String())
	}

	var payload []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal users public failed: %v", err)
	}
	if len(payload) != 1 {
		t.Fatalf("expected one public user, got %d", len(payload))
	}
	if payload[0]["Name"] != "admin-user" {
		t.Fatalf("expected admin username, got %#v", payload[0]["Name"])
	}
	if payload[0]["ServerId"] != "fusionride" {
		t.Fatalf("expected fusionride server id, got %#v", payload[0]["ServerId"])
	}
	if payload[0]["Id"] != handler.proxyUserID {
		t.Fatalf("expected proxy user id %q, got %#v", handler.proxyUserID, payload[0]["Id"])
	}
}

func TestHandleLoginReturnsStableProxyUserID(t *testing.T) {
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
		switch r.URL.Path {
		case "/Users/upstream-user":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Id":       "upstream-user",
				"Name":     "Proxy Admin",
				"ServerId": "upstream-server",
			})
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
	cfg.Admin.Username = "proxy-admin"
	cfg.Admin.Password = "proxy-secret"
	setupProxyAdmin(t, database, cfg.Admin.Username, cfg.Admin.Password)

	handler := NewHandler(cfg, database, manager, nil, store, logger.New(""), traffic.NewMeter(database))
	upstreamInstance := manager.ByID(1)
	if upstreamInstance == nil {
		t.Fatal("expected upstream instance")
	}
	upstreamInstance.Mu.Lock()
	upstreamInstance.HealthStatus = "online"
	upstreamInstance.Session = &auth.UpstreamSession{Token: "session-token", UserID: "upstream-user"}
	upstreamInstance.Mu.Unlock()

	req := httptest.NewRequest(http.MethodPost, "/emby/Users/AuthenticateByName", strings.NewReader(`{"Username":"proxy-admin","Pw":"proxy-secret"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Emby-Authorization", `MediaBrowser Client="Legacy", Device="Old TV", DeviceId="old-device", Version="1.0.0"`)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d with body %s", rec.Code, rec.Body.String())
	}
	if captured.Body != "" {
		t.Fatalf("expected login not to hit upstream auth endpoint, got %q", captured.Body)
	}

	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal login response failed: %v", err)
	}
	user := payload["User"].(map[string]any)
	if user["Id"] != handler.proxyUserID {
		t.Fatalf("expected proxied login response to use proxy user id %q, got %q", handler.proxyUserID, user["Id"])
	}
	if user["ServerId"] != "fusionride" {
		t.Fatalf("expected user server id to be rewritten, got %q", user["ServerId"])
	}
	if payload["ServerId"] != "fusionride" {
		t.Fatalf("expected server id to be rewritten, got %q", payload["ServerId"])
	}
	token, _ := payload["AccessToken"].(string)
	if token == "" || token == "session-token" {
		t.Fatalf("expected proxy-issued token, got %q", token)
	}
	session, ok := handler.lookupSession(token)
	if !ok {
		t.Fatal("expected login session to be stored")
	}
	if session.UpstreamID != 1 || session.UpstreamUserID != "upstream-user" {
		t.Fatalf("expected login session to bind upstream user, got %#v", session)
	}
}

func TestUsersMeReturnsProxyIdentity(t *testing.T) {
	var paths []string
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)

		switch r.URL.Path {
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
	cfg.Admin.Username = "proxy-admin"
	cfg.Admin.Password = "proxy-secret"
	setupProxyAdmin(t, database, cfg.Admin.Username, cfg.Admin.Password)
	agg := aggregator.New(manager, store, logger.New(""), time.Second, nil)
	handler := NewHandler(cfg, database, manager, agg, store, logger.New(""), traffic.NewMeter(database))
	upstreamInstance := manager.ByID(1)
	if upstreamInstance == nil {
		t.Fatal("expected upstream instance")
	}
	upstreamInstance.Mu.Lock()
	upstreamInstance.HealthStatus = "online"
	upstreamInstance.Session = &auth.UpstreamSession{Token: "session-token", UserID: "upstream-user"}
	upstreamInstance.Mu.Unlock()

	loginReq := httptest.NewRequest(http.MethodPost, "/emby/Users/AuthenticateByName", strings.NewReader(`{"Username":"proxy-admin","Pw":"proxy-secret"}`))
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
	if len(paths) != 0 {
		t.Fatalf("expected /Users/Me not to hit upstream, got %q", strings.Join(paths, ","))
	}

	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal /Users/Me response failed: %v", err)
	}
	if payload["Name"] != "proxy-admin" {
		t.Fatalf("expected /Users/Me to return proxy admin name, got %#v", payload["Name"])
	}
	if payload["Id"] != handler.proxyUserID {
		t.Fatalf("expected /Users/Me to return proxy user id %q, got %q", handler.proxyUserID, payload["Id"])
	}
	if payload["ServerId"] != "fusionride" {
		t.Fatalf("expected server id to be rewritten, got %q", payload["ServerId"])
	}
	policy, ok := payload["Policy"].(map[string]any)
	if !ok {
		t.Fatalf("expected policy payload, got %#v", payload["Policy"])
	}
	if policy["IsAdministrator"] != true {
		t.Fatalf("expected proxy admin policy, got %#v", policy["IsAdministrator"])
	}
}

func TestProxyUserRootPathReturnsProxyIdentity(t *testing.T) {
	database := openProxyTestDB(t)
	manager := upstream.NewManager(database, logger.New(""), "proxy")
	store := idmap.NewStore(database)
	cfg := config.Default()
	cfg.Server.ID = "fusionride"
	cfg.Admin.Username = "proxy-admin"
	cfg.Admin.Password = "proxy-secret"
	setupProxyAdmin(t, database, cfg.Admin.Username, cfg.Admin.Password)

	handler := NewHandler(cfg, database, manager, nil, store, logger.New(""), traffic.NewMeter(database))
	handler.rememberSession("client-token", loginSession{
		UpstreamID:     1,
		UpstreamUserID: "upstream-user",
		ProxyUserID:    handler.proxyUserID,
		lastAccess:     time.Now(),
	})

	req := httptest.NewRequest(http.MethodGet, "/emby/Users/"+handler.proxyUserID, nil)
	req.Header.Set("X-Emby-Token", "client-token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d with body %s", rec.Code, rec.Body.String())
	}

	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal proxy user response failed: %v", err)
	}
	if payload["Name"] != "proxy-admin" {
		t.Fatalf("expected proxy identity name, got %#v", payload["Name"])
	}
	if payload["Id"] != handler.proxyUserID {
		t.Fatalf("expected proxy identity id %q, got %#v", handler.proxyUserID, payload["Id"])
	}
}
func TestHandleSystemInfoPublicReturnsCompatiblePayload(t *testing.T) {
	database := openProxyTestDB(t)
	manager := upstream.NewManager(database, logger.New(""), "proxy")
	handler := NewHandler(config.Default(), database, manager, nil, idmap.NewStore(database), logger.New(""), traffic.NewMeter(database))
	handler.cfg.Server.Name = "FusionRide"
	handler.cfg.Server.ID = "fusionride"

	req := httptest.NewRequest(http.MethodGet, "http://media.example:8096/System/Info/Public", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d with body %s", rec.Code, rec.Body.String())
	}

	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal /System/Info/Public failed: %v", err)
	}
	if payload["StartupWizardCompleted"] != true {
		t.Fatalf("expected StartupWizardCompleted=true, got %#v", payload["StartupWizardCompleted"])
	}
	if payload["ServerName"] != "FusionRide" {
		t.Fatalf("expected FusionRide server name, got %#v", payload["ServerName"])
	}
	if payload["Id"] != "fusionride" {
		t.Fatalf("expected FusionRide id, got %#v", payload["Id"])
	}
	if payload["ProductName"] != "Emby Server" {
		t.Fatalf("expected Emby Server product name, got %#v", payload["ProductName"])
	}
	if payload["LocalAddress"] != "http://media.example:8096" {
		t.Fatalf("expected LocalAddress to use request host, got %#v", payload["LocalAddress"])
	}
	if payload["CanSelfRestart"] != false {
		t.Fatalf("expected CanSelfRestart=false, got %#v", payload["CanSelfRestart"])
	}
	if payload["SupportsLibraryMonitor"] != true {
		t.Fatalf("expected SupportsLibraryMonitor=true, got %#v", payload["SupportsLibraryMonitor"])
	}
}

func TestHandleSystemInfoAuthReturnsExtendedPayload(t *testing.T) {
	database := openProxyTestDB(t)
	manager := upstream.NewManager(database, logger.New(""), "proxy")
	handler := NewHandler(config.Default(), database, manager, nil, idmap.NewStore(database), logger.New(""), traffic.NewMeter(database))
	handler.cfg.Server.Name = "FusionRide"
	handler.cfg.Server.ID = "fusionride"
	handler.rememberSession("client-token", loginSession{
		UpstreamID:     1,
		UpstreamUserID: "upstream-user",
		ProxyUserID:    handler.proxyUserID,
		lastAccess:     time.Now(),
	})

	req := httptest.NewRequest(http.MethodGet, "http://media.example:8096/emby/System/Info", nil)
	req.Header.Set("X-Emby-Token", "client-token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d with body %s", rec.Code, rec.Body.String())
	}

	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal /System/Info failed: %v", err)
	}
	if payload["StartupWizardCompleted"] != true {
		t.Fatalf("expected StartupWizardCompleted=true, got %#v", payload["StartupWizardCompleted"])
	}
	if payload["ServerName"] != "FusionRide" {
		t.Fatalf("expected FusionRide server name, got %#v", payload["ServerName"])
	}
	if payload["Id"] != "fusionride" {
		t.Fatalf("expected FusionRide id, got %#v", payload["Id"])
	}
	if payload["ProductName"] != "Emby Server" {
		t.Fatalf("expected Emby Server product name, got %#v", payload["ProductName"])
	}
	if payload["LocalAddress"] != "http://media.example:8096" {
		t.Fatalf("expected LocalAddress to use request host, got %#v", payload["LocalAddress"])
	}
	if payload["WanAddress"] != "http://media.example:8096" {
		t.Fatalf("expected WanAddress to use request host, got %#v", payload["WanAddress"])
	}
	if payload["OperatingSystemDisplayName"] == nil {
		t.Fatal("expected OperatingSystemDisplayName to be present")
	}
	if payload["CompletedInstallations"] == nil {
		t.Fatal("expected CompletedInstallations to be present")
	}
}

func TestHandleSystemInfoAuthRejectsUnknownToken(t *testing.T) {
	database := openProxyTestDB(t)
	manager := upstream.NewManager(database, logger.New(""), "proxy")
	handler := NewHandler(config.Default(), database, manager, nil, idmap.NewStore(database), logger.New(""), traffic.NewMeter(database))

	req := httptest.NewRequest(http.MethodGet, "http://media.example:8096/emby/System/Info", nil)
	req.Header.Set("X-Emby-Token", "missing-token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d with body %s", rec.Code, rec.Body.String())
	}
}

func TestHandleProxyUserRequestRoutesWithSessionUserID(t *testing.T) {
	var capturedPath string
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.RequestURI()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"Items": []map[string]any{{
				"Id":   "item-original",
				"Name": "Demo",
			}},
			"TotalRecordCount": 1,
		})
	}))
	defer upstreamServer.Close()

	database := openProxyTestDB(t)
	_, err := database.Exec(`
		INSERT INTO upstreams(id, name, url, playback_mode, spoof_mode, enabled, health_status, session_token)
		VALUES(?, ?, ?, '', 'web', 1, 'online', 'session-token')
	`, 2, "emby-a", upstreamServer.URL)
	if err != nil {
		t.Fatalf("insert upstream failed: %v", err)
	}

	manager := upstream.NewManager(database, logger.New(""), "proxy")
	store := idmap.NewStore(database)
	cfg := config.Default()
	cfg.Server.ID = "fusionride"
	handler := NewHandler(cfg, database, manager, nil, store, logger.New(""), traffic.NewMeter(database))
	handler.rememberSession("client-token", loginSession{
		UpstreamID:     2,
		UpstreamUserID: "upstream-user",
		ProxyUserID:    handler.proxyUserID,
		lastAccess:     time.Now(),
	})

	virtualItemID := store.GetOrCreate("item-original", 2, "Movie")

	req := httptest.NewRequest(http.MethodGet, "/emby/Users/"+handler.proxyUserID+"/Items/Latest", nil)
	req.Header.Set("X-Emby-Token", "client-token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d with body %s", rec.Code, rec.Body.String())
	}
	if capturedPath != "/emby/Users/upstream-user/Items/Latest" {
		t.Fatalf("expected user path to be rewritten, got %q", capturedPath)
	}

	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal proxy user response failed: %v", err)
	}
	items := payload["Items"].([]any)
	if items[0].(map[string]any)["Id"] != virtualItemID {
		t.Fatalf("expected item id to be virtualized, got %#v", items[0].(map[string]any)["Id"])
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

func TestHandleLoginUsesPasswordFieldFallback(t *testing.T) {
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/Users/upstream-user" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"Id":       "upstream-user",
			"Name":     "demo",
			"ServerId": "upstream-server",
		})
	}))
	defer upstreamServer.Close()

	database := openProxyTestDB(t)
	_, err := database.Exec(`
		INSERT INTO upstreams(id, name, url, playback_mode, spoof_mode, enabled, health_status, session_token)
		VALUES(?, ?, ?, '', 'web', 1, 'online', 'token-a')
	`, 1, "emby-a", upstreamServer.URL)
	if err != nil {
		t.Fatalf("insert upstream failed: %v", err)
	}

	manager := upstream.NewManager(database, logger.New(""), "proxy")
	store := idmap.NewStore(database)
	cfg := config.Default()
	cfg.Server.ID = "fusionride"
	cfg.Admin.Username = "proxy-admin"
	cfg.Admin.Password = "proxy-secret"
	setupProxyAdmin(t, database, cfg.Admin.Username, cfg.Admin.Password)

	handler := NewHandler(cfg, database, manager, nil, store, logger.New(""), traffic.NewMeter(database))
	upstreamInstance := manager.ByID(1)
	if upstreamInstance == nil {
		t.Fatal("expected upstream instance")
	}
	upstreamInstance.Mu.Lock()
	upstreamInstance.HealthStatus = "online"
	upstreamInstance.Session = &auth.UpstreamSession{Token: "token-a", UserID: "upstream-user"}
	upstreamInstance.Mu.Unlock()

	req := httptest.NewRequest(http.MethodPost, "/emby/Users/AuthenticateByName", strings.NewReader(`{"Username":"proxy-admin","Password":"proxy-secret"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected login to accept Password field, got %d with body %s", rec.Code, rec.Body.String())
	}
}

func TestHandleLoginReturnsUnauthorizedWhenCredentialsMismatch(t *testing.T) {

	database := openProxyTestDB(t)
	manager := upstream.NewManager(database, logger.New(""), "proxy")
	store := idmap.NewStore(database)
	cfg := config.Default()
	cfg.Admin.Username = "proxy-admin"
	cfg.Admin.Password = "proxy-secret"
	setupProxyAdmin(t, database, cfg.Admin.Username, cfg.Admin.Password)
	handler := NewHandler(cfg, database, manager, nil, store, logger.New(""), traffic.NewMeter(database))

	req := httptest.NewRequest(http.MethodPost, "/emby/Users/AuthenticateByName", strings.NewReader(`{"Username":"proxy-admin","Pw":"wrong"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 when proxy credentials mismatch, got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "用户名或密码错误") {
		t.Fatalf("expected Chinese credential failure, got %q", body)
	}
	if shouldCheckLegacyMessage := false; shouldCheckLegacyMessage {
		if body := rec.Body.String(); !strings.Contains(body, "所有上游均登录失败") {
			t.Fatalf("expected Chinese aggregate login failure, got %q", body)
		}
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
		ProxyUserID:    "proxy-user",
		lastAccess:     oldTime,
	}
	handler.sessions["fresh-token"] = loginSession{
		UpstreamID:     1,
		UpstreamUserID: "user-fresh",
		ProxyUserID:    "proxy-user",
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
	handler := NewHandler(cfg, database, manager, nil, store, logger.New(""), traffic.NewMeter(database))

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
	handler := NewHandler(config.Default(), database, manager, nil, store, logger.New(""), traffic.NewMeter(database))

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
	handler := NewHandler(config.Default(), database, manager, nil, store, logger.New(""), traffic.NewMeter(database))

	virtualID := store.GetOrCreate("item-original", 2, "Movie")
	handler.rememberSession("client-token", loginSession{
		UpstreamID:     2,
		UpstreamUserID: "user-2",
		ProxyUserID:    "proxy-user",
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
	handler := NewHandler(config.Default(), database, manager, nil, store, logger.New(""), traffic.NewMeter(database))

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

func TestHandleCurrentUserReturnsProxyIdentityWithoutForwarding(t *testing.T) {
	var requests int
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"Id":       "upstream-user",
			"Name":     "demo",
			"ServerId": "upstream-server",
		})
	}))
	defer upstreamServer.Close()

	database := openProxyTestDB(t)
	_, err := database.Exec(`
		INSERT INTO upstreams(id, name, url, playback_mode, spoof_mode, enabled, health_status, session_token)
		VALUES(?, ?, ?, '', 'web', 1, 'online', 'upstream-session-token')
	`, 2, "emby-b", upstreamServer.URL)
	if err != nil {
		t.Fatalf("insert upstream failed: %v", err)
	}

	manager := upstream.NewManager(database, logger.New(""), "proxy")
	store := idmap.NewStore(database)
	cfg := config.Default()
	cfg.Admin.Username = "proxy-admin"
	cfg.Admin.Password = "proxy-secret"
	setupProxyAdmin(t, database, cfg.Admin.Username, cfg.Admin.Password)
	handler := NewHandler(cfg, database, manager, nil, store, logger.New(""), traffic.NewMeter(database))
	handler.rememberSession("proxy-client-token", loginSession{
		UpstreamID:     2,
		UpstreamUserID: "upstream-user",
		ProxyUserID:    handler.proxyUserID,
		lastAccess:     time.Now(),
	})

	req := httptest.NewRequest(http.MethodGet, "/Users/Me", nil)
	req.Header.Set("X-Emby-Token", "proxy-client-token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected current user request to succeed, got %d with body %s", rec.Code, rec.Body.String())
	}
	if requests != 0 {
		t.Fatalf("expected current user request not to hit upstream, got %d requests", requests)
	}

	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal current user response failed: %v", err)
	}
	if payload["Name"] != "proxy-admin" {
		t.Fatalf("expected proxy admin name, got %#v", payload["Name"])
	}
	policy, ok := payload["Policy"].(map[string]any)
	if !ok {
		t.Fatalf("expected proxy policy payload, got %#v", payload["Policy"])
	}
	if policy["IsAdministrator"] != true {
		t.Fatalf("expected proxy admin policy, got %#v", policy["IsAdministrator"])
	}
}

func TestHandleLoginAcceptsFormBodyAndReturnsSessionInfo(t *testing.T) {
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/Users/upstream-user" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"Id":       "upstream-user",
			"Name":     "demo",
			"ServerId": "upstream-server",
		})
	}))
	defer upstreamServer.Close()

	database := openProxyTestDB(t)
	_, err := database.Exec(`
		INSERT INTO upstreams(id, name, url, playback_mode, spoof_mode, enabled, health_status, session_token)
		VALUES(?, ?, ?, '', 'web', 1, 'online', 'token-a')
	`, 1, "emby-a", upstreamServer.URL)
	if err != nil {
		t.Fatalf("insert upstream failed: %v", err)
	}

	manager := upstream.NewManager(database, logger.New(""), "proxy")
	store := idmap.NewStore(database)
	cfg := config.Default()
	cfg.Server.ID = "fusionride"
	cfg.Admin.Username = "proxy-admin"
	cfg.Admin.Password = "proxy-secret"
	setupProxyAdmin(t, database, cfg.Admin.Username, cfg.Admin.Password)

	handler := NewHandler(cfg, database, manager, nil, store, logger.New(""), traffic.NewMeter(database))
	upstreamInstance := manager.ByID(1)
	if upstreamInstance == nil {
		t.Fatal("expected upstream instance")
	}
	upstreamInstance.Mu.Lock()
	upstreamInstance.HealthStatus = "online"
	upstreamInstance.Session = &auth.UpstreamSession{Token: "token-a", UserID: "upstream-user"}
	upstreamInstance.Mu.Unlock()

	req := httptest.NewRequest(http.MethodPost, "/emby/Users/AuthenticateByName", strings.NewReader("Username=PROXY-ADMIN&Pw=proxy-secret"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Emby-Client", "Hills")
	req.Header.Set("X-Emby-Device-Name", "Windows PC")
	req.Header.Set("X-Emby-Device-Id", "pc-001")
	req.Header.Set("X-Emby-Client-Version", "4.9.0")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected form login to succeed, got %d with body %s", rec.Code, rec.Body.String())
	}

	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal login response failed: %v", err)
	}
	user := payload["User"].(map[string]any)
	if user["Id"] != handler.proxyUserID {
		t.Fatalf("expected proxy user id, got %#v", user["Id"])
	}
	if user["LastLoginDate"] == "" || user["LastActivityDate"] == "" {
		t.Fatalf("expected login timestamps in user payload, got %#v", user)
	}
	sessionInfo, ok := payload["SessionInfo"].(map[string]any)
	if !ok {
		t.Fatalf("expected SessionInfo in login response, got %#v", payload["SessionInfo"])
	}
	if sessionInfo["UserId"] != handler.proxyUserID {
		t.Fatalf("expected SessionInfo user id %q, got %#v", handler.proxyUserID, sessionInfo["UserId"])
	}
	if sessionInfo["Client"] != "Hills" || sessionInfo["DeviceName"] != "Windows PC" {
		t.Fatalf("expected device metadata in SessionInfo, got %#v", sessionInfo)
	}
}

func TestHandleLoginExtractsCredentialsFromAuthorizationHeader(t *testing.T) {
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/Users/upstream-user" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"Id":       "upstream-user",
			"Name":     "demo",
			"ServerId": "upstream-server",
		})
	}))
	defer upstreamServer.Close()

	database := openProxyTestDB(t)
	_, err := database.Exec(`
		INSERT INTO upstreams(id, name, url, playback_mode, spoof_mode, enabled, health_status, session_token)
		VALUES(?, ?, ?, '', 'web', 1, 'online', 'token-a')
	`, 1, "emby-a", upstreamServer.URL)
	if err != nil {
		t.Fatalf("insert upstream failed: %v", err)
	}

	manager := upstream.NewManager(database, logger.New(""), "proxy")
	store := idmap.NewStore(database)
	cfg := config.Default()
	cfg.Admin.Username = "proxy-admin"
	cfg.Admin.Password = "proxy-secret"
	setupProxyAdmin(t, database, cfg.Admin.Username, cfg.Admin.Password)

	handler := NewHandler(cfg, database, manager, nil, store, logger.New(""), traffic.NewMeter(database))
	upstreamInstance := manager.ByID(1)
	if upstreamInstance == nil {
		t.Fatal("expected upstream instance")
	}
	upstreamInstance.Mu.Lock()
	upstreamInstance.HealthStatus = "online"
	upstreamInstance.Session = &auth.UpstreamSession{Token: "token-a", UserID: "upstream-user"}
	upstreamInstance.Mu.Unlock()

	req := httptest.NewRequest(http.MethodPost, "/Users/AuthenticateByName", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Emby-Authorization", `MediaBrowser Username="proxy-admin", Password="proxy-secret", Client="Legacy", Device="Old TV", DeviceId="old-device", Version="1.0.0"`)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected header-based login to succeed, got %d with body %s", rec.Code, rec.Body.String())
	}
}

func TestHandlePlaybackInfoRewritesMediaSourceURLsToProxy(t *testing.T) {
	upstreamBase := "https://upstream.example"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"MediaSources": []map[string]any{{
				"Id":                   "media-1",
				"ItemId":               "item-original",
				"DirectStreamUrl":      upstreamBase + "/Videos/item-original/stream?Static=true",
				"TranscodingUrl":       upstreamBase + "/Videos/item-original/master.m3u8?MediaSourceId=media-1",
				"Path":                 "/srv/media/item-original.mkv",
				"SupportsDirectPlay":   true,
				"SupportsDirectStream": true,
				"SupportsTranscoding":  true,
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
	handler := NewHandler(cfg, database, manager, nil, store, logger.New(""), traffic.NewMeter(database))
	upstreamInstance := manager.ByID(2)
	if upstreamInstance == nil {
		t.Fatal("expected upstream instance")
	}
	upstreamInstance.Mu.Lock()
	upstreamInstance.HealthStatus = "online"
	upstreamInstance.Session = &auth.UpstreamSession{Token: "token", UserID: "upstream-user"}
	upstreamInstance.Mu.Unlock()

	virtualID := store.GetOrCreate("item-original", 2, "Movie")
	req := httptest.NewRequest(http.MethodPost, "/emby/Items/"+virtualID+"/PlaybackInfo?UserId="+handler.proxyUserID, strings.NewReader(`{"ItemId":"`+virtualID+`"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected playback info 200, got %d with body %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), upstreamBase) {
		t.Fatalf("expected playback info response not to leak upstream URL, got %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "/srv/media/") {
		t.Fatalf("expected playback info response not to leak upstream path, got %s", rec.Body.String())
	}

	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal playback info failed: %v", err)
	}
	mediaSources := payload["MediaSources"].([]any)
	mediaSource := mediaSources[0].(map[string]any)
	if mediaSource["SupportsDirectPlay"] != false {
		t.Fatalf("expected SupportsDirectPlay=false, got %#v", mediaSource["SupportsDirectPlay"])
	}
	if _, exists := mediaSource["DirectStreamUrl"]; exists {
		t.Fatalf("expected DirectStreamUrl to be removed, got %#v", mediaSource["DirectStreamUrl"])
	}
	transcodingURL, _ := mediaSource["TranscodingUrl"].(string)
	if !strings.HasPrefix(transcodingURL, "/") {
		t.Fatalf("expected relative transcoding url, got %q", transcodingURL)
	}
	if mediaSource["ItemId"] != virtualID {
		t.Fatalf("expected item id to remain virtualized, got %#v", mediaSource["ItemId"])
	}
}

func TestHandleStreamDirectModeRedirectsToStreamingURL(t *testing.T) {
	database := openProxyTestDB(t)
	_, err := database.Exec(`
		INSERT INTO upstreams(id, name, url, playback_mode, streaming_url, spoof_mode, enabled, health_status, session_token)
		VALUES(?, ?, ?, 'direct', ?, 'web', 1, 'online', 'token')
	`, 2, "emby-b", "https://api.example.com", "https://stream.example.com")
	if err != nil {
		t.Fatalf("insert upstream failed: %v", err)
	}

	manager := upstream.NewManager(database, logger.New(""), "proxy")
	store := idmap.NewStore(database)
	handler := NewHandler(config.Default(), database, manager, nil, store, logger.New(""), traffic.NewMeter(database))

	upstreamInstance := manager.ByID(2)
	if upstreamInstance == nil {
		t.Fatal("expected upstream instance")
	}
	upstreamInstance.Mu.Lock()
	upstreamInstance.HealthStatus = "online"
	upstreamInstance.Session = &auth.UpstreamSession{Token: "token", UserID: "upstream-user"}
	upstreamInstance.Mu.Unlock()

	virtualID := store.GetOrCreate("item-original", 2, "Movie")
	virtualMediaSourceID := store.GetOrCreate("media-original", 2, "MediaSource")
	req := httptest.NewRequest(http.MethodGet, "/emby/Videos/"+virtualID+"/stream?Static=true&MediaSourceId="+virtualMediaSourceID, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("expected direct playback mode to redirect, got %d with body %s", rec.Code, rec.Body.String())
	}
	location := rec.Header().Get("Location")
	if !strings.HasPrefix(location, "https://stream.example.com/Videos/item-original/stream?") {
		t.Fatalf("expected redirect to streaming_url, got %q", location)
	}
	if !strings.Contains(location, "api_key=token") {
		t.Fatalf("expected redirect url to include upstream session token, got %q", location)
	}
	if !strings.Contains(location, "MediaSourceId=media-original") {
		t.Fatalf("expected redirect url to devirtualize MediaSourceId, got %q", location)
	}
	if strings.Contains(location, virtualMediaSourceID) {
		t.Fatalf("expected redirect url not to leak virtual MediaSourceId %q, got %q", virtualMediaSourceID, location)
	}
}
