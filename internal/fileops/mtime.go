package fileops

// The index records modification times in nanoseconds, as the node that wrote
// a file saw them. A replica's filesystem may keep less: NTFS keeps 100 ns,
// HFS+ and ext3 whole seconds, FAT two seconds, and SMB whatever the server
// does. Compared exactly, such a volume never matched its own index, so every
// sweep re-read and restamped every file, and a peer was never offered a
// resumable range. Comparisons of an on-disk timestamp with a recorded one go
// through SameModTime, at the granularity measured for that volume.

import (
	"fmt"
	"os"
	"time"
)

// modTimeGranularities are the resolutions a probe is rounded up to, finest
// first. A filesystem that keeps something in between is treated as the next
// coarser one, which only ever makes a comparison more forgiving.
var modTimeGranularities = []time.Duration{
	time.Nanosecond,
	100 * time.Nanosecond,
	time.Microsecond,
	time.Millisecond,
	time.Second,
	2 * time.Second,
}

// modTimeProbes are written to the probe file to measure what the filesystem
// keeps. Both have every sub-second digit set; one has an odd and one an even
// second, so a two-second filesystem shows its loss whether it truncates or
// rounds up.
var modTimeProbes = []time.Time{
	time.Unix(1_600_000_001, 123_456_789),
	time.Unix(1_600_000_002, 987_654_321),
}

// DetectModTimeGranularity measures the timestamp resolution of the filesystem
// holding probe, a scratch path that is created and removed again. Whenever
// the measurement fails — including a filesystem that moves a timestamp by
// more than the coarsest supported granularity, which no granularity would
// make comparable — it returns an error and one nanosecond: exact comparison,
// which costs re-reads but never trusts a timestamp too far.
func DetectModTimeGranularity(probe string) (time.Duration, error) {
	if err := os.WriteFile(probe, nil, 0o600); err != nil {
		return time.Nanosecond, err
	}
	defer os.Remove(probe)
	var worst time.Duration
	for _, stamp := range modTimeProbes {
		if err := os.Chtimes(probe, stamp, stamp); err != nil {
			return time.Nanosecond, err
		}
		info, err := os.Stat(probe)
		if err != nil {
			return time.Nanosecond, err
		}
		worst = max(worst, absDuration(info.ModTime().Sub(stamp)))
	}
	for _, granularity := range modTimeGranularities {
		if worst < granularity {
			return granularity, nil
		}
	}
	coarsest := modTimeGranularities[len(modTimeGranularities)-1]
	return time.Nanosecond, fmt.Errorf("filesystem moved a modification time by %v, more than the supported %v", worst, coarsest)
}

// SetModTimeGranularity records the timestamp resolution of the volume at dir.
// Anything below a nanosecond means exact comparison.
func SetModTimeGranularity(dir string, granularity time.Duration) {
	root(dir).mtimeGranularity.Store(int64(max(granularity, time.Nanosecond)))
}

// ModTimeGranularity is the resolution recorded for the volume at dir; a volume
// nobody measured compares exactly.
func ModTimeGranularity(dir string) time.Duration {
	if g := time.Duration(root(dir).mtimeGranularity.Load()); g > 0 {
		return g
	}
	return time.Nanosecond
}

// SameModTime reports whether onDisk, read from the volume at dir, is the
// recorded timestamp as that volume can store it. Filesystems truncate, round
// to nearest, or (FAT) round up, so anything closer than one granularity step
// is the same time.
func SameModTime(dir string, onDisk time.Time, recorded int64) bool {
	granularity := ModTimeGranularity(dir)
	diff := time.Duration(onDisk.UnixNano() - recorded)
	if granularity <= time.Nanosecond {
		return diff == 0
	}
	return absDuration(diff) < granularity
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}
