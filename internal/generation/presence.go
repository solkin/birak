package generation

import (
	"context"
	"os"
)

// Present is a cheap scheduling hint, never a durability or integrity proof.
// Scrub hashes existing files; missing/obviously damaged files can be repaired
// promptly without waiting for a multi-terabyte integrity scan to finish.
func (s *Store) Present(ctx context.Context, ref Ref) (bool, error) {
	s.life.RLock()
	defer s.life.RUnlock()
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := ref.Validate(); err != nil {
		return false, err
	}
	if err := s.checkStorage(); err != nil {
		return false, err
	}
	info, err := os.Lstat(s.path(ref))
	if os.IsNotExist(err) {
		s.forget(ref)
		s.revoke(ref)
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() || info.Size() != ref.Size {
		s.forget(ref)
		s.revoke(ref)
		return false, nil
	}
	return true, nil
}
