package idmap

import (
	"path/filepath"
	"testing"

	"github.com/snnabb/fusion-ride/internal/db"
)

func openTestDB(t *testing.T) *db.DB {
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

func TestStoreCreatesAndResolvesMapping(t *testing.T) {
	store := NewStore(openTestDB(t))

	virtualID := store.GetOrCreate("item-1", 1, "Movie")
	if virtualID == "" {
		t.Fatal("expected virtual ID to be created")
	}

	originalID, serverID, ok := store.Resolve(virtualID)
	if !ok {
		t.Fatal("expected Resolve to find created mapping")
	}
	if originalID != "item-1" || serverID != 1 {
		t.Fatalf("unexpected mapping resolution result: original=%q server=%d", originalID, serverID)
	}
}

func TestStoreMaintainsInstances(t *testing.T) {
	store := NewStore(openTestDB(t))

	virtualID := store.GetOrCreate("item-1", 1, "Movie")
	if err := store.AddInstance(virtualID, "item-2", 2, 2000); err != nil {
		t.Fatalf("AddInstance failed: %v", err)
	}

	instances := store.GetInstances(virtualID)
	if len(instances) != 2 {
		t.Fatalf("expected 2 instances including primary, got %d", len(instances))
	}
	if instances[1].OriginalID != "item-2" || instances[1].ServerID != 2 {
		t.Fatalf("unexpected secondary instance: %+v", instances[1])
	}
}

func TestStoreCleanupRemovesServerMappings(t *testing.T) {
	store := NewStore(openTestDB(t))

	virtualID := store.GetOrCreate("item-1", 1, "Movie")
	_ = store.GetOrCreate("item-2", 2, "Movie")

	if err := store.CleanupServer(1); err != nil {
		t.Fatalf("CleanupServer failed: %v", err)
	}

	if _, _, ok := store.Resolve(virtualID); ok {
		t.Fatal("expected mapping for cleaned server to be removed")
	}
}
