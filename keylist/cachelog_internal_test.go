package keylist

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/resurgentech/keyclient/keyclienttest"
)

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (w *lockedBuf) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (w *lockedBuf) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

// K-6, K-7: while cache writes keep failing, the failure is logged once for the
// streak, and the recovery once; not on every refresh.
func TestK6_CacheWriteFailureLoggedOncePerStreak(t *testing.T) {
	l := keyclienttest.NewList(t)
	l.Add("alice@laptop", "agent", "key-alice-0123456789")
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var logs lockedBuf
	s := newInternal(t, l, func(o *Options) {
		o.CacheFile = filepath.Join(blocker, "list.json") // a file where a directory should be
		o.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	})
	for i := 0; i < 4; i++ {
		if err := s.Pull(context.Background()); !errors.Is(err, ErrCacheWrite) {
			t.Fatalf("pull %d: %v, want ErrCacheWrite", i, err)
		}
	}
	if n := strings.Count(logs.String(), "key list cache not written"); n != 1 {
		t.Fatalf("four failing cache writes logged %d times, want once:\n%s", n, logs.String())
	}
	// The cache becomes writable: the recovery is logged once.
	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := s.Pull(context.Background()); err != nil {
			t.Fatalf("pull after recovery: %v", err)
		}
	}
	if n := strings.Count(logs.String(), "key list cache written again"); n != 1 {
		t.Fatalf("recovery logged %d times, want once:\n%s", n, logs.String())
	}
	// A new streak is logged again.
	if err := os.Remove(filepath.Join(blocker, "list.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	l.Add("bob@desk", "agent", "key-bob-0123456789ab") // a changed list, so the pull is a 200
	if err := s.Pull(context.Background()); !errors.Is(err, ErrCacheWrite) {
		t.Fatalf("pull: %v, want ErrCacheWrite", err)
	}
	if n := strings.Count(logs.String(), "key list cache not written"); n != 2 {
		t.Fatalf("a second streak: logged %d times in all, want 2", n)
	}
}
