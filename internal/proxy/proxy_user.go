package proxy

import (
	"crypto/rand"
	"fmt"

	"github.com/snnabb/fusion-ride/internal/db"
)

func loadOrCreateProxyUserID(database *db.DB) string {
	if database == nil {
		return generateProxyUserID()
	}

	var existing string
	err := database.QueryRow(`SELECT value FROM meta WHERE key = 'proxy_user_id'`).Scan(&existing)
	if err == nil && existing != "" {
		return existing
	}

	generated := generateProxyUserID()
	_, _ = database.Exec(`INSERT OR IGNORE INTO meta(key, value) VALUES('proxy_user_id', ?)`, generated)

	if err := database.QueryRow(`SELECT value FROM meta WHERE key = 'proxy_user_id'`).Scan(&existing); err == nil && existing != "" {
		return existing
	}

	return generated
}

func generateProxyUserID() string {
	return generateHexToken()
}

func generateSecureToken() string {
	return generateHexToken()
}

func generateHexToken() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "00000000000000000000000000000000"
	}

	return fmt.Sprintf("%x", buf)
}
