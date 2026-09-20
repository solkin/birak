package birak_test

import (
	"os"
	"testing"
)

// TestMain owns what the tests build once and share: the daemon binary the
// crash and backup tests run as a real process.
func TestMain(m *testing.M) {
	code := m.Run()
	removeDaemonBinary()
	os.Exit(code)
}
