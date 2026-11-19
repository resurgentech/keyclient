package keylist

import (
	"context"
	"testing"
	"time"

	"github.com/resurgentech/keyclient/keyclienttest"
)

// Each pull is bounded by its own timeout (PullTimeout in production), even
// with a context that never ends and a client without a timeout.
func TestPullTimeoutBoundsEachPull(t *testing.T) {
	l := keyclienttest.NewList(t)
	s := newInternal(t, l, nil)
	s.pullTimeout = 150 * time.Millisecond
	l.Block()
	start := time.Now()
	err := s.Pull(context.Background())
	if err == nil {
		t.Fatal("a pull to a hung provider succeeded")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("the pull took %v; the pull timeout did not bound it", d)
	}
	if s.pullTimeout = 0; PullTimeout != 10*time.Second {
		t.Fatalf("PullTimeout = %v", PullTimeout)
	}
	if n := newInternal(t, l, nil); n.pullTimeout != PullTimeout {
		t.Fatalf("a new Source's pull timeout is %v, want PullTimeout", n.pullTimeout)
	}
}
