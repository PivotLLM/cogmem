// cogmem - Cognitive Memory
// License: MIT

package store

import (
	"testing"
	"time"
)

// TestDomainLastActive covers the accessor: unset reports ok=false; a unix-seconds
// value round-trips.
func TestDomainLastActive(t *testing.T) {
	if _, ok := (Domain{LastActiveAt: 0}).LastActive(); ok {
		t.Error("unset last_active_at should report ok=false")
	}

	secs := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC).Unix()
	got, ok := (Domain{LastActiveAt: secs}).LastActive()
	if !ok || !got.Equal(time.Unix(secs, 0)) {
		t.Errorf("seconds value: got %v ok=%v", got, ok)
	}
}
