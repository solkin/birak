package fileops

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestSameModTimeHonoursTheVolumeGranularity(t *testing.T) {
	root := t.TempDir()
	source := time.Unix(100, 600_000_000).UnixNano()
	for _, tc := range []struct {
		granularity time.Duration
		onDisk      time.Time
		want        bool
	}{
		{0, time.Unix(100, 600_000_000), true}, // never measured: exact
		{0, time.Unix(100, 600_000_001), false},
		{time.Nanosecond, time.Unix(100, 0), false},
		{100 * time.Nanosecond, time.Unix(100, 599_999_950), true},
		{time.Second, time.Unix(100, 0), true},     // truncated
		{time.Second, time.Unix(101, 0), true},     // rounded
		{time.Second, time.Unix(99, 0), false},     // a second off is a change
		{2 * time.Second, time.Unix(102, 0), true}, // FAT rounds up
		{2 * time.Second, time.Unix(98, 0), false},
	} {
		if tc.granularity > 0 {
			SetModTimeGranularity(root, tc.granularity)
		}
		if got := SameModTime(root, tc.onDisk, source); got != tc.want {
			t.Errorf("granularity %v: SameModTime(%v)=%v, want %v", tc.granularity, tc.onDisk, got, tc.want)
		}
	}
}

func TestDetectModTimeGranularityOfThisVolume(t *testing.T) {
	probe := filepath.Join(t.TempDir(), "probe")
	granularity, err := DetectModTimeGranularity(probe)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(modTimeGranularities, granularity) {
		t.Fatalf("granularity %v is not one of %v", granularity, modTimeGranularities)
	}
	if _, err := os.Lstat(probe); !os.IsNotExist(err) {
		t.Fatalf("probe left behind: %v", err)
	}
}
