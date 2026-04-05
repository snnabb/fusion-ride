package aggregator

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/snnabb/fusion-ride/internal/auth"
	"github.com/snnabb/fusion-ride/internal/db"
	"github.com/snnabb/fusion-ride/internal/idmap"
	"github.com/snnabb/fusion-ride/internal/logger"
	"github.com/snnabb/fusion-ride/internal/upstream"
)

func openAggregatorTestDB(t *testing.T) *db.DB {
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

func TestDedupMergesItemsAndSortsMediaSources(t *testing.T) {
	store := idmap.NewStore(openAggregatorTestDB(t))
	agg := &Aggregator{
		upMgr:   &upstream.Manager{},
		idStore: store,
		log:     logger.New(""),
		codecPriority: map[string]int{
			"hevc": 3,
			"av1":  2,
			"h264": 1,
		},
	}

	items := []EmbyItem{
		{
			ID:       "movie-a",
			Name:     "电影 A",
			Type:     "Movie",
			ServerID: 1,
			ProviderIDs: map[string]string{
				"Tmdb": "100",
			},
			MediaSources: []MediaSource{{
				ID:            "ms-1",
				Bitrate:       1000,
				Width:         1920,
				Height:        1080,
				VideoCodec:    "h264",
				AudioChannels: 2,
				Size:          100,
				ServerID:      1,
				OriginalID:    "movie-a",
			}},
		},
		{
			ID:       "movie-b",
			Name:     "Movie A",
			Type:     "Movie",
			ServerID: 2,
			ProviderIDs: map[string]string{
				"Tmdb": "100",
			},
			MediaSources: []MediaSource{{
				ID:            "ms-2",
				Bitrate:       2000,
				Width:         3840,
				Height:        2160,
				VideoCodec:    "hevc",
				AudioChannels: 6,
				Size:          200,
				ServerID:      2,
				OriginalID:    "movie-b",
			}},
		},
	}

	merged := agg.dedup(items)
	if len(merged) != 1 {
		t.Fatalf("expected dedup to merge items into 1 entry, got %d", len(merged))
	}
	if len(merged[0].MediaSources) != 2 {
		t.Fatalf("expected merged item to contain 2 media sources, got %d", len(merged[0].MediaSources))
	}
	if merged[0].MediaSources[0].Bitrate != 2000 {
		t.Fatalf("expected highest bitrate source first, got %d", merged[0].MediaSources[0].Bitrate)
	}
}

func TestAggregateItemsRespectsCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(250 * time.Millisecond):
			_ = json.NewEncoder(w).Encode(EmbyItemsResponse{})
		}
	}))
	defer server.Close()

	database := openAggregatorTestDB(t)
	_, err := database.Exec(`
		INSERT INTO upstreams(name, url, playback_mode, spoof_mode, enabled, health_status, session_token)
		VALUES(?, ?, 'proxy', 'infuse', 1, 'online', 'token')
	`, "emby-a", server.URL)
	if err != nil {
		t.Fatalf("insert upstream failed: %v", err)
	}

	manager := upstream.NewManager(database, logger.New(""), "proxy")
	agg := New(manager, idmap.NewStore(database), logger.New(""), time.Second, []string{"hevc", "av1", "h264"})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err = agg.AggregateItems(ctx, "/Items")
	if err == nil {
		t.Fatal("expected AggregateItems to return cancellation error")
	}
	if time.Since(start) > 200*time.Millisecond {
		t.Fatalf("expected AggregateItems to stop quickly after cancellation, took %s", time.Since(start))
	}
}

