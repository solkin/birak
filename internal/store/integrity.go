package store

// Quarantine is local durable state, independent of the replicated version.
// Equal metadata must never be taken as proof that quarantined bytes are good.

func (s *Store) loadQuarantine() error {
	rows, err := s.db.Query("SELECT name FROM quarantine")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		s.quarantined[name] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	objects, err := s.db.Query("SELECT identity FROM damaged_objects")
	if err != nil {
		return err
	}
	defer objects.Close()
	for objects.Next() {
		var key string
		if err := objects.Scan(&key); err != nil {
			return err
		}
		s.damagedObjects[key] = true
	}
	return objects.Err()
}

func (s *Store) IsDamagedObject(key string) bool {
	s.integrityMu.RLock()
	defer s.integrityMu.RUnlock()
	return s.damagedObjects[key]
}

func (s *Store) IsQuarantined(name string) bool {
	s.integrityMu.RLock()
	defer s.integrityMu.RUnlock()
	return s.quarantined[name]
}

func (s *Store) QuarantinedNames() []string {
	s.integrityMu.RLock()
	defer s.integrityMu.RUnlock()
	names := make([]string, 0, len(s.quarantined))
	for name := range s.quarantined {
		names = append(names, name)
	}
	return names
}

func (s *Store) QuarantineCount() int {
	s.integrityMu.RLock()
	defer s.integrityMu.RUnlock()
	return len(s.quarantined)
}

func (s *Store) SetQuarantined(name, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Fence reads even if the durable write fails. Return that failure so the
	// caller does not report successful persistence or discard repair work.
	s.integrityMu.Lock()
	s.quarantined[name] = true
	s.damagedObjects[key] = true
	s.integrityMu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("INSERT INTO quarantine(name) VALUES (?) ON CONFLICT DO NOTHING", name); err != nil {
		return err
	}
	if _, err := tx.Exec("INSERT INTO damaged_objects(identity) VALUES (?) ON CONFLICT DO NOTHING", key); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ClearQuarantine(name, verifiedKey string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("DELETE FROM quarantine WHERE name=?", name); err != nil {
		return err
	}
	if verifiedKey != "" {
		if _, err := tx.Exec("DELETE FROM damaged_objects WHERE identity=?", verifiedKey); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.integrityMu.Lock()
	delete(s.quarantined, name)
	delete(s.damagedObjects, verifiedKey)
	s.integrityMu.Unlock()
	return nil
}
