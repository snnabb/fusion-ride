package db

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

const DefaultFileName = "fusionride.db"

type DB struct {
	*sql.DB
}

func New(dataDir string) (*DB, error) {
	return Open(filepath.Join(dataDir, DefaultFileName))
}

func Open(path string) (*DB, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("创建数据目录失败: %w", err)
	}

	dsn := fmt.Sprintf("file:%s?_journal_mode=WAL&_busy_timeout=5000&_synchronous=NORMAL&_cache_size=-20000", path)
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}

	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(2)

	database := &DB{DB: sqlDB}
	if err := database.migrate(); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("数据库迁移失败: %w", err)
	}

	return database, nil
}

func (d *DB) migrate() error {
	migrations := []string{
		`CREATE TABLE IF NOT EXISTS meta (
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,

		`CREATE TABLE IF NOT EXISTS admin (
			id            INTEGER PRIMARY KEY CHECK(id = 1),
			username      TEXT NOT NULL DEFAULT 'admin',
			password_hash TEXT NOT NULL DEFAULT ''
		)`,

		`CREATE TABLE IF NOT EXISTS upstreams (
			id               INTEGER PRIMARY KEY AUTOINCREMENT,
			name             TEXT NOT NULL,
			url              TEXT NOT NULL,
			username         TEXT DEFAULT '',
			password         TEXT DEFAULT '',
			api_key          TEXT DEFAULT '',
			playback_mode    TEXT DEFAULT 'proxy',
			streaming_url    TEXT DEFAULT '',
			spoof_mode       TEXT DEFAULT 'infuse',
			custom_ua        TEXT DEFAULT '',
			custom_client    TEXT DEFAULT '',
			custom_version   TEXT DEFAULT '',
			custom_device    TEXT DEFAULT '',
			custom_device_id TEXT DEFAULT '',
			proxy_id         TEXT DEFAULT '',
			priority         INTEGER DEFAULT 0,
			priority_meta    BOOLEAN DEFAULT 0,
			follow_redirects BOOLEAN DEFAULT 1,
			enabled          BOOLEAN DEFAULT 1,
			health_status    TEXT DEFAULT 'unknown',
			session_token    TEXT DEFAULT '',
			last_check       INTEGER DEFAULT 0,
			created_at       INTEGER NOT NULL DEFAULT (unixepoch()),
			updated_at       INTEGER NOT NULL DEFAULT (unixepoch())
		)`,

		`CREATE TABLE IF NOT EXISTS id_mapping (
			virtual_id   TEXT PRIMARY KEY,
			original_id  TEXT NOT NULL,
			server_id    INTEGER NOT NULL,
			item_type    TEXT DEFAULT '',
			created_at   INTEGER NOT NULL DEFAULT (unixepoch()),
			UNIQUE(original_id, server_id)
		)`,

		`CREATE TABLE IF NOT EXISTS id_instances (
			virtual_id   TEXT NOT NULL,
			original_id  TEXT NOT NULL,
			server_id    INTEGER NOT NULL,
			bitrate      INTEGER DEFAULT 0,
			PRIMARY KEY(virtual_id, server_id),
			FOREIGN KEY(virtual_id) REFERENCES id_mapping(virtual_id) ON DELETE CASCADE
		)`,

		`CREATE TABLE IF NOT EXISTS proxies (
			id      TEXT PRIMARY KEY,
			name    TEXT NOT NULL,
			url     TEXT NOT NULL,
			enabled BOOLEAN DEFAULT 1
		)`,

		`CREATE TABLE IF NOT EXISTS traffic_stats (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			upstream_id INTEGER NOT NULL,
			bytes_in    INTEGER DEFAULT 0,
			bytes_out   INTEGER DEFAULT 0,
			timestamp   INTEGER NOT NULL DEFAULT (unixepoch()),
			FOREIGN KEY(upstream_id) REFERENCES upstreams(id) ON DELETE CASCADE
		)`,

		`CREATE TABLE IF NOT EXISTS client_identities (
			server_id      INTEGER PRIMARY KEY,
			user_agent     TEXT DEFAULT '',
			emby_client    TEXT DEFAULT '',
			emby_device    TEXT DEFAULT '',
			emby_device_id TEXT DEFAULT '',
			emby_version   TEXT DEFAULT '',
			updated_at     INTEGER NOT NULL DEFAULT (unixepoch()),
			FOREIGN KEY(server_id) REFERENCES upstreams(id) ON DELETE CASCADE
		)`,

		`CREATE INDEX IF NOT EXISTS idx_id_mapping_original ON id_mapping(original_id, server_id)`,
		`CREATE INDEX IF NOT EXISTS idx_id_instances_virtual ON id_instances(virtual_id)`,
		`CREATE INDEX IF NOT EXISTS idx_traffic_upstream ON traffic_stats(upstream_id, timestamp)`,
	}

	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, migration := range migrations {
		if _, err := tx.Exec(migration); err != nil {
			preview := migration
			if len(preview) > 60 {
				preview = preview[:60]
			}
			return fmt.Errorf("执行迁移失败: %s: %w", preview, err)
		}
	}

	if _, err := tx.Exec(`INSERT OR REPLACE INTO meta(key, value) VALUES('schema_version', '1')`); err != nil {
		return err
	}

	return tx.Commit()
}
