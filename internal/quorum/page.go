package quorum

import (
	"container/heap"
	"context"
	"errors"
	"sort"
	"strings"
)

type ListItem struct {
	Key          string
	Entry        Entry
	CommonPrefix bool
}
type pageHeap []ListItem

func (h pageHeap) Len() int           { return len(h) }
func (h pageHeap) Less(i, j int) bool { return h[i].Key > h[j].Key }
func (h pageHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *pageHeap) Push(v any)        { *h = append(*h, v.(ListItem)) }
func (h *pageHeap) Pop() any          { old := *h; n := len(old); v := old[n-1]; *h = old[:n-1]; return v }

// ListPage bounds response/scratch memory by the requested page size, including
// delimiter groups. The current map index still requires an O(total keys) scan;
// it does not copy or sort the whole bucket to return a thousand results.
func (n *Node) ListPage(ctx context.Context, prefix, after, delimiter string, limit int) ([]ListItem, bool, error) {
	if limit < 0 || limit > 1000 {
		return nil, false, errors.New("invalid page size")
	}
	ctx, cancel := context.WithTimeout(ctx, n.timeout)
	defer cancel()
	if err := n.barrier(ctx); err != nil {
		return nil, false, err
	}
	n.fsm.mu.RLock()
	defer n.fsm.mu.RUnlock()
	return selectPage(ctx, n.fsm.state.Entries, prefix, after, delimiter, limit)
}
func selectPage(ctx context.Context, entries map[string]Entry, prefix, after, delimiter string, limit int) ([]ListItem, bool, error) {
	if limit == 0 {
		return nil, false, nil
	}
	h := pageHeap{}
	keys := map[string]bool{}
	for key, e := range entries {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		if e.Deleted || !strings.HasPrefix(key, prefix) {
			continue
		}
		item := ListItem{Key: key, Entry: e}
		if delimiter != "" {
			if i := strings.Index(strings.TrimPrefix(key, prefix), delimiter); i >= 0 {
				item.Key = key[:len(prefix)+i+len(delimiter)]
				item.Entry = Entry{}
				item.CommonPrefix = true
			}
		}
		if item.Key <= after || keys[item.Key] {
			continue
		}
		if len(h) == limit+1 {
			if item.Key >= h[0].Key {
				continue
			}
			delete(keys, heap.Pop(&h).(ListItem).Key)
		}
		keys[item.Key] = true
		heap.Push(&h, item)
	}
	more := len(h) > limit
	if more {
		heap.Pop(&h)
	}
	sort.Slice(h, func(i, j int) bool { return h[i].Key < h[j].Key })
	return []ListItem(h), more, nil
}
