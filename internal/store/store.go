package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/url"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// NoAckGate disables the version filter in the low-level PurgeTombstones helper.
// The daemon never purges tombstones: a stream cursor is not an application ACK.
const NoAckGate = int64(math.MaxInt64)

// FileMeta represents a file entry in the store. It doubles as the wire format
// of /changes, /meta and /manifest, so only replicated fields are serialized.
type FileMeta struct {
	Name     string `json:"name"`
	ModTime  int64  `json:"mod_time"` // UnixNano
	Size     int64  `json:"size"`
	Hash     string `json:"hash"` // SHA256 hex
	Deleted  bool   `json:"deleted"`
	Version  int64  `json:"version"`
	BigClock string `json:"big_clock,omitempty"`
	Clock    int64  `json:"clock"` // per-path logical time; independent of filesystem mtime
	// A namespace tombstone carries the winning live state's clock, mtime,
	// hash and name. It must not invent a clock that can defeat a newer file.
	SupersededBy string `json:"superseded_by,omitempty"`
	ConflictOf   string `json:"conflict_of,omitempty"` // original name of a preserved copy

	// DeletedAt is the local wall-clock time at which this node recorded the
	// deletion. It is node-local bookkeeping for tombstone retention and is
	// deliberately not replicated: every node ages its own tombstones.
	DeletedAt int64 `json:"-"`
}

// PeerState is what this node remembers about one peer between polls.
type PeerState struct {
	// Version is the highest peer version successfully consumed.
	Version int64
	// Epoch identifies the peer's process incarnation. Every restart invalidates
	// Version, including a restart against a restored metadata backup.
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
	Revision  int64
	Meta      *FileMeta
}

// RepairStats summarizes the repair queue for /status.
type RepairStats struct {
	Total       int64 `json:"total"`
	Due         int64 `json:"due"`
	OldestAgeMS int64 `json:"oldest_age_ms"`
}

// DefaultRepairLimit caps how many distinct names one peer may have queued.
// A peer that diverges wholesale — a wrong ignore list, a restored backup, a
// broken build — would otherwise grow this database without any bound. The cap
// turns that into back-pressure: enqueue fails, the cursor stops advancing, and
// the backlog is visible in /status instead of filling the disk.
const DefaultRepairLimit = 100_000

// ErrRepairQueueFull reports that cap. It is a signal to stop consuming more
// work from that peer, never a reason to drop the change: the caller must not
// advance its cursor past an entry it could not record.
var ErrRepairQueueFull = errors.New("repair queue is full")

// Store manages the SQLite database for file metadata and peer cursors.
type Store struct {
	db            *sql.DB
	mu            sync.Mutex // serializes writes and version assignment
	logger        *slog.Logger
	cachedNextVer int64 // next version to assign, protected by mu
	epoch         string
	incarnation   string
	repairLimit   int64 // 0 disables the cap; protected by mu

	// Queue summaries are read by monitoring, not by replication, and cost a
	// scan. They are remembered under their own mutex so a scrape never queues
	// behind a write, and dropped whenever the queue changes.
	statsMu    sync.Mutex
	statsCache RepairStats
	statsValid bool
	statsGen   uint64
	// queueSize is the per-peer queue length, kept up to date as rows come and
	// go. Counting rows to answer "is there room for one more" turned a bulk
	// sync into O(n²): every new name scanned the whole queue first.
	queueSize      map[string]int64
	integrityMu    sync.RWMutex
	quarantined    map[string]bool
	damagedObjects map[string]bool
	countMu        sync.Mutex
	countVersion   int64
	countValid     bool
	countCached    int64
}

// SetRepairLimit changes the per-peer queue cap. Zero disables it.
func (s *Store) SetRepairLimit(limit int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.repairLimit = max(0, limit)
}

