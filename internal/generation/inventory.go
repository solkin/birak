package generation

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
)

const InventoryBatch = 4096

type Inventory struct {
	Session string `json:"session"`
	Missing []Ref  `json:"missing"`
}

type durableReceipt struct {
	ref  Ref
	info os.FileInfo
}

// Receipts are process-local. A restart must hash and flush files again before
// certifying them. External writers/GC are forbidden on this private volume.
// An unchanged receipt certifies past durability, not immunity to later bitrot.
func (s *Store) remember(ref Ref) error {
	info, err := os.Lstat(s.path(ref))
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() != ref.Size {
		return ErrCorrupt
	}
	s.proofMu.Lock()
	s.receipts[ref.Hash] = durableReceipt{ref, info}
	s.proofMu.Unlock()
	return nil
}
func (s *Store) forget(ref Ref) {
	s.proofMu.Lock()
	defer s.proofMu.Unlock()
	if _, ok := s.receipts[ref.Hash]; ok {
		delete(s.receipts, ref.Hash)
		s.proofEpoch++
	}
}
func (s *Store) proofSession() string {
	s.proofMu.Lock()
	defer s.proofMu.Unlock()
	return fmt.Sprintf("%s:%d", s.proofID, s.proofEpoch)
}

// Inventory verifies uncached files locally, without transferring them again.
// Callers must compare Session over every batch and recheck the complete final
// boundary after transfers. A changed/missing previously certified file revokes
// the session; a live replaced volume is fenced by checkStorage.
func (s *Store) Inventory(ctx context.Context, refs []Ref) (Inventory, error) {
	if len(refs) > InventoryBatch {
		return Inventory{}, errors.New("inventory batch too large")
	}
	s.life.RLock()
	defer s.life.RUnlock()
	if err := s.checkStorage(); err != nil {
		return Inventory{}, err
	}
	start := s.proofSession()
	out := Inventory{Session: start}
	for _, ref := range refs {
		if err := ref.Validate(); err != nil {
			return Inventory{}, err
		}
		if err := ctx.Err(); err != nil {
			return Inventory{}, err
		}
		s.proofMu.Lock()
		receipt, ok := s.receipts[ref.Hash]
		s.proofMu.Unlock()
		if ok && receipt.ref != ref {
			return Inventory{}, ErrCorrupt
		}
		info, err := os.Lstat(s.path(ref))
		if ok && err == nil && info.Mode().IsRegular() && info.Size() == ref.Size && os.SameFile(info, receipt.info) && info.ModTime() == receipt.info.ModTime() {
			continue
		}
		if ok {
			s.forget(ref)
		}
		// Avoid recursive life.RLock: Close may already be waiting on its writer lock.
		if err := s.verifyDurable(ctx, ref); err != nil {
			if !os.IsNotExist(err) && !errors.Is(err, ErrCorrupt) {
				return Inventory{}, err
			}
			out.Missing = append(out.Missing, ref)
		}
	}
	if err := s.checkStorage(); err != nil {
		return Inventory{}, err
	}
	if start != s.proofSession() {
		return Inventory{}, errors.New("generation certification changed during inventory; retry catch-up")
	}
	return out, nil
}

func newProofID() string { return rand.Text() }
