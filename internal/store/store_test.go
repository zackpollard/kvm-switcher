package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/zackpollard/kvm-switcher/internal/models"
)

// Type aliases for convenience in tests
type AuditEntry = models.AuditEntry
type AuditFilter = models.AuditFilter

func openTestDB(t *testing.T) *DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open(%q): %v", path, err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestOpen_CreatesAndMigrates(t *testing.T) {
	db := openTestDB(t)
	if err := db.Ping(); err != nil {
		t.Fatalf("Ping: %v", err)
	}

	// Verify tables exist
	var count int
	err := db.QueryRow("SELECT COUNT(*) FROM schema_version").Scan(&count)
	if err != nil {
		t.Fatalf("schema_version query: %v", err)
	}
	if count != 3 {
		t.Errorf("schema_version rows = %d, want 3", count)
	}
}

func TestOpen_IdempotentMigrations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")

	// First open
	db1, err := Open(path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	db1.Close()

	// Second open should not fail
	db2, err := Open(path)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	db2.Close()
}

func TestAuditLog(t *testing.T) {
	db := openTestDB(t)

	// Insert entries
	for i := 0; i < 3; i++ {
		err := db.LogAudit(AuditEntry{
			EventType:  "session_create",
			UserEmail:  "test@example.com",
			ServerName: "server-1",
			SessionID:  "sess-1",
			RemoteAddr: "10.0.0.1",
			Details:    map[string]string{"key": "value"},
		})
		if err != nil {
			t.Fatalf("LogAudit: %v", err)
		}
	}

	// Query all
	entries, err := db.QueryAudit(AuditFilter{})
	if err != nil {
		t.Fatalf("QueryAudit: %v", err)
	}
	if len(entries) != 3 {
		t.Errorf("entries = %d, want 3", len(entries))
	}

	// Query by event type
	entries, err = db.QueryAudit(AuditFilter{EventType: "session_create"})
	if err != nil {
		t.Fatalf("QueryAudit: %v", err)
	}
	if len(entries) != 3 {
		t.Errorf("filtered entries = %d, want 3", len(entries))
	}

	// Query non-existent type
	entries, err = db.QueryAudit(AuditFilter{EventType: "nonexistent"})
	if err != nil {
		t.Fatalf("QueryAudit: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("filtered entries = %d, want 0", len(entries))
	}
}

func TestAuditLog_Limit(t *testing.T) {
	db := openTestDB(t)

	for i := 0; i < 10; i++ {
		db.LogAudit(AuditEntry{EventType: "test"})
	}

	entries, err := db.QueryAudit(AuditFilter{Limit: 5})
	if err != nil {
		t.Fatalf("QueryAudit: %v", err)
	}
	if len(entries) != 5 {
		t.Errorf("entries = %d, want 5", len(entries))
	}
}

func TestSQLiteSessionStore(t *testing.T) {
	db := openTestDB(t)
	store, err := NewSQLiteSessionStore(db)
	if err != nil {
		t.Fatalf("NewSQLiteSessionStore: %v", err)
	}

	// Set and Get
	sess := &models.KVMSession{
		ID:           "test-1",
		ServerName:   "server-1",
		BMCIP:        "10.0.0.1",
		Status:       models.SessionStarting,
		CreatedAt:    time.Now(),
		LastActivity: time.Now(),
	}
	store.Set(sess)

	got, ok := store.Get("test-1")
	if !ok {
		t.Fatal("Get: session not found")
	}
	if got.ServerName != "server-1" {
		t.Errorf("ServerName = %q, want server-1", got.ServerName)
	}

	// List
	all := store.List()
	if len(all) != 1 {
		t.Errorf("List = %d, want 1", len(all))
	}

	// FindByServer
	found, ok := store.FindByServer("server-1")
	if !ok {
		t.Fatal("FindByServer: not found")
	}
	if found.ID != "test-1" {
		t.Errorf("FindByServer ID = %q, want test-1", found.ID)
	}

	// Update status
	sess.Status = models.SessionConnected
	store.Set(sess)
	got, _ = store.Get("test-1")
	if got.Status != models.SessionConnected {
		t.Errorf("Status = %q, want connected", got.Status)
	}

	// Delete
	store.Delete("test-1")
	_, ok = store.Get("test-1")
	if ok {
		t.Error("Get after Delete: should not find session")
	}
}

func TestSQLiteSessionStore_Persistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")

	// First open: create session
	db1, err := Open(path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	store1, err := NewSQLiteSessionStore(db1)
	if err != nil {
		t.Fatalf("NewSQLiteSessionStore: %v", err)
	}
	store1.Set(&models.KVMSession{
		ID:           "persist-1",
		ServerName:   "server-1",
		BMCIP:        "10.0.0.1",
		Status:       models.SessionConnected,
		CreatedAt:    time.Now(),
		LastActivity: time.Now(),
	})
	db1.Close()

	// Second open: session should be loaded (and marked disconnected since it was "connected")
	db2, err := Open(path)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer db2.Close()
	store2, err := NewSQLiteSessionStore(db2)
	if err != nil {
		t.Fatalf("NewSQLiteSessionStore: %v", err)
	}

	got, ok := store2.Get("persist-1")
	if !ok {
		t.Fatal("persisted session not found after reopen")
	}
	if got.Status != models.SessionDisconnected {
		t.Errorf("persisted session status = %q, want disconnected (stale recovery)", got.Status)
	}
}

func TestSQLiteSessionStore_FindByServerIgnoresTerminal(t *testing.T) {
	db := openTestDB(t)
	store, err := NewSQLiteSessionStore(db)
	if err != nil {
		t.Fatalf("NewSQLiteSessionStore: %v", err)
	}

	store.Set(&models.KVMSession{
		ID:           "done-1",
		ServerName:   "server-1",
		BMCIP:        "10.0.0.1",
		Status:       models.SessionDisconnected,
		CreatedAt:    time.Now(),
		LastActivity: time.Now(),
	})

	_, ok := store.FindByServer("server-1")
	if ok {
		t.Error("FindByServer should not return disconnected sessions")
	}
}