func TestAggregateSingleItemUsesSessionUserID(t *testing.T) {
	var capturedPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.RequestURI()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"Id": "item-1",
		})
	}))
	defer server.Close()

	database := openAggregatorTestDB(t)
	_, err := database.Exec(`
		INSERT INTO upstreams(id, name, url, playback_mode, spoof_mode, enabled, health_status, session_token)
		VALUES(?, ?, ?, 'proxy', 'infuse', 1, 'online', 'token')
	`, 1, "emby-a", server.URL)
	if err != nil {
		t.Fatalf("insert upstream failed: %v", err)
	}

	manager := upstream.NewManager(database, logger.New(""), "proxy")
	selected := manager.ByID(1)
	selected.Mu.Lock()
	selected.Session = &auth.UpstreamSession{
		ServerID: 1,
		Token:    "token",
		UserID:   "user-123",
	}
	selected.Mu.Unlock()

	store := idmap.NewStore(database)
	virtualID := store.GetOrCreate("item-1", 1, "Movie")
	agg := New(manager, store, logger.New(""), time.Second, nil)

	if _, err := agg.AggregateSingleItem(context.Background(), virtualID); err != nil {
		t.Fatalf("AggregateSingleItem failed: %v", err)
	}

	if capturedPath != "/Users/user-123/Items/item-1" {
		t.Fatalf("expected AggregateSingleItem to use session user ID path, got %q", capturedPath)
	}
}

func TestAggregateSingleItemFallsBackWhenSessionUserIDMissing(t *testing.T) {
	var capturedPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.RequestURI()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"Id": "item-1",
		})
	}))
	defer server.Close()

	database := openAggregatorTestDB(t)
	_, err := database.Exec(`
		INSERT INTO upstreams(id, name, url, playback_mode, spoof_mode, enabled, health_status, session_token)
		VALUES(?, ?, ?, 'proxy', 'infuse', 1, 'online', 'token')
	`, 1, "emby-a", server.URL)
	if err != nil {
		t.Fatalf("insert upstream failed: %v", err)
	}

	manager := upstream.NewManager(database, logger.New(""), "proxy")
	selected := manager.ByID(1)
	selected.Mu.Lock()
	selected.Session = &auth.UpstreamSession{
		ServerID: 1,
		Token:    "token",
	}
	selected.Mu.Unlock()

	store := idmap.NewStore(database)
	virtualID := store.GetOrCreate("item-1", 1, "Movie")
	agg := New(manager, store, logger.New(""), time.Second, nil)

	if _, err := agg.AggregateSingleItem(context.Background(), virtualID); err != nil {
		t.Fatalf("AggregateSingleItem failed: %v", err)
	}

	if capturedPath != "/Items/item-1?Fields=MediaSources,ProviderIds" {
		t.Fatalf("expected AggregateSingleItem fallback path, got %q", capturedPath)
	}
}

func TestVirtualizeItemUsesStructuredJSONWalk(t *testing.T) {
	store := idmap.NewStore(openAggregatorTestDB(t))
	agg := &Aggregator{
		idStore: store,
		log:     logger.New(""),
	}

	expectedID := store.GetOrCreate("item-1", 1, "Movie")
	expectedSeriesID := store.GetOrCreate("series-1", 1, "")
	expectedParentID := store.GetOrCreate("parent-1", 1, "")

	item := EmbyItem{
		ID:       "item-1",
		Type:     "Movie",
		ServerID: 1,
		Raw: json.RawMessage(`{
			"Id":"item-1",
			"SeriesId":"series-1",
			"ParentId":"parent-1",
			"Name":"Movie item-1",
			"Overview":"literal item-1 should stay"
		}`),
	}

	virtualized := agg.virtualizeItem(item)
	var parsed map[string]any
	if err := json.Unmarshal(virtualized, &parsed); err != nil {
		t.Fatalf("unmarshal virtualized item failed: %v", err)
	}

	if got := parsed["Id"]; got != expectedID {
		t.Fatalf("expected Id %q, got %v", expectedID, got)
	}
	if got := parsed["SeriesId"]; got != expectedSeriesID {
		t.Fatalf("expected SeriesId %q, got %v", expectedSeriesID, got)
	}
	if got := parsed["ParentId"]; got != expectedParentID {
		t.Fatalf("expected ParentId %q, got %v", expectedParentID, got)
	}
	if got := parsed["Overview"]; got != "literal item-1 should stay" {
		t.Fatalf("expected Overview string to stay unchanged, got %v", got)
	}
}
