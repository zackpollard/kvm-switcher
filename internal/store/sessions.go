package store

import (
	"log"
	"sync"
	"time"

	"github.com/zackpollard/kvm-switcher/internal/models"
)

const timeFormat = "2006-01-02T15:04:05Z07:00"

// SQLiteSessionStore implements models.SessionStoreInterface backed by SQLite.
// It keeps a fast in-memory cache synchronized with the database for read performance.
type SQLiteSessionStore struct {
	db    *DB
	mu    sync.RWMutex
	cache map[string]*models.KVMSession
}

// Verify interface compliance.
var _ models.SessionStoreInterface = (*SQLiteSessionStore)(nil)

// NewSQLiteSessionStore creates a session store backed by SQLite.
// It loads existing sessions from the DB and marks stale active sessions as disconnected.
func NewSQLiteSessionStore(db *DB) (*SQLiteSessionStore, error) {
	s := &SQLiteSessionStore{
		db:    db,
		cache: make(map[string]*models.KVMSession),
	}

	// Load existing sessions from DB
	rows, err := db.Query(`SELECT id, server_name, bmc_ip, status,
		COALESCE(conn_mode,''), COALESCE(kvm_target,''),
		COALESCE(kvm_password,''), created_at, last_activity, COALESCE(error,'')
		FROM kvm_sessions`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var sess models.KVMSession
		var status, connMode, createdAt, lastActivity string
		if err := rows.Scan(&sess.ID, &sess.ServerName, &sess.BMCIP, &status,
			&connMode,
			&sess.KVMTarget, &sess.KVMPassword, &createdAt, &lastActivity, &sess.Error); err != nil {
			return nil, err
		}
		sess.Status = models.SessionStatus(status)
		sess.ConnMode = models.KVMMode(connMode)
		sess.CreatedAt, _ = time.Parse(timeFormat, createdAt)
		sess.LastActivity, _ = time.Parse(timeFormat, lastActivity)
		s.cache[sess.ID] = &sess
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Mark stale "starting" or "connected" sessions as "disconnected"
	// (transient sessions cannot survive a restart).
	count := 0
	for _, sess := range s.cache {
		if sess.Status == models.SessionStarting || sess.Status == models.SessionConnected {
			sess.Status = models.SessionDisconnected
			s.persistSession(sess)
			count++
		}
	}
	if count > 0 {
		log.Printf("SQLiteSessionStore: marked %d stale sessions as disconnected", count)
	}

	return s, nil
}

func (s *SQLiteSessionStore) Get(id string) (*models.KVMSession, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sess, ok := s.cache[id]
	return sess, ok
}

func (s *SQLiteSessionStore) Set(session *models.KVMSession) {
	s.mu.Lock()
	s.cache[session.ID] = session
	s.mu.Unlock()

	s.persistSession(session)
}

func (s *SQLiteSessionStore) Delete(id string) {
	s.mu.Lock()
	delete(s.cache, id)
	s.mu.Unlock()

	if _, err := s.db.Exec("DELETE FROM kvm_sessions WHERE id = ?", id); err != nil {
		log.Printf("SQLiteSessionStore: delete error: %v", err)
	}
}

func (s *SQLiteSessionStore) List() []*models.KVMSession {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*models.KVMSession, 0, len(s.cache))
	for _, sess := range s.cache {
		result = append(result, sess)
	}
	return result
}

func (s *SQLiteSessionStore) FindByServer(serverName string) (*models.KVMSession, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, sess := range s.cache {
		if sess.ServerName == serverName && sess.Status != models.SessionDisconnected && sess.Status != models.SessionError {
			return sess, true
		}
	}
	return nil, false
}

func (s *SQLiteSessionStore) persistSession(sess *models.KVMSession) {
	_, err := s.db.Exec(
		`INSERT INTO kvm_sessions (id, server_name, bmc_ip, status, conn_mode, kvm_target, kvm_password, created_at, last_activity, error)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   status=excluded.status, conn_mode=excluded.conn_mode,
		   kvm_target=excluded.kvm_target, kvm_password=excluded.kvm_password,
		   last_activity=excluded.last_activity, error=excluded.error`,
		sess.ID, sess.ServerName, sess.BMCIP, string(sess.Status),
		string(sess.ConnMode),
		sess.KVMTarget, sess.KVMPassword,
		sess.CreatedAt.Format(timeFormat), sess.LastActivity.Format(timeFormat),
		sess.Error,
	)
	if err != nil {
		log.Printf("SQLiteSessionStore: persist error: %v", err)
	}
}
