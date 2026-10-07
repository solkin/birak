package quorum

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/birak/birak/internal/generation"
)

type scrubProgress struct {
	Failed    bool
	After     string
	Completed int64
}

func (n *Node) loadScrubProgress() (scrubProgress, error) {
	var p scrubProgress
	b, err := os.ReadFile(filepath.Join(n.dir, "scrub-progress.json"))
	if os.IsNotExist(err) {
		return p, nil
	}
	if err != nil {
		return p, err
	}
	if json.Unmarshal(b, &p) != nil || (p.After != "" && (generation.Ref{Hash: p.After}).Validate() != nil) || p.Completed < 0 {
		return scrubProgress{}, errors.New("invalid scrub progress")
	}
	return p, nil
}
func (n *Node) saveScrubProgress(p scrubProgress) error {
	if err := n.objects.CheckStorage(); err != nil {
		return err
	}
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return replaceStateFile(filepath.Join(n.dir, "scrub-progress.json"), b)
}
