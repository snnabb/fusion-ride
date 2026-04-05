package auth

import (
	"path/filepath"
	"testing"

	"github.com/snnabb/fusion-ride/internal/db"
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

func TestVerifyCredentials(t *testing.T) {
	database := openAdminTestDB(t)
	adminAuth := NewAdminAuth(database, "")

	if err := adminAuth.Setup("proxy-admin", "proxy-secret"); err != nil {
		t.Fatalf("setup admin failed: %v", err)
	}

	if !adminAuth.VerifyCredentials("proxy-admin", "proxy-secret") {
		t.Fatal("expected matching credentials to verify")
	}
	if adminAuth.VerifyCredentials("other-user", "proxy-secret") {
		t.Fatal("expected different username to fail verification")
	}
	if adminAuth.VerifyCredentials("proxy-admin", "wrong-password") {
		t.Fatal("expected wrong password to fail verification")
	}
}

func TestVerifyCredentialsIgnoresUsernameCase(t *testing.T) {
	database := openAdminTestDB(t)
	adminAuth := NewAdminAuth(database, "")

	if err := adminAuth.Setup("proxy-admin", "proxy-secret"); err != nil {
		t.Fatalf("setup admin failed: %v", err)
	}

	if !adminAuth.VerifyCredentials("Proxy-Admin", "proxy-secret") {
		t.Fatal("expected username comparison to be case-insensitive")
	}
}
