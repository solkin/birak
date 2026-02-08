package store

import (
	"database/sql"
	"fmt"
	"log/slog"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// FileMeta represents a file entry in the store.
type FileMeta struct {
	Name    string `json:"name"`
	ModTime int64  `json:"mod_time"` // UnixNano
	Size    int64  `json:"size"`
	Hash    string `json:"hash"` // SHA256 hex
	Deleted bool   `json:"deleted"`
	Version int64  `json:"version"`
}

// Store manages the SQLite database for file metadata and peer cursors.
type Store struct {
	db     *sql.DB
	mu     sync.Mutex // serializes writes
	logger *slog.Logger
}

// New creates a new Store, initializing the database schema.
func New(dbPath string, logger *slog.Logger) (*Store, error) {
	db, err := sql.Open("sqlite", dbPath+"?_journal_mode=WAL")
	if err != nil {
		return nil, fmt.Errorf("open database %s: %w", dbPath, err)
	}

	// Serialize all access through a single connection to avoid SQLITE_BUSY
	// errors. With WAL mode this still allows concurrent reads during writes
	// within the same connection.
	db.SetMaxOpenConns(1)

	// Enable WAL mode and set pragmas for performance.
	pragmas := []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA cache_size=-64000", // 64MB
		"PRAGMA busy_timeout=5000",
	}
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			db.Close()
			return nil, fmt.Errorf("set pragma %q: %w", p, err)
		}
	}

	s := &Store{db: db, logger: logger}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}

	return s, nil
}

func (s *Store) migrate() error {
	schema := `
	CREATE TABLE IF NOT EXISTS files (
		name     TEXT PRIMARY KEY,
		mod_time INTEGER NOT NULL,
		size     INTEGER NOT NULL,
		hash     TEXT NOT NULL,
		deleted  INTEGER NOT NULL DEFAULT 0,
		version  INTEGER NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_files_version ON files(version);

	CREATE TABLE IF NOT EXISTS cursors (
		peer_id  TEXT PRIMARY KEY,
		last_ver INTEGER NOT NULL DEFAULT 0
	);
	`
	_, err := s.db.Exec(schema)
	return err
}

// Close closes the database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

// nextVersion returns the next version number (max + 1).
// Must be called while holding s.mu.
func (s *Store) nextVersion() (int64, error) {
	var maxVer sql.NullInt64
	err := s.db.QueryRow("SELECT MAX(version) FROM files").Scan(&maxVer)
	if err != nil {
		return 0, err
	}
	if !maxVer.Valid {
		return 1, nil
	}
	return maxVer.Int64 + 1, nil
}

// PutFile inserts or updates a file entry with a new version.
// Returns the assigned version number.
func (s *Store) PutFile(name string, modTime int64, size int64, hash string, deleted bool) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	ver, err := s.nextVersion()
	if err != nil {
		return 0, fmt.Errorf("next version: %w", err)
	}

	deletedInt := 0
	if deleted {
		deletedInt = 1
	}

	_, err = s.db.Exec(`
		INSERT INTO files (name, mod_time, size, hash, deleted, version)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
			mod_time = excluded.mod_time,
			size     = excluded.size,
			hash     = excluded.hash,
			deleted  = excluded.deleted,
			version  = excluded.version
	`, name, modTime, size, hash, deletedInt, ver)
	if err != nil {
		return 0, fmt.Errorf("upsert file %q: %w", name, err)
	}

	s.logger.Debug("store: file updated", "name", name, "version", ver, "hash", hash[:min(12, len(hash))], "deleted", deleted)
	return ver, nil
}

// GetFile returns a single file entry by name, or nil if not found.
func (s *Store) GetFile(name string) (*FileMeta, error) {
	row := s.db.QueryRow("SELECT name, mod_time, size, hash, deleted, version FROM files WHERE name = ?", name)

	var f FileMeta
	var deleted int
	err := row.Scan(&f.Name, &f.ModTime, &f.Size, &f.Hash, &deleted, &f.Version)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get file %q: %w", name, err)
	}
	f.Deleted = deleted != 0
	return &f, nil
}

// GetChanges returns files with version > sinceVersion, ordered by version, limited to limit entries.
func (s *Store) GetChanges(sinceVersion int64, limit int) ([]FileMeta, error) {
	rows, err := s.db.Query(
		"SELECT name, mod_time, size, hash, deleted, version FROM files WHERE version > ? ORDER BY version ASC LIMIT ?",
		sinceVersion, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("get changes since %d: %w", sinceVersion, err)
	}
	defer rows.Close()

	var result []FileMeta
	for rows.Next() {
		var f FileMeta
		var deleted int
		if err := rows.Scan(&f.Name, &f.ModTime, &f.Size, &f.Hash, &deleted, &f.Version); err != nil {
			return nil, fmt.Errorf("scan file row: %w", err)
		}
		f.Deleted = deleted != 0
		result = append(result, f)
	}
	return result, rows.Err()
}

// MaxVersion returns the current maximum version number, or 0 if the table is empty.
func (s *Store) MaxVersion() (int64, error) {
	var maxVer sql.NullInt64
	err := s.db.QueryRow("SELECT MAX(version) FROM files").Scan(&maxVer)
	if err != nil {
		return 0, err
	}
	if !maxVer.Valid {
		return 0, nil
	}
	return maxVer.Int64, nil
}

// FileCount returns the number of non-deleted files.
func (s *Store) FileCount() (int64, error) {
	var count int64
	err := s.db.QueryRow("SELECT COUNT(*) FROM files WHERE deleted = 0").Scan(&count)
	return count, err
}

// GetCursor returns the last seen version for a peer. Returns 0 if not found.
func (s *Store) GetCursor(peerID string) (int64, error) {
	var ver int64
	err := s.db.QueryRow("SELECT last_ver FROM cursors WHERE peer_id = ?", peerID).Scan(&ver)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return ver, err
}

// SetCursor updates the last seen version for a peer.
func (s *Store) SetCursor(peerID string, version int64) error {
	_, err := s.db.Exec(`
		INSERT INTO cursors (peer_id, last_ver) VALUES (?, ?)
		ON CONFLICT(peer_id) DO UPDATE SET last_ver = excluded.last_ver
	`, peerID, version)
	return err
}

// PurgeTombstones removes deleted file entries older than the given TTL.
func (s *Store) PurgeTombstones(ttl time.Duration) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	cutoff := time.Now().Add(-ttl).UnixNano()
	result, err := s.db.Exec("DELETE FROM files WHERE deleted = 1 AND mod_time < ?", cutoff)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// AllFiles returns all non-deleted file entries (for periodic scan diffing).
func (s *Store) AllFiles() (map[string]FileMeta, error) {
	rows, err := s.db.Query("SELECT name, mod_time, size, hash, deleted, version FROM files WHERE deleted = 0")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[string]FileMeta)
	for rows.Next() {
		var f FileMeta
		var deleted int
		if err := rows.Scan(&f.Name, &f.ModTime, &f.Size, &f.Hash, &deleted, &f.Version); err != nil {
			return nil, err
		}
		f.Deleted = deleted != 0
		result[f.Name] = f
	}
	return result, rows.Err()
}
