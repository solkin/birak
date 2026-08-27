package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log/slog"
	"math"
	"net/url"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// NoAckGate disables the peer-acknowledgement gate in PurgeTombstones. Pass it
// when no peer is configured and tombstones only have to satisfy the TTL.
const NoAckGate = int64(math.MaxInt64)

// FileMeta represents a file entry in the store. It doubles as the wire format
// of /changes, /meta and /manifest, so only replicated fields are serialized.
type FileMeta struct {
	Name    string `json:"name"`
	ModTime int64  `json:"mod_time"` // UnixNano
	Size    int64  `json:"size"`
	Hash    string `json:"hash"` // SHA256 hex
	Deleted bool   `json:"deleted"`
	Version int64  `json:"version"`

	// DeletedAt is the local wall-clock time at which this node recorded the
	// deletion. It is node-local bookkeeping for tombstone retention and is
	// deliberately not replicated: every node ages its own tombstones.
	DeletedAt int64 `json:"-"`
}

// PeerState is what this node remembers about one peer between polls.
type PeerState struct {
	// Version is the highest peer version successfully consumed.
	Version int64
	// Epoch identifies the peer's database incarnation. A peer that lost or
	// restored its meta directory comes back with a different epoch, which
	// invalidates Version — the peer restarts numbering from 1 and a stale
	// cursor would silently hide every one of its changes forever.
	Epoch string
}

// RepairItem is one entry of the durable repair queue: a change from a peer
// that could not be applied yet and must not be forgotten when the cursor
// moves past it.
type RepairItem struct {
	PeerID    string
	Name      string
	Version   int64
	Attempts  int
	FirstSeen int64
	LastError string
}

// RepairStats summarizes the repair queue for /status.
type RepairStats struct {
	Total       int64 `json:"total"`
	Due         int64 `json:"due"`
	OldestAgeMS int64 `json:"oldest_age_ms"`
}

// Store manages the SQLite database for file metadata and peer cursors.
type Store struct {
	db            *sql.DB
	mu            sync.Mutex // serializes writes and version assignment
	logger        *slog.Logger
	cachedNextVer int64 // next version to assign, protected by mu
	epoch         string
}

