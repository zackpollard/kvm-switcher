package store

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/zackpollard/kvm-switcher/internal/models"
)

// InsertISO records a new ISO file in the database.
func (db *DB) InsertISO(filename string, sizeBytes int64, sha256 string, uploadedBy string) error {
	_, err := db.Exec(
		`INSERT INTO iso_files (filename, size_bytes, sha256, uploaded_by) VALUES (?, ?, ?, ?)`,
		filename, sizeBytes, nilIfEmpty(sha256), nilIfEmpty(uploadedBy),
	)
	if err != nil {
		return fmt.Errorf("inserting ISO record: %w", err)
	}
	return nil
}

// ListISOs returns all ISO file records ordered by filename.
func (db *DB) ListISOs() ([]models.ISOFile, error) {
	rows, err := db.Query(
		`SELECT id, filename, size_bytes, COALESCE(sha256,''), COALESCE(uploaded_by,''), uploaded_at, last_used
		 FROM iso_files ORDER BY filename`,
	)
	if err != nil {
		return nil, fmt.Errorf("querying ISO files: %w", err)
	}
	defer rows.Close()

	var isos []models.ISOFile
	for rows.Next() {
		var f models.ISOFile
		var uploadedAt string
		var lastUsed *string
		if err := rows.Scan(&f.ID, &f.Filename, &f.SizeBytes, &f.SHA256, &f.UploadedBy, &uploadedAt, &lastUsed); err != nil {
			return nil, fmt.Errorf("scanning ISO row: %w", err)
		}
		f.UploadedAt, _ = time.Parse("2006-01-02 15:04:05", uploadedAt)
		if lastUsed != nil {
			t, _ := time.Parse("2006-01-02 15:04:05", *lastUsed)
			f.LastUsed = &t
		}
		isos = append(isos, f)
	}

	if isos == nil {
		isos = []models.ISOFile{}
	}
	return isos, rows.Err()
}

// GetISO returns a single ISO file record by filename.
func (db *DB) GetISO(filename string) (*models.ISOFile, error) {
	row := db.QueryRow(
		`SELECT id, filename, size_bytes, COALESCE(sha256,''), COALESCE(uploaded_by,''), uploaded_at, last_used
		 FROM iso_files WHERE filename = ?`,
		filename,
	)

	var f models.ISOFile
	var uploadedAt string
	var lastUsed *string
	if err := row.Scan(&f.ID, &f.Filename, &f.SizeBytes, &f.SHA256, &f.UploadedBy, &uploadedAt, &lastUsed); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("scanning ISO row: %w", err)
	}
	f.UploadedAt, _ = time.Parse("2006-01-02 15:04:05", uploadedAt)
	if lastUsed != nil {
		t, _ := time.Parse("2006-01-02 15:04:05", *lastUsed)
		f.LastUsed = &t
	}
	return &f, nil
}

// DeleteISO removes an ISO file record from the database.
func (db *DB) DeleteISO(filename string) error {
	_, err := db.Exec(`DELETE FROM iso_files WHERE filename = ?`, filename)
	if err != nil {
		return fmt.Errorf("deleting ISO record: %w", err)
	}
	return nil
}

// TouchISOLastUsed updates the last_used timestamp for an ISO file.
func (db *DB) TouchISOLastUsed(filename string) error {
	_, err := db.Exec(
		`UPDATE iso_files SET last_used = datetime('now') WHERE filename = ?`,
		filename,
	)
	if err != nil {
		return fmt.Errorf("updating ISO last_used: %w", err)
	}
	return nil
}

// ListUnusedISOs returns ISO files that have not been used since the given cutoff time.
func (db *DB) ListUnusedISOs(olderThan time.Time) ([]models.ISOFile, error) {
	rows, err := db.Query(
		`SELECT id, filename, size_bytes, COALESCE(sha256,''), COALESCE(uploaded_by,''), uploaded_at, last_used
		 FROM iso_files
		 WHERE last_used IS NULL AND uploaded_at < ?
		    OR last_used < ?
		 ORDER BY filename`,
		olderThan.Format("2006-01-02 15:04:05"),
		olderThan.Format("2006-01-02 15:04:05"),
	)
	if err != nil {
		return nil, fmt.Errorf("querying unused ISOs: %w", err)
	}
	defer rows.Close()

	var isos []models.ISOFile
	for rows.Next() {
		var f models.ISOFile
		var uploadedAt string
		var lastUsed *string
		if err := rows.Scan(&f.ID, &f.Filename, &f.SizeBytes, &f.SHA256, &f.UploadedBy, &uploadedAt, &lastUsed); err != nil {
			return nil, fmt.Errorf("scanning ISO row: %w", err)
		}
		f.UploadedAt, _ = time.Parse("2006-01-02 15:04:05", uploadedAt)
		if lastUsed != nil {
			t, _ := time.Parse("2006-01-02 15:04:05", *lastUsed)
			f.LastUsed = &t
		}
		isos = append(isos, f)
	}

	if isos == nil {
		isos = []models.ISOFile{}
	}
	return isos, rows.Err()
}
