package store

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// DB wraps the sql.DB connection and provides migration support.
type DB struct {
	*sql.DB
}

// Open creates or opens a SQLite database at the given path and runs migrations.
func Open(path string) (*DB, error) {
	// Ensure parent directory exists
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("creating database directory: %w", err)
	}

	sqlDB, err := sql.Open("sqlite", path+"?_pragma=journal_mode(wal)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("opening database: %w", err)
	}

	// Verify connectivity
	if err := sqlDB.Ping(); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("pinging database: %w", err)
	}

	db := &DB{DB: sqlDB}
	if err := db.migrate(); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("running migrations: %w", err)
	}

	return db, nil
}

// Ping checks if the database is reachable.
func (db *DB) Ping() error {
	return db.DB.Ping()
}

// migrate runs schema migrations using a version table.
func (db *DB) migrate() error {
	// Create schema_version table if it doesn't exist
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_version (
		version INTEGER PRIMARY KEY
	)`); err != nil {
		return fmt.Errorf("creating schema_version table: %w", err)
	}

	var currentVersion int
	row := db.QueryRow("SELECT COALESCE(MAX(version), 0) FROM schema_version")
	if err := row.Scan(&currentVersion); err != nil {
		return fmt.Errorf("reading schema version: %w", err)
	}

	migrations := []struct {
		version int
		sql     string
	}{
		{1, `
			CREATE TABLE audit_log (
				id          INTEGER PRIMARY KEY AUTOINCREMENT,
				timestamp   TEXT NOT NULL DEFAULT (datetime('now')),
				event_type  TEXT NOT NULL,
				user_email  TEXT,
				server_name TEXT,
				session_id  TEXT,
				remote_addr TEXT,
				details     TEXT
			);
			CREATE INDEX idx_audit_timestamp ON audit_log(timestamp);
			CREATE INDEX idx_audit_server ON audit_log(server_name);
		`},
		{2, `
			CREATE TABLE kvm_sessions (
				id             TEXT PRIMARY KEY,
				server_name    TEXT NOT NULL,
				bmc_ip         TEXT NOT NULL,
				status         TEXT NOT NULL DEFAULT 'starting',
				container_id   TEXT,
				websocket_port INTEGER,
				conn_mode      TEXT,
				kvm_target     TEXT,
				kvm_password   TEXT,
				created_at     TEXT NOT NULL,
				last_activity  TEXT NOT NULL,
				error          TEXT
			);
			CREATE INDEX idx_sessions_status ON kvm_sessions(status);
		`},
		{3, `
			CREATE TABLE IF NOT EXISTS iso_files (
				id          INTEGER PRIMARY KEY AUTOINCREMENT,
				filename    TEXT NOT NULL UNIQUE,
				size_bytes  INTEGER NOT NULL,
				sha256      TEXT,
				uploaded_by TEXT,
				uploaded_at TEXT NOT NULL DEFAULT (datetime('now')),
				last_used   TEXT
			);
		`},
	}

	for _, m := range migrations {
		if m.version <= currentVersion {
			continue
		}
		log.Printf("Running database migration v%d", m.version)
		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("beginning migration v%d: %w", m.version, err)
		}
		if _, err := tx.Exec(m.sql); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration v%d: %w", m.version, err)
		}
		if _, err := tx.Exec("INSERT INTO schema_version (version) VALUES (?)", m.version); err != nil {
			tx.Rollback()
			return fmt.Errorf("recording migration v%d: %w", m.version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("committing migration v%d: %w", m.version, err)
		}
	}

	return nil
}
