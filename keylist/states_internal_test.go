package keylist

import (
	"context"
	"testing"
	"time"

	"github.com/resurgentech/keyclient/keyclienttest"
)

// TestR4_NoTimerArmedWhenWindowAlreadyOver: a miss refused because 100 callers
// already wait, when the window of the in-flight pull is already over, arms no
// trailing pull (R-4). When the window is still closed, one is armed.
func TestR4_NoTimerArmedWhenWindowAlreadyOver(t *testing.T) {
	for _, c := range []struct {
		name  string
		ago   time.Duration
		armed bool
	}{
		{"window over", 2 * time.Minute, false},
		{"window closed", 10 * time.Second, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			l := keyclienttest.NewList(t)
			s := newInternal(t, l, func(o *Options) { o.RefreshOnMiss = time.Minute })
			SetMissState(s, MaxMissWaiters, time.Now().Add(-c.ago))
			defer EndMissState(s)
			s.LookupOrRefresh(context.Background(), "unknown")
			if got := TrailingArmed(s); got != c.armed {
				t.Fatalf("trailing pull armed = %v, want %v", got, c.armed)
			}
			if got := Waiters(s); got != MaxMissWaiters {
				t.Fatalf("waiters = %d: a refused miss must not change the count", got)
			}
		})
	}
}