// New creates a new Store, initializing the database schema.
func New(dbPath string, logger *slog.Logger) (*Store, error) {
	// Pragmas belong in the DSN, not in db.Exec: a pooled Exec applies the
	// pragma to whichever connection happened to serve it, leaving every other
	// connection on the defaults. Setting them here makes them per-connection
	// invariants, which is what lets the pool hold more than one connection.
	dsn := "file:" + url.PathEscape(dbPath) +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=busy_timeout(10000)" +
		"&_pragma=cache_size(-64000)"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database %s: %w", dbPath, err)
	}

	// WAL allows concurrent readers alongside a single writer. Every write path
	// takes s.mu, so extra connections only ever add read parallelism —
	// which matters because a periodic scan must not queue /changes for peers
	// behind it.
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(8)
	db.SetConnMaxLifetime(0)

	s := &Store{db: db, logger: logger}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}

	// Initialize the cached version counter from the current max version.
	// This is the only time we scan MAX(version); afterwards the counter
	// is maintained in-memory and incremented atomically with each PutFile.
	var maxVer sql.NullInt64
	if err := db.QueryRow("SELECT MAX(version) FROM files").Scan(&maxVer); err != nil {
		db.Close()
		return nil, fmt.Errorf("init version counter: %w", err)
	}
	if maxVer.Valid {
		s.cachedNextVer = maxVer.Int64 + 1
	} else {
		s.cachedNextVer = 1
	}

	epoch, err := s.loadOrCreateEpoch()
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("init epoch: %w", err)
	}
	s.epoch = epoch

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
	CREATE INDEX IF NOT EXISTS idx_files_deleted_name ON files(deleted, name);
	CREATE INDEX IF NOT EXISTS idx_files_name ON files(name);

	CREATE TABLE IF NOT EXISTS cursors (
		peer_id  TEXT PRIMARY KEY,
		last_ver INTEGER NOT NULL DEFAULT 0
	);

	-- Node-local key/value state. Holds the database epoch, which peers use to
	-- detect that this node's metadata was rebuilt.
	CREATE TABLE IF NOT EXISTS node_meta (
		key   TEXT PRIMARY KEY,
		value TEXT NOT NULL
	);

	-- Durable list of changes that could not be applied. It is what makes it
	-- safe for the cursor to advance past a failure: nothing is dropped, it is
	-- retried out of band until it succeeds.
	CREATE TABLE IF NOT EXISTS repair_queue (
		peer_id      TEXT    NOT NULL,
		name         TEXT    NOT NULL,
		version      INTEGER NOT NULL,
		attempts     INTEGER NOT NULL DEFAULT 0,
		next_attempt INTEGER NOT NULL,
		first_seen   INTEGER NOT NULL,
		last_error   TEXT    NOT NULL DEFAULT '',
		PRIMARY KEY (peer_id, name)
	);
	CREATE INDEX IF NOT EXISTS idx_repair_due ON repair_queue(peer_id, next_attempt);

	-- How far each peer has consumed OUR change stream, learned from the
	-- "since" parameter of its polls. Tombstones may only be purged once every
	-- live peer has moved past them, otherwise a deletion can be lost and the
	-- file resurrected by the next full reconciliation.
	CREATE TABLE IF NOT EXISTS peer_acks (
		peer_id       TEXT PRIMARY KEY,
		acked_version INTEGER NOT NULL,
		updated_at    INTEGER NOT NULL
	);
	`
	if _, err := s.db.Exec(schema); err != nil {
		return err
	}

	// Additive column migrations for databases created by earlier versions.
	if err := s.addColumnIfMissing("files", "deleted_at", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := s.addColumnIfMissing("cursors", "epoch", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}

	// Tombstones written before deleted_at existed carry 0, which would either
	// pin them forever or expose them to an immediate purge. Age them from now.
	if _, err := s.db.Exec(
		"UPDATE files SET deleted_at = ? WHERE deleted = 1 AND deleted_at = 0",
		time.Now().UnixNano(),
	); err != nil {
		return err
	}
	return nil
}

// addColumnIfMissing performs an idempotent ALTER TABLE ADD COLUMN.
func (s *Store) addColumnIfMissing(table, column, definition string) error {
	rows, err := s.db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return fmt.Errorf("inspect %s: %w", table, err)
	}
	defer rows.Close()

	found := false
	for rows.Next() {
		var (
			cid        int
			name       string
			typ        string
			notNull    int
			defaultVal sql.NullString
			pk         int
		)
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultVal, &pk); err != nil {
			return fmt.Errorf("scan %s schema: %w", table, err)
		}
		if name == column {
			found = true
			break
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close()
	if found {
		return nil
	}

	_, err = s.db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, definition))
	if err != nil && !strings.Contains(err.Error(), "duplicate column") {
		return fmt.Errorf("add column %s.%s: %w", table, column, err)
	}
	return nil
}

// loadOrCreateEpoch returns this database's epoch, generating one on first use.
func (s *Store) loadOrCreateEpoch() (string, error) {
	var epoch string
	err := s.db.QueryRow("SELECT value FROM node_meta WHERE key = 'epoch'").Scan(&epoch)
	if err == nil && epoch != "" {
		return epoch, nil
	}
	if err != nil && err != sql.ErrNoRows {
		return "", err
	}

	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	epoch = hex.EncodeToString(buf)
	if _, err := s.db.Exec("INSERT OR REPLACE INTO node_meta (key, value) VALUES ('epoch', ?)", epoch); err != nil {
		return "", err
	}
	return epoch, nil
}

// Epoch returns the identifier of this database incarnation. A fresh meta
// directory yields a fresh epoch, which tells peers to reset their cursors.
func (s *Store) Epoch() string {
	return s.epoch
}

// Close closes the database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

// PutFile inserts or updates a file entry with a new version.
// Returns the assigned version number.
func (s *Store) PutFile(name string, modTime int64, size int64, hash string, deleted bool) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	ver := s.cachedNextVer
	s.cachedNextVer++

	deletedInt := 0
	// deleted_at is wall-clock "when did this node record the deletion", which
	// is the only sound basis for a retention TTL. mod_time cannot be used: a
	// deletion inherits the file's own mtime, so deleting a month-old file
	// would produce a tombstone that a 7-day TTL discards immediately.
	var deletedAt int64
	if deleted {
		deletedInt = 1
		deletedAt = time.Now().UnixNano()
	}

	_, execErr := s.db.Exec(`
		INSERT INTO files (name, mod_time, size, hash, deleted, version, deleted_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
			mod_time   = excluded.mod_time,
			size       = excluded.size,
			hash       = excluded.hash,
			deleted    = excluded.deleted,
			version    = excluded.version,
			deleted_at = excluded.deleted_at
	`, name, modTime, size, hash, deletedInt, ver, deletedAt)
	if execErr != nil {
		// Roll back the version counter on failure so versions stay gapless.
		s.cachedNextVer = ver
		return 0, fmt.Errorf("upsert file %q: %w", name, execErr)
	}

	s.logger.Debug("store: file updated", "name", name, "version", ver, "hash", hash[:min(12, len(hash))], "deleted", deleted)
	return ver, nil
}

const fileColumns = "name, mod_time, size, hash, deleted, version, deleted_at"

func scanFile(sc interface{ Scan(...any) error }) (FileMeta, error) {
	var f FileMeta
	var deleted int
	err := sc.Scan(&f.Name, &f.ModTime, &f.Size, &f.Hash, &deleted, &f.Version, &f.DeletedAt)
	f.Deleted = deleted != 0
	return f, err
}

// GetFile returns a single file entry by name, or nil if not found.
func (s *Store) GetFile(name string) (*FileMeta, error) {
	row := s.db.QueryRow("SELECT "+fileColumns+" FROM files WHERE name = ?", name)
	f, err := scanFile(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get file %q: %w", name, err)
	}
	return &f, nil
}

// GetChanges returns files with version > sinceVersion, ordered by version, limited to limit entries.
func (s *Store) GetChanges(sinceVersion int64, limit int) ([]FileMeta, error) {
	rows, err := s.db.Query(
		"SELECT "+fileColumns+" FROM files WHERE version > ? ORDER BY version ASC LIMIT ?",
		sinceVersion, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("get changes since %d: %w", sinceVersion, err)
	}
	defer rows.Close()

	var result []FileMeta
	for rows.Next() {
		f, err := scanFile(rows)
		if err != nil {
			return nil, fmt.Errorf("scan file row: %w", err)
		}
		result = append(result, f)
	}
	return result, rows.Err()
}

// ListManifest returns every entry, tombstones included, in name order starting
// after afterName. Full reconciliation walks it to find differences that the
// version cursor can never surface — anything skipped, lost, or written while
// this node was down.
func (s *Store) ListManifest(afterName string, limit int) ([]FileMeta, error) {
	rows, err := s.db.Query(
		"SELECT "+fileColumns+" FROM files WHERE name > ? ORDER BY name ASC LIMIT ?",
		afterName, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list manifest after %q: %w", afterName, err)
	}
	defer rows.Close()

	var result []FileMeta
	for rows.Next() {
		f, err := scanFile(rows)
		if err != nil {
			return nil, fmt.Errorf("scan file row: %w", err)
		}
		result = append(result, f)
	}
	return result, rows.Err()
}

// MaxVersion returns the current maximum version number, or 0 if the table is empty.
// Uses the in-memory counter — O(1) instead of scanning the index.
func (s *Store) MaxVersion() (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cachedNextVer <= 1 {
		return 0, nil
	}
	return s.cachedNextVer - 1, nil
}

// FileCount returns the number of non-deleted files.
func (s *Store) FileCount() (int64, error) {
	var count int64
	err := s.db.QueryRow("SELECT COUNT(*) FROM files WHERE deleted = 0").Scan(&count)
	return count, err
}

// GetPeerState returns the cursor and remembered epoch for a peer.
func (s *Store) GetPeerState(peerID string) (PeerState, error) {
	var ps PeerState
	err := s.db.QueryRow("SELECT last_ver, epoch FROM cursors WHERE peer_id = ?", peerID).
		Scan(&ps.Version, &ps.Epoch)
	if err == sql.ErrNoRows {
		return PeerState{}, nil
	}
	return ps, err
}

// SetPeerState stores the cursor and epoch for a peer atomically, so a reset
// can never leave a new epoch paired with a stale version.
func (s *Store) SetPeerState(peerID string, ps PeerState) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(`
		INSERT INTO cursors (peer_id, last_ver, epoch) VALUES (?, ?, ?)
		ON CONFLICT(peer_id) DO UPDATE SET
			last_ver = excluded.last_ver,
			epoch    = excluded.epoch
	`, peerID, ps.Version, ps.Epoch)
	return err
}

// GetCursor returns the last seen version for a peer. Returns 0 if not found.
func (s *Store) GetCursor(peerID string) (int64, error) {
	ps, err := s.GetPeerState(peerID)
	return ps.Version, err
}

// SetCursor updates the last seen version for a peer, preserving its epoch.
func (s *Store) SetCursor(peerID string, version int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(`
		INSERT INTO cursors (peer_id, last_ver, epoch) VALUES (?, ?, '')
		ON CONFLICT(peer_id) DO UPDATE SET last_ver = excluded.last_ver
	`, peerID, version)
	return err
}

// PruneCursors drops cursor and ack rows for peers that are no longer
// configured, so a rotated peer list does not accumulate dead state forever.
func (s *Store) PruneCursors(keep []string) error {
	if len(keep) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	placeholders := make([]string, len(keep))
	args := make([]any, len(keep))
	for i, p := range keep {
		placeholders[i] = "?"
		args[i] = p
	}
	in := "(" + strings.Join(placeholders, ",") + ")"
	if _, err := s.db.Exec("DELETE FROM cursors WHERE peer_id NOT IN "+in, args...); err != nil {
		return err
	}
	_, err := s.db.Exec("DELETE FROM repair_queue WHERE peer_id NOT IN "+in, args...)
	return err
}

// RecordAck notes how far a peer has consumed our change stream. The "since"
// value of its poll is a proof of consumption: it will never ask for anything
// below that version again.
func (s *Store) RecordAck(peerID string, version int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if peerID == "" {
		return nil
	}
	_, err := s.db.Exec(`
		INSERT INTO peer_acks (peer_id, acked_version, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(peer_id) DO UPDATE SET
			acked_version = MAX(peer_acks.acked_version, excluded.acked_version),
			updated_at    = excluded.updated_at
	`, peerID, version, time.Now().UnixNano())
	return err
}

// MinAckedVersion returns the lowest version acknowledged by peers that were
// heard from within staleAfter. Peers silent for longer are excluded: they will
// perform a full reconciliation when they return, so they no longer pin
// tombstones. Returns NoAckGate when no live peer is known.
func (s *Store) MinAckedVersion(staleAfter time.Duration) (int64, error) {
	cutoff := time.Now().Add(-staleAfter).UnixNano()
	var minAck sql.NullInt64
	err := s.db.QueryRow(
		"SELECT MIN(acked_version) FROM peer_acks WHERE updated_at >= ?", cutoff,
	).Scan(&minAck)
	if err != nil {
		return 0, err
	}
	if !minAck.Valid {
		return NoAckGate, nil
	}
	return minAck.Int64, nil
}

// PeerAcks returns the acknowledged version per peer, for /status.
func (s *Store) PeerAcks() (map[string]int64, error) {
	rows, err := s.db.Query("SELECT peer_id, acked_version FROM peer_acks")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[string]int64)
	for rows.Next() {
		var id string
		var v int64
		if err := rows.Scan(&id, &v); err != nil {
			return nil, err
		}
		result[id] = v
	}
	return result, rows.Err()
}

// PurgeTombstones removes deleted file entries that are both older than ttl
// (measured from when the deletion was recorded) and already consumed by every
// live peer. Pass NoAckGate for maxVersion to apply the TTL alone.
func (s *Store) PurgeTombstones(ttl time.Duration, maxVersion int64) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	cutoff := time.Now().Add(-ttl).UnixNano()
	result, err := s.db.Exec(
		"DELETE FROM files WHERE deleted = 1 AND deleted_at < ? AND version <= ?",
		cutoff, maxVersion,
	)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// EnqueueRepair records a change that could not be applied. Keeping the highest
// version seen for a name means the retry always targets the newest state, and
// an existing entry keeps its attempt count so backoff is not reset by a
// repeated sighting.
func (s *Store) EnqueueRepair(peerID, name string, version int64, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UnixNano()
	_, err := s.db.Exec(`
		INSERT INTO repair_queue (peer_id, name, version, attempts, next_attempt, first_seen, last_error)
		VALUES (?, ?, ?, 0, ?, ?, ?)
		ON CONFLICT(peer_id, name) DO UPDATE SET
			version    = MAX(repair_queue.version, excluded.version),
			last_error = excluded.last_error
	`, peerID, name, version, now, now, reason)
	return err
}

// DueRepairs returns repair items for a peer whose backoff has elapsed.
func (s *Store) DueRepairs(peerID string, limit int) ([]RepairItem, error) {
	rows, err := s.db.Query(`
		SELECT peer_id, name, version, attempts, first_seen, last_error
		FROM repair_queue
		WHERE peer_id = ? AND next_attempt <= ?
		ORDER BY next_attempt ASC LIMIT ?
	`, peerID, time.Now().UnixNano(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []RepairItem
	for rows.Next() {
		var it RepairItem
		if err := rows.Scan(&it.PeerID, &it.Name, &it.Version, &it.Attempts, &it.FirstSeen, &it.LastError); err != nil {
			return nil, err
		}
		items = append(items, it)
	}
	return items, rows.Err()
}

// ResolveRepair removes an item that was applied successfully.
func (s *Store) ResolveRepair(peerID, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec("DELETE FROM repair_queue WHERE peer_id = ? AND name = ?", peerID, name)
	return err
}

// DeferRepair increments the attempt count and schedules the next try. Items are
// never dropped: an item that cannot be applied stays visible in /status rather
// than silently disappearing.
func (s *Store) DeferRepair(peerID, name string, retryAfter time.Duration, cause string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(`
		UPDATE repair_queue
		SET attempts = attempts + 1, next_attempt = ?, last_error = ?
		WHERE peer_id = ? AND name = ?
	`, time.Now().Add(retryAfter).UnixNano(), cause, peerID, name)
	return err
}

// RepairQueueStats summarizes outstanding repairs across all peers.
func (s *Store) RepairQueueStats() (RepairStats, error) {
	var st RepairStats
	var oldest sql.NullInt64
	err := s.db.QueryRow(`
		SELECT COUNT(*),
		       COALESCE(SUM(CASE WHEN next_attempt <= ? THEN 1 ELSE 0 END), 0),
		       MIN(first_seen)
		FROM repair_queue
	`, time.Now().UnixNano()).Scan(&st.Total, &st.Due, &oldest)
	if err != nil {
		return st, err
	}
	if oldest.Valid {
		st.OldestAgeMS = (time.Now().UnixNano() - oldest.Int64) / int64(time.Millisecond)
	}
	return st, nil
}

// GetFilesBatch returns metadata for a batch of file names.
// Missing files are simply absent from the result map.
func (s *Store) GetFilesBatch(names []string) (map[string]*FileMeta, error) {
	if len(names) == 0 {
		return nil, nil
	}

	placeholders := make([]string, len(names))
	args := make([]interface{}, len(names))
	for i, n := range names {
		placeholders[i] = "?"
		args[i] = n
	}

	query := "SELECT " + fileColumns + " FROM files WHERE name IN (" +
		strings.Join(placeholders, ",") + ")"

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("get files batch: %w", err)
	}
	defer rows.Close()

	result := make(map[string]*FileMeta, len(names))
	for rows.Next() {
		f, err := scanFile(rows)
		if err != nil {
			return nil, fmt.Errorf("scan file row: %w", err)
		}
		entry := f
		result[f.Name] = &entry
	}
	return result, rows.Err()
}

// ListNonDeleted returns non-deleted file entries in name order, starting
// after afterName, limited to limit entries. Used for paginated iteration
// without loading the entire table into memory.
func (s *Store) ListNonDeleted(afterName string, limit int) ([]FileMeta, error) {
	rows, err := s.db.Query(
		"SELECT "+fileColumns+" FROM files WHERE deleted = 0 AND name > ? ORDER BY name ASC LIMIT ?",
		afterName, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list non-deleted: %w", err)
	}
	defer rows.Close()

	var result []FileMeta
	for rows.Next() {
		f, err := scanFile(rows)
		if err != nil {
			return nil, fmt.Errorf("scan file row: %w", err)
		}
		result = append(result, f)
	}
	return result, rows.Err()
}

// AllFiles returns all non-deleted file entries (for periodic scan diffing).
// Deprecated: prefer ListNonDeleted for paginated access to avoid memory spikes.
func (s *Store) AllFiles() (map[string]FileMeta, error) {
	rows, err := s.db.Query("SELECT " + fileColumns + " FROM files WHERE deleted = 0")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[string]FileMeta)
	for rows.Next() {
		f, err := scanFile(rows)
		if err != nil {
			return nil, err
		}
		result[f.Name] = f
	}
	return result, rows.Err()
}

// PendingRepairCount returns how many items are queued for a peer, due or not.
func (s *Store) PendingRepairCount(peerID string) (int64, error) {
	var n int64
	err := s.db.QueryRow("SELECT COUNT(*) FROM repair_queue WHERE peer_id = ?", peerID).Scan(&n)
	return n, err
}
