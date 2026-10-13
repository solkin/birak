package store

import (
	"encoding/json"
	"fmt"
	"time"
)

// PartialDownload is a replica transfer that stopped part-way. Its scratch file
// holds Verified bytes of Meta's content whose SHA-256 is Prefix; anything the
// file holds beyond that was never confirmed durable and is discarded.
type PartialDownload struct {
	Meta     FileMeta
	Path     string // scratch file, slash-separated, relative to the sync root
	Verified int64
	Prefix   string
	Updated  time.Time
}

// PartialDownloads lists every recorded partial transfer.
func (s *Store) PartialDownloads() ([]PartialDownload, error) {
	rows, err := s.db.Query("SELECT metadata, path, verified_bytes, prefix_hash, updated_at FROM partial_downloads ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PartialDownload
	for rows.Next() {
		var p PartialDownload
		var encoded string
		var updated int64
		if err := rows.Scan(&encoded, &p.Path, &p.Verified, &p.Prefix, &updated); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(encoded), &p.Meta); err != nil {
			return nil, fmt.Errorf("decode partial download: %w", err)
		}
		p.Updated = time.Unix(0, updated)
		out = append(out, p)
	}
	return out, rows.Err()
}

// SavePartialDownload records how far a transfer got. The caller must have made
// those bytes durable first: the record is what a restart trusts.
func (s *Store) SavePartialDownload(p PartialDownload) error {
	encoded, err := json.Marshal(p.Meta)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err = s.db.Exec(`INSERT INTO partial_downloads (name, metadata, path, verified_bytes, prefix_hash, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
			metadata = excluded.metadata,
			path = excluded.path,
			verified_bytes = excluded.verified_bytes,
			prefix_hash = excluded.prefix_hash,
			updated_at = excluded.updated_at`,
		p.Meta.Name, string(encoded), p.Path, p.Verified, p.Prefix, time.Now().UnixNano())
	return err
}

// DropPartialDownload forgets a partial transfer. Its scratch file is the
// caller's to remove.
func (s *Store) DropPartialDownload(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec("DELETE FROM partial_downloads WHERE name = ?", name)
	return err
}
