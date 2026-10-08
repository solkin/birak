package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"path"
	"strings"
)

// A local replacement observes both the old file and the old tree. Raising its
// clock above that namespace preserves explicit file <-> directory operations
// even when their filesystem mtime is older than a displaced child's mtime.
func namespaceClock(tx *sql.Tx, name string) (*FileMeta, error) {
	var args []any
	var placeholders []string
	for p := name; ; {
		args = append(args, p)
		placeholders = append(placeholders, "?")
		parent := path.Dir(p)
		if parent == "." || parent == p {
			break
		}
		p = parent
	}
	// '/' is immediately before '0': this range selects exactly descendants
	// and uses the name index instead of a full-table prefix expression.
	args = append(args, name+"/", name+"0")
	where := "name IN (" + strings.Join(placeholders, ",") + ") OR (name>=? AND name<?)"
	var clock sql.NullInt64
	if err := tx.QueryRow("SELECT MAX(CASE WHEN clock=0 THEN mod_time ELSE clock END) FROM files WHERE "+where, args...).Scan(&clock); err != nil {
		return nil, err
	}
	if !clock.Valid {
		return nil, nil
	}
	out := &FileMeta{Clock: clock.Int64}
	err := tx.QueryRow("SELECT big_clock FROM files WHERE ("+where+") AND big_clock<>'' ORDER BY length(big_clock) DESC, big_clock DESC LIMIT 1", args...).Scan(&out.BigClock)
	if err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	return out, nil
}

// NamespaceConflict is a strict ancestor/descendant relation, not a same-name
// overwrite. Names are canonical slash-separated names from the sync namespace.
func NamespaceConflict(a, b string) bool {
	return strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}

// Superseded records the winner's existing rank exactly. Adding one to its clock
// would allow two concurrent resolutions to delete both live candidates.
func Superseded(name string, winner FileMeta) FileMeta {
	return FileMeta{Name: name, Deleted: true, Clock: winner.StateClock(), BigClock: winner.BigClock, ModTime: winner.ModTime, Hash: winner.Hash, SupersededBy: winner.Name}
}

// ConflictCopyName is outside the original tree and bounded even for long or
// deeply nested names. Equal content at the same original name shares a copy.
func ConflictCopyName(name, hash string) string {
	data, _ := json.Marshal([2]string{name, hash})
	sum := sha256.Sum256(data)
	return "birak-conflict-" + hex.EncodeToString(sum[:])
}

func ConflictCopy(meta FileMeta) FileMeta {
	return FileMeta{Name: ConflictCopyName(meta.Name, meta.Hash), ModTime: meta.ModTime, Clock: meta.StateClock(), BigClock: meta.BigClock, Size: meta.Size, Hash: meta.Hash, ConflictOf: meta.Name}
}
