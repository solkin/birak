package store

import (
	"cmp"
	"fmt"
	"math"
	"strconv"
)

// BigClock extends the original int64 clock only when it overflows. Its decimal
// representation has no integer ceiling. Clock remains MaxInt64 as a projection
// for local APIs; protocol 4 prevents old nodes from discarding the extension.
func ValidateClock(f FileMeta) error {
	if f.BigClock == "" {
		return nil
	}
	if f.Clock != math.MaxInt64 || len(f.BigClock) < 19 || f.BigClock[0] == '0' {
		return fmt.Errorf("non-canonical extended clock")
	}
	for _, c := range f.BigClock {
		if c < '0' || c > '9' {
			return fmt.Errorf("invalid extended clock")
		}
	}
	if compareDecimal(f.BigClock, strconv.FormatInt(math.MaxInt64, 10)) <= 0 {
		return fmt.Errorf("extended clock must exceed int64")
	}
	return nil
}

func compareDecimal(a, b string) int {
	if c := cmp.Compare(len(a), len(b)); c != 0 {
		return c
	}
	return cmp.Compare(a, b)
}

func CompareClock(a, b FileMeta) int {
	if a.BigClock != "" && b.BigClock != "" {
		return compareDecimal(a.BigClock, b.BigClock)
	}
	if a.BigClock != "" {
		return 1
	}
	if b.BigClock != "" {
		return -1
	}
	return cmp.Compare(a.StateClock(), b.StateClock())
}

// AdvanceClock chooses a positive clock above the observed state, preserving
// any larger incoming clock. No remote value can exhaust the next local write.
func AdvanceClock(f *FileMeta, previous *FileMeta) {
	if f.BigClock == "" {
		f.Clock = max(1, f.Clock, f.ModTime)
	}
	if previous == nil || CompareClock(*f, *previous) > 0 {
		return
	}
	if previous.BigClock == "" && previous.StateClock() < math.MaxInt64 {
		f.Clock, f.BigClock = previous.StateClock()+1, ""
		return
	}
	digits := previous.BigClock
	if digits == "" {
		digits = strconv.FormatInt(previous.StateClock(), 10)
	}
	b := []byte(digits)
	carry := true
	for i := len(b) - 1; i >= 0 && carry; i-- {
		if b[i] == '9' {
			b[i] = '0'
		} else {
			b[i]++
			carry = false
		}
	}
	if carry {
		b = append([]byte{'1'}, b...)
	}
	f.Clock, f.BigClock = math.MaxInt64, string(b)
}
