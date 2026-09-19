package store

import (
	"database/sql"
	"encoding/json"
	"strings"
)

// Local intents identify explicit gateway mutations across a process restart.
// They permit checksum changes even when clients preserve size and timestamps.
func (s *Store) BeginLocal(paths []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, path := range paths {
		if _, err = tx.Exec("INSERT OR IGNORE INTO local_intents(path) VALUES (?)", path); err != nil {
			return err
		}
	}
	return tx.Commit()
}
func (s *Store) LocalIntents() ([]string, error) {
	rows, err := s.db.Query("SELECT path FROM local_intents ORDER BY path")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var paths []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		paths = append(paths, p)
	}
	return paths, rows.Err()
}
func (s *Store) HasLocalIntent(name string) (bool, error) {
	paths, err := s.LocalIntents()
	if err != nil {
		return false, err
	}
	for _, p := range paths {
		if name == p || strings.HasPrefix(name, p+"/") {
			return true, nil
		}
	}
	return false, nil
}
func (s *Store) EndLocal(paths []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, p := range paths {
		if _, err = tx.Exec("DELETE FROM local_intents WHERE path=?", p); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// A replica intent bridges the filesystem/SQLite commit boundary. Recovery can
// recognize already published bytes without inventing a new local mutation.
func (s *Store) StageReplica(meta FileMeta) error {
	data, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err = s.db.Exec("INSERT INTO replica_intents(name,metadata) VALUES (?,?) ON CONFLICT(name) DO UPDATE SET metadata=excluded.metadata", meta.Name, string(data))
	return err
}
func (s *Store) ReplicaIntent(name string) (*FileMeta, error) {
	var data string
	err := s.db.QueryRow("SELECT metadata FROM replica_intents WHERE name=?", name).Scan(&data)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var meta FileMeta
	if err = json.Unmarshal([]byte(data), &meta); err != nil {
		return nil, err
	}
	return &meta, nil
}
func (s *Store) ClearReplicaIntent(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec("DELETE FROM replica_intents WHERE name=?", name)
	return err
}

func (s *Store) ListSubtree(path string) ([]FileMeta, error) {
	rows, err := s.db.Query("SELECT "+fileColumns+" FROM files WHERE name=? OR (name>=? AND name<?)", path, path+"/", path+"0")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []FileMeta
	for rows.Next() {
		meta, err := scanFile(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, meta)
	}
	return result, rows.Err()
}
