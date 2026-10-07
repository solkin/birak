package generation

import "sync"

// PinReferences protects a backup boundary without keeping one descriptor per
// object. It does not certify existence: export must verify every byte and seal
// the archive only after all objects are read successfully.
func (s *Store) PinReferences(refs map[string]Ref) func() {
	s.mu.Lock()
	s.readersMu.Lock()
	for hash := range refs {
		s.backupPins[hash]++
	}
	s.readersMu.Unlock()
	s.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.readersMu.Lock()
			defer s.readersMu.Unlock()
			for hash := range refs {
				s.backupPins[hash]--
				if s.backupPins[hash] == 0 {
					delete(s.backupPins, hash)
				}
			}
		})
	}
}
