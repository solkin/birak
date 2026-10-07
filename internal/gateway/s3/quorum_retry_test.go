package s3

import (
	"errors"
	"testing"

	"github.com/birak/birak/internal/quorum"
)

func TestPartCASExhaustionCannotAcknowledgeRefresh(t *testing.T) {
	calls := 0
	err := retryPartCommit(func() error { calls++; return quorum.ErrCondition }, func() error { return nil })
	if !errors.Is(err, quorum.ErrCondition) || calls != 16 {
		t.Fatal("refresh acknowledged an uncommitted part", calls, err)
	}
	calls = 0
	err = retryPartCommit(func() error {
		calls++
		if calls == 3 {
			return nil
		}
		return quorum.ErrCondition
	}, func() error { return nil })
	if err != nil || calls != 3 {
		t.Fatal("committed retry lost", calls, err)
	}
	diskError := errors.New("disk")
	if err := retryPartCommit(func() error { return diskError }, func() error { t.Fatal("refreshed non-conflict"); return nil }); err != diskError {
		t.Fatal(err)
	}
	if err := retryPartCommit(func() error { return quorum.ErrCondition }, func() error { return diskError }); err != diskError {
		t.Fatal(err)
	}
}
