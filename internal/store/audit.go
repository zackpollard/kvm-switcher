package store

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/zackpollard/kvm-switcher/internal/models"
)

// Verify DB implements AuditLogger at compile time.
var _ models.AuditLogger = (*DB)(nil)

// LogAudit inserts an audit log entry.
func (db *DB) LogAudit(entry models.AuditEntry) error {
	var detailsJSON *string
	if entry.Details != nil {
		b, err := json.Marshal(entry.Details)
		if err != nil {
			return fmt.Errorf("marshaling audit details: %w", err)
		}
		s := string(b)
		detailsJSON = &s
	}

	_, err := db.Exec(
		`INSERT INTO audit_log (event_type, user_email, server_name, session_id, remote_addr, details)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		entry.EventType,
		nilIfEmpty(entry.UserEmail),
		nilIfEmpty(entry.ServerName),
		nilIfEmpty(entry.SessionID),
		nilIfEmpty(entry.RemoteAddr),
		detailsJSON,
	)
	if err != nil {
		return fmt.Errorf("inserting audit entry: %w", err)
	}
	return nil
}

// QueryAudit retrieves audit entries matching the filter.
func (db *DB) QueryAudit(filter models.AuditFilter) ([]models.AuditEntry, error) {
	query := "SELECT id, timestamp, event_type, COALESCE(user_email,''), COALESCE(server_name,''), COALESCE(session_id,''), COALESCE(remote_addr,''), details FROM audit_log WHERE 1=1"
	var args []any

	if filter.EventType != "" {
		query += " AND event_type = ?"
		args = append(args, filter.EventType)
	}
	if filter.ServerName != "" {
		query += " AND server_name = ?"
		args = append(args, filter.ServerName)
	}
	if filter.UserEmail != "" {
		query += " AND user_email = ?"
		args = append(args, filter.UserEmail)
	}

	query += " ORDER BY id DESC"

	limit := filter.Limit
	if limit <= 0 {
		limit = 100
	}
	query += fmt.Sprintf(" LIMIT %d", limit)
	if filter.Offset > 0 {
		query += fmt.Sprintf(" OFFSET %d", filter.Offset)
	}

	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying audit log: %w", err)
	}
	defer rows.Close()

	var entries []models.AuditEntry
	for rows.Next() {
		var e models.AuditEntry
		var ts string
		var detailsJSON *string
		if err := rows.Scan(&e.ID, &ts, &e.EventType, &e.UserEmail, &e.ServerName, &e.SessionID, &e.RemoteAddr, &detailsJSON); err != nil {
			return nil, fmt.Errorf("scanning audit row: %w", err)
		}
		e.Timestamp, _ = time.Parse("2006-01-02 15:04:05", ts)
		if detailsJSON != nil {
			var details any
			json.Unmarshal([]byte(*detailsJSON), &details)
			e.Details = details
		}
		entries = append(entries, e)
	}

	if entries == nil {
		entries = []models.AuditEntry{}
	}
	return entries, rows.Err()
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
