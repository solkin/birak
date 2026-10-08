package store

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"
)

func TestLiveRangeBoundsAndCancellation(t *testing.T) {
	s := newTestStore(t)
	for _, name := range []string{"a", "bucket/a", "bucket/b", "bucket/b/child", "bucket/c", "other/a"} {
		if _, err := s.PutFile(name, 1, 1, "hash", false); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.PutFile("bucket/b", 1, 0, "", true); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		from, after string
		want        []string
	}{
		{"bucket/", "", []string{"bucket/a", "bucket/b/child", "bucket/c"}},
		{"bucket/", "bucket/b", []string{"bucket/b/child", "bucket/c"}},
		{"bucket/c", "bucket/a", []string{"bucket/c"}},
	} {
		page, err := s.ListLiveRange(context.Background(), tc.from, "bucket0", tc.after, 100)
		if err != nil || len(page) != len(tc.want) {
			t.Fatalf("range: %v %v", page, err)
		}
		for i, name := range tc.want {
			if page[i].Name != name {
				t.Fatalf("range returned %s, want %s", page[i].Name, name)
			}
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.ListLiveRange(ctx, "bucket/", "bucket0", "", 100); err == nil {
		t.Fatal("cancelled query succeeded")
	}
}

func TestFileCountCacheInvalidatesAfterRecreationAndRollback(t *testing.T) {
	s := newTestStore(t)
	for _, deleted := range []bool{false, true, false} {
		if _, err := s.PutLocal(FileMeta{Name: "file", ModTime: 1, Hash: "hash", Deleted: deleted}); err != nil {
			t.Fatal(err)
		}
		want := int64(1)
		if deleted {
			want = 0
		}
		for range 3 {
			if n, err := s.FileCount(); err != nil || n != want {
				t.Fatalf("count %d %v, want %d", n, err, want)
			}
		}
	}
	if _, err := s.PutLocal(FileMeta{Name: "FILE", Hash: "collision"}); err == nil {
		t.Fatal("collision succeeded")
	}
	if n, err := s.FileCount(); err != nil || n != 1 {
		t.Fatalf("rollback count %d %v", n, err)
	}
}

// Opt-in metadata scale acceptance; does not represent a physical APK dataset.
func TestProductionIndexScale(t *testing.T) {
	raw := os.Getenv("BIRAK_SCALE_NAMES")
	if raw == "" {
		t.Skip("set BIRAK_SCALE_NAMES for index scale acceptance")
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 100000 {
		t.Fatal("BIRAK_SCALE_NAMES must be >=100000")
	}
	s := newTestStore(t)
	started := time.Now()
	_, err = s.db.Exec(`WITH RECURSIVE ids(n) AS (VALUES(0) UNION ALL SELECT n+1 FROM ids WHERE n+1<?)
 INSERT INTO files(name,mod_time,size,hash,deleted,version,clock)
 SELECT 'bucket/'||printf('%09d',n),1,524288000,'hash',0,n+1,1 FROM ids`, n)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetNodeValue("last_version", strconv.Itoa(n)); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.cachedNextVer = int64(n) + 1
	s.mu.Unlock()
	t.Logf("%d synthetic names inserted in %s", n, time.Since(started))
	cursor := fmt.Sprintf("bucket/%09d", n-1001)
	start := time.Now()
	page, err := s.ListLiveRange(context.Background(), "bucket/", "bucket0", cursor, 1000)
	if err != nil || len(page) != 1000 || page[999].Name != fmt.Sprintf("bucket/%09d", n-1) {
		t.Fatalf("tail: len=%d err=%v", len(page), err)
	}
	t.Logf("deep index page 1000: %s", time.Since(start))
	count, err := s.FileCount()
	if err != nil || count != int64(n) {
		t.Fatalf("count: %d %v", count, err)
	}
	start = time.Now()
	for range 1000 {
		if count, err := s.FileCount(); err != nil || count != int64(n) {
			t.Fatalf("cached count: %d %v", count, err)
		}
	}
	t.Logf("1000 warm count reads: %s", time.Since(start))
}
