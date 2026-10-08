package s3

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/birak/birak/internal/store"
)

type measuredCatalog struct {
	*store.Store
	calls, rows int
}

func (m *measuredCatalog) ListLiveRange(ctx context.Context, from, until, after string, limit int) ([]store.FileMeta, error) {
	m.calls++
	page, err := m.Store.ListLiveRange(ctx, from, until, after, limit)
	m.rows += len(page)
	return page, err
}

func TestIndexedListingMatchesFilesystemPages(t *testing.T) {
	g, root := testGateway(t, Config{})
	st, err := store.New(filepath.Join(t.TempDir(), "catalog.db"), g.logger)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	bp := filepath.Join(root, "bucket")
	keys := []string{"a.apk", "a/first.png", "a/nested/icon.png", "b.apk", "café/icon.png", "café.apk", "hidden.tmp"}
	for _, key := range keys {
		p := filepath.Join(bp, filepath.FromSlash(key))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(key), 0o600); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.PutFile("bucket/"+key, info.ModTime().UnixNano(), info.Size(), "hash", false); err != nil {
			t.Fatal(err)
		}
	}
	g.ignorePatterns = append(g.ignorePatterns, "*.tmp")
	g.config.Catalog = st
	for _, prefix := range []string{"", "a", "a/", "café"} {
		for _, delimiter := range []string{"", "/", "é"} {
			objects, prefixes, err := g.collectObjects(bp, prefix, delimiter)
			if err != nil {
				t.Fatal(err)
			}
			for _, after := range []string{"", "a/", "a/first.png", "café/", "z"} {
				for _, limit := range []int{1, 3, 10} {
					wantO, wantP, wantTrunc, wantNext := paginate(objects, prefixes, after, limit)
					gotO, gotP, gotTrunc, gotNext, err := g.listPage(context.Background(), bp, "bucket", prefix, delimiter, after, limit)
					if err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(gotO, wantO) || !reflect.DeepEqual(gotP, wantP) || gotTrunc != wantTrunc || gotNext != wantNext {
						t.Fatalf("prefix=%q delimiter=%q after=%q limit=%d: got %v %v %v %q, want %v %v %v %q", prefix, delimiter, after, limit, gotO, gotP, gotTrunc, gotNext, wantO, wantP, wantTrunc, wantNext)
					}
				}
			}
		}
	}
	// A completed gateway mutation is indexed before ACK. Direct filesystem
	// inputs join LIST after indexing; GET continues to read the filesystem.
	newPath := filepath.Join(bp, "direct.apk")
	if err := os.WriteFile(newPath, []byte("direct"), 0o600); err != nil {
		t.Fatal(err)
	}
	o, _, _, _, err := g.listPage(context.Background(), bp, "bucket", "direct", "", "", 1)
	if err != nil || len(o) != 0 {
		t.Fatalf("unindexed input: %v %v", o, err)
	}
	if _, err := st.PutFile("bucket/direct.apk", time.Now().UnixNano(), 6, "hash", false); err != nil {
		t.Fatal(err)
	}
	o, _, _, _, err = g.listPage(context.Background(), bp, "bucket", "direct", "", "", 1)
	if err != nil || len(o) != 1 || o[0].Key != "direct.apk" {
		t.Fatalf("indexed input: %v %v", o, err)
	}
}

func TestIndexedListingSeeksPastDelimiterGroup(t *testing.T) {
	g, root := testGateway(t, Config{})
	st, err := store.New(filepath.Join(t.TempDir(), "catalog.db"), g.logger)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	bp := filepath.Join(root, "bucket")
	if err := os.MkdirAll(filepath.Join(bp, "a"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bp, "a", "000000"), []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 400; i++ {
		name := fmt.Sprintf("bucket/a/%06d", i)
		if _, err := st.PutFile(name, 1, 1, "hash", false); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(bp, "z.apk"), []byte("last"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PutFile("bucket/z.apk", 1, 4, "hash", false); err != nil {
		t.Fatal(err)
	}
	measured := &measuredCatalog{Store: st}
	g.config.Catalog = measured
	objects, prefixes, truncated, next, err := g.listPage(context.Background(), bp, "bucket", "", "/", "", 10)
	if err != nil || len(objects) != 1 || len(prefixes) != 1 || truncated || next != "" {
		t.Fatalf("page: %v %v %v %q %v", objects, prefixes, truncated, next, err)
	}
	if measured.calls > 3 || measured.rows > 129 {
		t.Fatalf("walked delimiter group: calls=%d rows=%d", measured.calls, measured.rows)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, _, _, err := g.listPage(ctx, bp, "bucket", "", "", "", 10); err == nil {
		t.Fatal("cancelled listing continued")
	}
}