// New creates a new Store, initializing the database schema.
func New(dbPath string, logger *slog.Logger) (*Store, error) {
	// Pragmas belong in the DSN, not in db.Exec: a pooled Exec applies the
	// pragma to whichever connection happened to serve it, leaving every other
	// connection on the defaults. Setting them here makes them per-connection
	// invariants, which is what lets the pool hold more than one connection.
	dsn := "file:" + url.PathEscape(dbPath) +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(FULL)" +
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

	s := &Store{db: db, logger: logger, repairLimit: DefaultRepairLimit, queueSize: make(map[string]int64), quarantined: make(map[string]bool), damagedObjects: make(map[string]bool)}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}

	if err := s.migrateNames(); err != nil {
		db.Close()
		return nil, fmt.Errorf("portable namespace: %w", err)
	}

	// Migrate legacy databases from MAX(version), retaining any higher persisted
	// high-water mark. PutFile commits the counter and file state together.
	var maxVer int64
	if _, err := db.Exec(`INSERT INTO node_meta(key, value)
		VALUES ('last_version', (SELECT COALESCE(MAX(version), 0) FROM files))
		ON CONFLICT(key) DO UPDATE SET value = MAX(CAST(node_meta.value AS INTEGER), CAST(excluded.value AS INTEGER))`); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate version counter: %w", err)
	}
	if err := db.QueryRow("SELECT CAST(value AS INTEGER) FROM node_meta WHERE key = 'last_version'").Scan(&maxVer); err != nil {
		db.Close()
		return nil, fmt.Errorf("init version counter: %w", err)
	}
	s.cachedNextVer = maxVer + 1

	if err := s.loadQueueSizes(); err != nil {
		db.Close()
		return nil, fmt.Errorf("count queued repairs: %w", err)
	}

	epoch, err := s.loadOrCreateEpoch()
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("init epoch: %w", err)
	}
	s.epoch = epoch
	// A process incarnation also invalidates cursors after a restored database
	// has already caught up with its former high-water mark. Replaying on restart
	// is deliberate; hash/version comparisons make it idempotent.
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		db.Close()
		return nil, err
	}
	s.incarnation = epoch + "-" + hex.EncodeToString(buf)

	if err := s.loadQuarantine(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// loadQueueSizes counts the queue once, at open, so nothing else has to.
func (s *Store) loadQueueSizes() error {
	rows, err := s.db.Query("SELECT peer_id, COUNT(*) FROM repair_queue GROUP BY peer_id")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var peer string
		var count int64
		if err := rows.Scan(&peer, &count); err != nil {
			return err
		}
		s.queueSize[peer] = count
	}
	return rows.Err()
}

// addQueueSize adjusts a peer's queue length and drops the cached summary.
func (s *Store) addQueueSize(peerID string, delta int64) {
	s.statsMu.Lock()
	defer s.statsMu.Unlock()
	s.queueSize[peerID] = max(0, s.queueSize[peerID]+delta)
	s.statsGen++
	s.statsValid = false
}

