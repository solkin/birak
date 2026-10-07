package s3

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/birak/birak/internal/quorum"
)

// CleanupQuorum expires idle uploads with an index guard, so a concurrent part
// update wins safely. Closed uploads cannot acquire new parts: every part write
// requires their live upload index. Completion receipts have the same TTL;
// afterwards a retry returns NoSuchUpload and never republishes old content.
func (g *Gateway) CleanupQuorum(ctx context.Context, before time.Time) error {
	n := g.config.Quorum
	records, err := n.View(ctx, "u/", "c/")
	if err != nil {
		return err
	}
	active := map[string]string{}
	for _, rec := range records {
		if strings.HasPrefix(rec.Key, "u/") {
			active[rec.Key[strings.LastIndex(rec.Key, "/")+1:]] = rec.Key
		}
		if rec.Entry.Modified >= before.UnixNano() {
			continue
		}
		_, err := n.TransactOnce(ctx, operationID(), []quorum.Change{{Key: rec.Key, Delete: true}}, []quorum.Condition{{Key: rec.Key, Index: rec.Entry.Index}})
		if err != nil && !errors.Is(err, quorum.ErrCondition) {
			return err
		}
		if err == nil && strings.HasPrefix(rec.Key, "u/") {
			delete(active, rec.Key[strings.LastIndex(rec.Key, "/")+1:])
		}
	}
	// Read uploads and parts at one committed boundary. An upload closed after
	// this read is conservatively retained until the next pass; a new upload
	// created afterwards has no parts in this view. Upload IDs are never reused.
	// CAS protects each part against an unexpected concurrent change.
	records, err = n.View(ctx, "u/", "p/")
	if err != nil {
		return err
	}
	for _, rec := range records {
		if strings.HasPrefix(rec.Key, "u/") {
			active[rec.Key[strings.LastIndex(rec.Key, "/")+1:]] = rec.Key
		}
	}
	var changes []quorum.Change
	var guards []quorum.Condition
	flush := func() error {
		if len(changes) == 0 {
			return nil
		}
		_, err := n.TransactOnce(ctx, operationID(), changes, guards)
		changes = nil
		guards = nil
		if errors.Is(err, quorum.ErrCondition) {
			return nil
		}
		return err
	}
	for _, rec := range records {
		if !strings.HasPrefix(rec.Key, "p/") {
			continue
		}
		parts := strings.Split(rec.Key, "/")
		if len(parts) != 3 || active[parts[1]] != "" {
			continue
		}
		changes = append(changes, quorum.Change{Key: rec.Key, Delete: true})
		guards = append(guards, quorum.Condition{Key: rec.Key, Index: rec.Entry.Index})
		if len(changes) == 64 {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	return flush()
}

func (g *Gateway) RunQuorumCleanup(ctx context.Context, ttl, interval time.Duration) {
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	if interval <= 0 {
		interval = time.Hour
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if g.config.Quorum.Status().State != "Leader" {
				continue
			}
			if err := g.CleanupQuorum(ctx, time.Now().Add(-ttl)); err != nil && ctx.Err() == nil {
				g.logger.Error("quorum multipart cleanup failed", "error", err)
			}
		}
	}
}
