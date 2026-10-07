package quorum

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestBoundedPageDelimiterAndContinuation(t *testing.T) {
	entries := map[string]Entry{}
	for _, key := range []string{"o/b/a", "o/b/dir/a", "o/b/dir/b", "o/b/z", "o/b/other/one", "o/b/other/two", "o/elsewhere"} {
		entries[key] = Entry{MetaOnly: true}
	}
	var keys []string
	after := ""
	for {
		page, more, err := selectPage(context.Background(), entries, "o/b/", after, "/", 1)
		if err != nil || len(page) > 1 {
			t.Fatal(page, err)
		}
		if len(page) > 0 {
			after = page[0].Key
			keys = append(keys, after)
		}
		if !more {
			break
		}
	}
	if strings.Join(keys, ",") != "o/b/a,o/b/dir/,o/b/other/,o/b/z" {
		t.Fatal(keys)
	}
	page, more, err := selectPage(context.Background(), entries, "o/b/", "", "/", 0)
	if err != nil || len(page) != 0 || more {
		t.Fatal(page, more, err)
	}
}
func BenchmarkPageMillionKeys(b *testing.B) {
	entries := make(map[string]Entry, 1_000_000)
	for i := 0; i < 1_000_000; i++ {
		entries[fmt.Sprintf("o/b/%08d", i)] = Entry{Index: uint64(i + 1), MetaOnly: true}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		page, _, err := selectPage(context.Background(), entries, "o/b/", "o/b/00500000", "", 1000)
		if err != nil || len(page) != 1000 {
			b.Fatal(len(page), err)
		}
	}
}
