package generation

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"
)

type Collection struct{ Files, Bytes, Pinned, Recent int64 }

// Collect may only be invoked under a consensus publication freeze. It never
// follows links, never removes a live reference or reader, and flushes removal
// before reporting completion. A partially completed sweep is safe to repeat.
func (s *Store) Collect(ctx context.Context, keep map[string]Ref, before time.Time) (Collection, error) {
	var out Collection
	s.life.RLock()
	defer s.life.RUnlock()
	for !s.mu.TryLock() {
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return out, ctx.Err()
		case <-timer.C:
		}
	}
	defer s.mu.Unlock()
	if err := s.checkStorage(); err != nil {
		return out, err
	}
	for prefix := 0; prefix < 256; prefix++ {
		dir := filepath.Join(s.dir, "objects", hexByte(byte(prefix)))
		info, err := os.Lstat(dir)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return out, err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return out, ErrCorrupt
		}
		f, err := os.Open(dir)
		if err != nil {
			return out, err
		}
		changed := false
		err = func() error {
			defer f.Close()
			for {
				entries, err := f.ReadDir(256)
				if err != nil && err != io.EOF {
					return err
				}
				for _, entry := range entries {
					if err := ctx.Err(); err != nil {
						return err
					}
					ref := Ref{Hash: hexByte(byte(prefix)) + entry.Name()}
					if ref.Validate() != nil {
						continue
					}
					if _, ok := keep[ref.Hash]; ok {
						continue
					}
					info, err := entry.Info()
					if err != nil {
						return err
					}
					if !info.Mode().IsRegular() {
						return ErrCorrupt
					}
					if !info.ModTime().Before(before) {
						out.Recent++
						continue
					}
					if s.pinned(ref.Hash) {
						out.Pinned++
						continue
					}
					if err := s.checkStorage(); err != nil {
						return err
					}
					if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil {
						return err
					}
					changed = true
					s.forget(ref)
					out.Files++
					out.Bytes += info.Size()
				}
				if err == io.EOF {
					return nil
				}
			}
		}()
		if changed {
			err = errors.Join(err, s.syncDir(dir))
		}
		if err != nil {
			return out, err
		}
	}
	return out, s.checkStorage()
}
func hexByte(b byte) string {
	const digits = "0123456789abcdef"
	return string([]byte{digits[b>>4], digits[b&15]})
}
