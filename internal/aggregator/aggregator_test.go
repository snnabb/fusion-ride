package aggregator

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

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
