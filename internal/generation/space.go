package generation

// Upload reservations are conservative: unknown-length streams reserve their
// configured maximum. Metadata retains a free-space margin even under parallel
// uploads. OS ENOSPC and fsync errors remain authoritative and never become ACKs.
func (s *Store) SetReserve(bytes uint64) { s.spaceMu.Lock(); s.minFree = bytes; s.spaceMu.Unlock() }
func (s *Store) reserve(bytes int64) (func(), error) {
	s.spaceMu.Lock()
	defer s.spaceMu.Unlock()
	if s.minFree == 0 {
		return func() {}, nil
	}
	if bytes < 0 {
		return nil, ErrNoSpace
	}
	free, err := availableBytes(s.dir)
	if err != nil {
		return nil, err
	}
	size := uint64(bytes)
	if free < s.minFree || free-s.minFree < s.reserved || free-s.minFree-s.reserved < size {
		return nil, ErrNoSpace
	}
	s.reserved += size
	return func() { s.spaceMu.Lock(); s.reserved -= size; s.spaceMu.Unlock() }, nil
}

type Space struct{ Available, Reserved, Minimum uint64 }

func (s *Store) Space() (Space, error) {
	s.spaceMu.Lock()
	defer s.spaceMu.Unlock()
	free, err := availableBytes(s.dir)
	return Space{free, s.reserved, s.minFree}, err
}