func (s *Store) queuedFor(peerID string) int64 {
	s.statsMu.Lock()
	defer s.statsMu.Unlock()
	return s.queueSize[peerID]
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

	CREATE TABLE IF NOT EXISTS namespace_names (folded TEXT PRIMARY KEY, name TEXT NOT NULL);
	CREATE TABLE IF NOT EXISTS local_intents (path TEXT PRIMARY KEY);
	CREATE TABLE IF NOT EXISTS quarantine (name TEXT PRIMARY KEY);
	CREATE TABLE IF NOT EXISTS damaged_objects (identity TEXT PRIMARY KEY);
 CREATE TABLE IF NOT EXISTS replica_intents (name TEXT PRIMARY KEY, metadata TEXT NOT NULL);
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
	-- "since" parameter of its polls. This is diagnostic consumption progress,
	-- not proof of application, and never authorizes automatic tombstone GC.
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
	for _, column := range []struct{ table, name, definition string }{
		{"files", "clock", "INTEGER NOT NULL DEFAULT 0"},
		{"files", "big_clock", "TEXT NOT NULL DEFAULT ''"},
		{"files", "superseded_by", "TEXT NOT NULL DEFAULT ''"},
		{"files", "conflict_of", "TEXT NOT NULL DEFAULT ''"},
		{"repair_queue", "metadata", "TEXT NOT NULL DEFAULT ''"},
		{"repair_queue", "revision", "INTEGER NOT NULL DEFAULT 1"},
		{"peer_acks", "epoch", "TEXT NOT NULL DEFAULT ''"},
	} {
		if err := s.addColumnIfMissing(column.table, column.name, column.definition); err != nil {
			return err
		}
	}

	// Repair generations must survive deletion/reinsertion, not just UPSERT.
	if _, err := s.db.Exec(`INSERT INTO node_meta(key,value)
 VALUES ('repair_revision', (SELECT COALESCE(MAX(revision),0) FROM repair_queue))
 ON CONFLICT(key) DO UPDATE SET value=MAX(CAST(node_meta.value AS INTEGER),CAST(excluded.value AS INTEGER))`); err != nil {
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

// Incarnation changes on every open, including when a backup is restored.
func (s *Store) Incarnation() string { return s.incarnation }

// PerPeerKeyPrefix marks node_meta keys that belong to one peer and must be
// dropped with it.
const PerPeerKeyPrefix = "reconcile_position:"

// NodeValue and SetNodeValue store node-local recovery state.
func (s *Store) NodeValue(key string) (string, error) {
	var value string
	err := s.db.QueryRow("SELECT value FROM node_meta WHERE key = ?", key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return value, err
}

func (s *Store) SetNodeValue(key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec("INSERT INTO node_meta(key,value) VALUES (?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", key, value)
	return err
}

// Close closes the database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

// PutFile inserts or updates a file entry with a new version.
// Returns the assigned version number.
func (s *Store) PutFile(name string, modTime int64, size int64, hash string, deleted bool) (int64, error) {
	return s.put(FileMeta{Name: name, ModTime: modTime, Size: size, Hash: hash, Deleted: deleted}, false)
}

// PutLocal advances this path beyond every state it has observed, even when a
// client preserves or rolls back filesystem timestamps.
func (s *Store) PutLocal(meta FileMeta) (int64, error) { return s.put(meta, true) }

// PutRemote preserves the source conflict clock; Version remains node-local.
func (s *Store) PutRemote(meta FileMeta) (int64, error) { return s.put(meta, false) }

// put commits one file state and logs it afterwards. Logging is synchronous, so
// doing it under s.mu would let a log consumer that stops reading stall every
// metadata write on this node — including the ones peers are waiting for.
func (s *Store) put(meta FileMeta, local bool) (int64, error) {
	ver, err := s.commitPut(meta, local)
	if err != nil {
		return 0, err
	}
	s.logger.Debug("store: file updated", "name", meta.Name, "version", ver,
		"hash", meta.Hash[:min(12, len(meta.Hash))], "deleted", meta.Deleted)
	return ver, nil
}

func (s *Store) commitPut(meta FileMeta, local bool) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	name, modTime, size, hash, deleted := meta.Name, meta.ModTime, meta.Size, meta.Hash, meta.Deleted
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if err := bindName(tx, name); err != nil {
		return 0, err
	}
	if err := ValidateClock(meta); err != nil {
		return 0, err
	}
	if local {
		previous, err := namespaceClock(tx, name)
		if err != nil {
			return 0, err
		}
		AdvanceClock(&meta, previous)
	}

	var ver int64
	if err := tx.QueryRow(`UPDATE node_meta SET value = CAST(value AS INTEGER) + 1
		WHERE key = 'last_version' RETURNING CAST(value AS INTEGER)`).Scan(&ver); err != nil {
		return 0, err
	}

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

	_, execErr := tx.Exec(`
		INSERT INTO files (name, mod_time, size, hash, deleted, version, deleted_at, clock, superseded_by, conflict_of, big_clock)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
			mod_time   = excluded.mod_time,
			size       = excluded.size,
			hash       = excluded.hash,
			deleted    = excluded.deleted,
			version    = excluded.version,
			deleted_at = excluded.deleted_at,
 clock = excluded.clock,
 superseded_by = excluded.superseded_by,
 conflict_of = excluded.conflict_of,
 big_clock = excluded.big_clock
	`, name, modTime, size, hash, deletedInt, ver, deletedAt, meta.Clock, meta.SupersededBy, meta.ConflictOf, meta.BigClock)
	if execErr != nil {
		// Roll back the version counter on failure so versions stay gapless.
		return 0, fmt.Errorf("upsert file %q: %w", name, execErr)
	}
	if _, err := tx.Exec("DELETE FROM replica_intents WHERE name=?", name); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit file %q: %w", name, err)
	}
	s.cachedNextVer = ver + 1
	return ver, nil
}

const fileColumns = "name, mod_time, size, hash, deleted, version, deleted_at, clock, superseded_by, conflict_of, big_clock"

func scanFile(sc interface{ Scan(...any) error }) (FileMeta, error) {
	var f FileMeta
	var deleted int
	err := sc.Scan(&f.Name, &f.ModTime, &f.Size, &f.Hash, &deleted, &f.Version, &f.DeletedAt, &f.Clock, &f.SupersededBy, &f.ConflictOf, &f.BigClock)
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

// MaxVersion returns the highest committed version, even if its row was removed.
// Uses the in-memory counter — O(1) instead of scanning the index.
func (s *Store) MaxVersion() (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cachedNextVer <= 1 {
		return 0, nil
	}
	return s.cachedNextVer - 1, nil
}

// FileCount caches the indexed COUNT until the next committed file mutation.
// Frequent monitoring must not scan half a million names on every scrape.
func (s *Store) FileCount() (int64, error) {
	s.countMu.Lock()
	defer s.countMu.Unlock()
	version, err := s.MaxVersion()
	if err != nil {
		return 0, err
	}
	if s.countValid && s.countVersion == version {
		return s.countCached, nil
	}
	var count int64
	err = s.db.QueryRow("SELECT COUNT(*) FROM files WHERE deleted = 0").Scan(&count)
	if err == nil {
		// A concurrent mutation leaves this cache tagged with the earlier
		// version, so the next call recomputes rather than keeping a stale count.
		s.countCached, s.countVersion, s.countValid = count, version, true
	}
	return count, err
}

// ListLiveRange seeks in the live-name index. Bounds are [from, until), with
// an additional exclusive cursor. It never materializes a whole bucket.
func (s *Store) ListLiveRange(ctx context.Context, from, until, after string, limit int) ([]FileMeta, error) {
	if limit <= 0 || limit > 10000 {
		return nil, fmt.Errorf("invalid live range limit %d", limit)
	}
	operator, lower := ">=", from
	if after >= from {
		operator, lower = ">", after
	}
	// Give SQLite one lower bound, so a deep cursor always becomes the index
	// seek rather than a residual filter on an earlier bucket-prefix bound.
	rows, err := s.db.QueryContext(ctx, "SELECT "+fileColumns+
		" FROM files WHERE deleted=0 AND name"+operator+"? AND name<? ORDER BY name LIMIT ?", lower, until, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var page []FileMeta
	for rows.Next() {
		meta, err := scanFile(rows)
		if err != nil {
			return nil, err
		}
		page = append(page, meta)
	}
	return page, rows.Err()
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

// PruneCursors drops stream positions for peers that are no longer configured.
// Accepted repairs are independent of membership: a removed source may hold
// the only queued deletion or file state. Keep that work durable and visible
// until it is resolved; re-adding the peer resumes its existing queue.
func (s *Store) PruneCursors(keep []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(keep) == 0 {
		if _, err := s.db.Exec("DELETE FROM cursors"); err != nil {
			return err
		}
		return s.pruneNodeKeysLocked(nil)
	}

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
	return s.pruneNodeKeysLocked(keep)
}

// pruneNodeKeysLocked drops per-peer positions for peers that are gone. They
// are one row each and easy to forget, which is how "a few rows" becomes a
// table nobody ever cleans.
func (s *Store) pruneNodeKeysLocked(keep []string) error {
	wanted := make(map[string]bool, len(keep))
	for _, peer := range keep {
		wanted[peer] = true
	}
	rows, err := s.db.Query("SELECT key FROM node_meta WHERE key LIKE ?", PerPeerKeyPrefix+"%")
	if err != nil {
		return err
	}
	var stale []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			rows.Close()
			return err
		}
		if !wanted[strings.TrimPrefix(key, PerPeerKeyPrefix)] {
			stale = append(stale, key)
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, key := range stale {
		if _, err := s.db.Exec("DELETE FROM node_meta WHERE key = ?", key); err != nil {
			return err
		}
	}
	return nil
}

// RecordAck records a legacy peer's most recently observed stream cursor.
func (s *Store) RecordAck(peerID string, version int64) error {
	return s.RecordAckEpoch(peerID, "", version)
}

// RecordAckEpoch records the most recent observation, including a rewind.
// This is diagnostic state, NOT proof of application and NOT a GC gate.
// Reordered requests may conservatively lower it. Tombstone GC is disabled in
// the daemon; membership and application acknowledgements need a separate protocol.
func (s *Store) RecordAckEpoch(peerID, epoch string, version int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if peerID == "" {
		return nil
	}
	_, err := s.db.Exec(`
		INSERT INTO peer_acks (peer_id, acked_version, updated_at, epoch) VALUES (?, ?, ?, ?)
		ON CONFLICT(peer_id) DO UPDATE SET
			acked_version = excluded.acked_version,
			epoch = excluded.epoch,
			updated_at    = excluded.updated_at
	`, peerID, version, time.Now().UnixNano(), epoch)
	return err
}

// MinAckedVersion returns the lowest recently observed stream cursor, or
// NoAckGate if none is known. This diagnostic is insufficient to authorize GC.
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

// PurgeTombstones is a low-level maintenance helper, unused by the daemon.
// It removes records older than ttl and at or below maxVersion. The caller
// must establish safety independently; poll cursors do not prove application.
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

// EnqueueRepair accepts legacy name-only work. New replication paths use
// EnqueueChange so recovery does not depend on the source retaining metadata.
func (s *Store) EnqueueRepair(peerID, name string, version int64, reason string) error {
	return s.enqueueRepair(peerID, name, version, reason, nil)
}

// StateClock supports pre-clock database rows and imported metadata.
func (f FileMeta) StateClock() int64 {
	if f.Clock != 0 {
		return f.Clock
	}
	return f.ModTime
}

// CompareState returns the deterministic ordering of replicated states. A nil
// state precedes everything, including deletion history for an unknown name.
func CompareState(a, b *FileMeta) int {
	if a == nil {
		if b == nil {
			return 0
		}
		return -1
	}
	if b == nil {
		return 1
	}
	if c := CompareClock(*a, *b); c != 0 {
		return c
	}
	if a.ModTime < b.ModTime {
		return -1
	}
	if a.ModTime > b.ModTime {
		return 1
	}
	aLive, bLive := !a.Deleted || a.SupersededBy != "", !b.Deleted || b.SupersededBy != ""
	if aLive != bLive {
		if !aLive {
			return -1
		}
		return 1
	}
	if !aLive {
		return 0
	}
	if c := strings.Compare(a.Hash, b.Hash); c != 0 {
		return c
	}
	aName, bName := a.Name, b.Name
	if a.SupersededBy != "" {
		aName = a.SupersededBy
	}
	if b.SupersededBy != "" {
		bName = b.SupersededBy
	}
	return strings.Compare(aName, bName)
}

// EnqueueChange retains the complete winning state, so even a deletion purged
// by an older peer can still be applied after restart.
func (s *Store) EnqueueChange(peerID string, meta FileMeta, reason string) error {
	return s.enqueueRepair(peerID, meta.Name, meta.Version, reason, &meta)
}

func (s *Store) enqueueRepair(peerID, name string, version int64, reason string, meta *FileMeta) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var encoded string
	err := s.db.QueryRow("SELECT metadata FROM repair_queue WHERE peer_id=? AND name=?", peerID, name).Scan(&encoded)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	// Only a new name can grow the queue; updating a row already in it must
	// always be allowed, or a full queue could never record newer state.
	fresh := err == sql.ErrNoRows
	if fresh && s.repairLimit > 0 {
		if queued := s.queuedFor(peerID); queued >= s.repairLimit {
			return fmt.Errorf("%w: peer %s holds %d entries", ErrRepairQueueFull, peerID, queued)
		}
	}
	if meta != nil {
		var previous FileMeta
		if encoded == "" || json.Unmarshal([]byte(encoded), &previous) != nil || CompareState(meta, &previous) >= 0 {
			data, err := json.Marshal(meta)
			if err != nil {
				return err
			}
			encoded = string(data)
		}
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var revision int64
	if err := tx.QueryRow(`UPDATE node_meta SET value=CAST(value AS INTEGER)+1 WHERE key='repair_revision' RETURNING CAST(value AS INTEGER)`).Scan(&revision); err != nil {
		return err
	}
	now := time.Now().UnixNano()
	_, err = tx.Exec(`
  INSERT INTO repair_queue (peer_id, name, version, attempts, next_attempt, first_seen, last_error, metadata, revision)
		VALUES (?, ?, ?, 0, ?, ?, ?, ?, ?)
		ON CONFLICT(peer_id, name) DO UPDATE SET
			version    = MAX(repair_queue.version, excluded.version),
			metadata   = excluded.metadata,
			revision   = excluded.revision,
			last_error = excluded.last_error
	`, peerID, name, version, now, now, reason, encoded, revision)
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if fresh {
		s.addQueueSize(peerID, 1)
	} else {
		s.invalidateQueueStats()
	}
	return nil
}

// DueRepairs returns repair items for a peer whose backoff has elapsed.
func (s *Store) DueRepairs(peerID string, limit int) ([]RepairItem, error) {
	rows, err := s.db.Query(`
		SELECT peer_id, name, version, attempts, first_seen, last_error, revision, metadata
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
		var encoded string
		if err := rows.Scan(&it.PeerID, &it.Name, &it.Version, &it.Attempts, &it.FirstSeen, &it.LastError, &it.Revision, &encoded); err != nil {
			return nil, err
		}
		if encoded != "" {
			it.Meta = &FileMeta{}
			if err := json.Unmarshal([]byte(encoded), it.Meta); err != nil {
				return nil, fmt.Errorf("decode queued state: %w", err)
			}
		}
		items = append(items, it)
	}
	return items, rows.Err()
}

// ResolveRepair removes an item that was applied successfully.
func (s *Store) ResolveRepair(peerID, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	result, err := s.db.Exec("DELETE FROM repair_queue WHERE peer_id = ? AND name = ?", peerID, name)
	s.trackDeletion(peerID, result)
	return err
}

// trackDeletion keeps the queue length honest after a delete that may or may
// not have matched a row.
func (s *Store) trackDeletion(peerID string, result sql.Result) {
	removed := int64(0)
	if result != nil {
		if n, err := result.RowsAffected(); err == nil {
			removed = n
		}
	}
	s.addQueueSize(peerID, -removed)
}

// ResolveRepairItem cannot erase an update enqueued while this retry ran.
func (s *Store) ResolveRepairItem(item RepairItem) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	result, err := s.db.Exec("DELETE FROM repair_queue WHERE peer_id=? AND name=? AND revision=?", item.PeerID, item.Name, item.Revision)
	s.trackDeletion(item.PeerID, result)
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
	s.invalidateQueueStats()
	return err
}

// DeferRepairItem cannot postpone a replacement enqueued after this retry began.
func (s *Store) DeferRepairItem(item RepairItem, retryAfter time.Duration, cause string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`UPDATE repair_queue SET attempts=attempts+1,next_attempt=?,last_error=? WHERE peer_id=? AND name=? AND revision=?`, time.Now().Add(retryAfter).UnixNano(), cause, item.PeerID, item.Name, item.Revision)
	s.invalidateQueueStats()
	return err
}

// RepairQueueStats summarizes outstanding repairs across all peers.
//
// Both /status and /metrics ask for this, monitoring scrapes them on a timer,
// and answering costs a scan of the whole queue — worst exactly when the queue
// is large, which is when it is looked at most. The answer is therefore
// remembered until the queue changes, so a stuck backlog is scanned once rather
// than on every scrape. Any write invalidates it, so a caller still reads back
// what it just wrote.
func (s *Store) RepairQueueStats() (RepairStats, error) {
	s.statsMu.Lock()
	cached, valid, generation := s.statsCache, s.statsValid, s.statsGen
	s.statsMu.Unlock()
	if valid {
		return cached, nil
	}

	// Never hold a lock across a database call: a writer finishing its
	// transaction has to be able to invalidate this immediately.
	st, err := s.queryRepairStats()
	if err != nil {
		return st, err
	}
	s.statsMu.Lock()
	if s.statsGen == generation {
		s.statsCache, s.statsValid = st, true
	}
	s.statsMu.Unlock()
	return st, nil
}

// invalidateQueueStats is called by every path that changes the repair queue.
// The generation counter is what makes a slow reader unable to store an answer
// that was already out of date when it arrived.
func (s *Store) invalidateQueueStats() {
	s.statsMu.Lock()
	defer s.statsMu.Unlock()
	s.statsGen++
	s.statsValid = false
}

func (s *Store) queryRepairStats() (RepairStats, error) {
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

// PendingRepairCount returns how many items are queued for a peer, due or not.
// Answered from the counter the queue maintains, not by counting rows.
func (s *Store) PendingRepairCount(peerID string) (int64, error) {
	return s.queuedFor(peerID), nil
}
