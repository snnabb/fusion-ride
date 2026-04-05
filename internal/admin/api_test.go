package admin

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/snnabb/fusion-ride/internal/config"
	"github.com/snnabb/fusion-ride/internal/db"
	"github.com/snnabb/fusion-ride/internal/idmap"
	"github.com/snnabb/fusion-ride/internal/logger"
	"github.com/snnabb/fusion-ride/internal/traffic"
	"github.com/snnabb/fusion-ride/internal/upstream"
)

func openAdminTestDB(t *testing.T) *db.DB {
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

func TestHandleUpstreamByIDAcceptsCamelCaseUpdatePayload(t *testing.T) {
	database := openAdminTestDB(t)
	result, err := database.Exec(`
		INSERT INTO upstreams(name, url, username, password, api_key, playback_mode, spoof_mode, enabled, health_status, session_token)
		VALUES(?, ?, ?, ?, ?, ?, ?, 1, 'online', 'session-token')
	`, "emby-a", "https://old.example", "demo", "old-pass", "", "proxy", "infuse")
	if err != nil {
		t.Fatalf("insert upstream failed: %v", err)
	}
	id64, err := result.LastInsertId()
	if err != nil {
		t.Fatalf("read inserted id failed: %v", err)
	}
	id := int(id64)

	manager := upstream.NewManager(database, logger.New(""), "proxy")
	api := &API{
		cfg:       config.Default(),
		cfgPath:   filepath.Join(t.TempDir(), "config.yaml"),
		upMgr:     manager,
		ids:       idmap.NewStore(database),
		log:       logger.New(""),
		meter:     traffic.NewMeter(database),
		sseHub:    NewSSEHub(),
		startTime: time.Now(),
	}

	req := httptest.NewRequest(http.MethodPut, "/admin/api/upstreams/"+strconv.Itoa(id), strings.NewReader(`{
		"name":"影院主库",
		"url":"https://edited.example/",
		"username":"alice",
		"password":"new-pass",
		"apiKey":"new-api-key",
		"playbackMode":"redirect",
		"spoofMode":"client",
		"enabled":false
	}`))
	rec := httptest.NewRecorder()

	api.handleUpstreamByID(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d with body %s", rec.Code, rec.Body.String())
	}

	u := manager.ByID(id)
	if u == nil {
		t.Fatal("expected updated upstream to exist")
	}
	if u.Name != "影院主库" {
		t.Fatalf("expected updated name, got %q", u.Name)
	}
	if u.URL != "https://edited.example" {
		t.Fatalf("expected trimmed URL, got %q", u.URL)
	}
	if u.Username != "alice" || u.Password != "new-pass" || u.APIKey != "new-api-key" {
		t.Fatalf("expected credentials to be updated, got username=%q password=%q apiKey=%q", u.Username, u.Password, u.APIKey)
	}
	if u.PlaybackMode != "redirect" {
		t.Fatalf("expected playback mode redirect, got %q", u.PlaybackMode)
	}
	if u.SpoofMode != "client" {
		t.Fatalf("expected spoof mode client, got %q", u.SpoofMode)
	}
	if u.Spoofer == nil || u.Spoofer.Mode() != "client" {
		t.Fatalf("expected spoofer to be rebuilt as client mode, got %#v", u.Spoofer)
	}
	if u.Enabled {
		t.Fatal("expected upstream to be disabled after update")
	}
}
