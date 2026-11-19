package keylist

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/resurgentech/keyclient/keyclienttest"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newInternal(t *testing.T, l *keyclienttest.List, mod func(*Options)) *Source {
	t.Helper()
	o := Options{
		URL:       l.URL(),
		CacheFile: filepath.Join(t.TempDir(), "list.json"),
		Refresh:   time.Hour,
		Accept:    func(Entry) (bool, string) { return true, "" },
		Logger:    quiet(),
	}
	if mod != nil {
		mod(&o)
	}
	s, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func burst(s *Source, n int, tag string) {
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.LookupOrRefresh(context.Background(), fmt.Sprintf("%s-%d", tag, i))
		}()
	}
	wg.Wait()
}

// TestThrottleWindowWithControlledClock is the second half of requirement 2:
// one pull per window, however many misses, and one more once it has passed.
func TestThrottleWindowWithControlledClock(t *testing.T) {
	l := keyclienttest.NewList(t)
	s := newInternal(t, l, func(o *Options) { o.RefreshOnMiss = time.Minute })
	var offset atomic.Int64
	base := time.Now()
	s.now = func() time.Time { return base.Add(time.Duration(offset.Load())) }
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); s.Pull(ctx) }) // waits for a pull in flight
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	for l.Pulls() < 1 {
		time.Sleep(time.Millisecond)
	}
	if err := s.Pull(ctx); err != nil { // settle the first pull
		t.Fatal(err)
	}
	n := l.Pulls()

	burst(s, 1000, "a")
	if got := l.Pulls() - n; got != 1 {
		t.Fatalf("first burst: %d pulls, want 1", got)
	}
	offset.Store(int64(59 * time.Second))
	burst(s, 1000, "b")
	if got := l.Pulls() - n; got != 1 {
		t.Fatalf("burst inside the window: %d pulls, want 1", got)
	}
	offset.Store(int64(61 * time.Second))
	burst(s, 1000, "c")
	if got := l.Pulls() - n; got != 2 {
		t.Fatalf("burst after the window: %d pulls in all, want 2", got)
	}
}

// TestMissPathNeverMovesTheTimer is requirement 6: with a 300 ms refresh and a
// 50 ms miss window, spamming misses for 1.2 s leaves the timer's pulls where
// they were: the first at Start, then one per 300 ms.
func TestMissPathNeverMovesTheTimer(t *testing.T) {
	l := keyclienttest.NewList(t)
	s := newInternal(t, l, func(o *Options) {
		o.Refresh = 300 * time.Millisecond
		o.RefreshOnMiss = 50 * time.Millisecond
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); s.Pull(ctx) }) // waits for a pull in flight
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	stop := time.Now().Add(1200 * time.Millisecond)
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; time.Now().Before(stop); i++ {
				s.LookupOrRefresh(ctx, fmt.Sprintf("bad-%d-%d", g, i))
				time.Sleep(time.Millisecond)
			}
		}()
	}
	wg.Wait()
	// Ticks at 300, 600, 900, 1200 ms plus the first pull: 4 or 5, never fewer
	// than 3, and nothing extra from the miss path.
	if n := s.timerPulls.Load(); n < 3 || n > 5 {
		t.Fatalf("timer pulls = %d, want 3 to 5", n)
	}
	if l.Pulls() <= int(s.timerPulls.Load()) {
		t.Fatalf("no miss pulls happened (%d pulls, %d timer)", l.Pulls(), s.timerPulls.Load())
	}
}

// TestLookupComparesEveryEntry is requirement 12: a hit on the first of N
// entries still compares N times, and a miss compares N times.
func TestLookupComparesEveryEntry(t *testing.T) {
	const n = 20
	l := keyclienttest.NewList(t)
	var entries []keyclienttest.Entry
	for i := 0; i < n; i++ {
		sum := sha256.Sum256([]byte(fmt.Sprintf("key-%02d", i)))
		entries = append(entries, keyclienttest.Entry{Name: fmt.Sprintf("n%02d", i), Role: "agent", SHA256: hex.EncodeToString(sum[:])})
	}
	l.Set(entries...)
	s := newInternal(t, l, nil)
	if err := s.Pull(context.Background()); err != nil {
		t.Fatal(err)
	}
	var calls int
	s.compare = func(a, b []byte) int { calls++; return subtle.ConstantTimeCompare(a, b) }

	if name, _, ok := s.Lookup("key-00"); !ok || name != "n00" {
		t.Fatalf("first entry not found: %q %v", name, ok)
	}
	if calls != n {
		t.Fatalf("hit on the first entry made %d comparisons, want %d", calls, n)
	}
	calls = 0
	if _, _, ok := s.Lookup("key-19"); !ok || calls != n {
		t.Fatalf("hit on the last entry: ok=%v, %d comparisons", ok, calls)
	}
	calls = 0
	if _, _, ok := s.Lookup("nope"); ok || calls != n {
		t.Fatalf("miss: ok=%v, %d comparisons", ok, calls)
	}
	calls = 0
	if _, _, ok := s.Lookup(""); ok || calls != 0 {
		t.Fatalf("empty key: ok=%v, %d comparisons", ok, calls)
	}
}
