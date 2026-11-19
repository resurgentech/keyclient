package keylist_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/resurgentech/keyclient/keyclienttest"
	"github.com/resurgentech/keyclient/keylist"
)

// Tests named after the rows and invariants of docs/STATES.md, machines 4 and 5.

// TestK5_CacheWrittenBeforeSwap: when the live list is swapped, the cache
// already holds the new body, so a crash between the two never leaves the cache
// older than a list that was live (K-5, K-I2).
func TestK5_CacheWrittenBeforeSwap(t *testing.T) {
	e := newEnv(t)
	e.add("alice@laptop", "agent", "key-alice")
	s := e.source(e.opts())
	var seen []byte
	var readErr error
	keylist.SetSwapHook(s, func() { seen, readErr = os.ReadFile(e.cache()) })
	if err := s.Pull(context.Background()); err != nil {
		t.Fatal(err)
	}
	if readErr != nil || !strings.Contains(string(seen), hashOf("key-alice")) {
		t.Fatalf("at the swap the cache held %q (err %v); want the new body", seen, readErr)
	}
}

// TestK6_FailedWriteStillSwapsAndRetries: a failed cache write still swaps the
// list, leaves the marker unset, so the next pull is a full 200 that writes
// the cache again (K-6).
func TestK6_FailedWriteStillSwapsAndRetries(t *testing.T) {
	e := newEnv(t)
	e.add("alice@laptop", "agent", "key-alice")
	o := e.opts()
	blocker := filepath.Join(e.dir, "blocker")
	writeFile(t, blocker, "") // a directory cannot be made under a file
	o.CacheFile = filepath.Join(blocker, "list.json")
	s := e.source(o)
	if err := s.Pull(context.Background()); !errors.Is(err, keylist.ErrCacheWrite) {
		t.Fatalf("Pull = %v, want ErrCacheWrite", err)
	}
	mustFind(t, s, "key-alice", "alice@laptop", "agent")
	if got := keylist.Etag(s); got != "" {
		t.Fatalf("marker after a failed cache write = %q, want unset", got)
	}
	if err := os.Remove(blocker); err != nil { // the cause is gone
		t.Fatal(err)
	}
	if err := s.Pull(context.Background()); err != nil {
		t.Fatalf("retry: %v", err)
	}
	reqs := e.l.Requests()
	if len(reqs) != 2 || reqs[1].IfNoneMatch != "" {
		t.Fatalf("requests = %+v; the retry must be unconditional", reqs)
	}
	if st := s.Status(); st.CacheFailing || st.LastError != nil {
		t.Fatalf("Status = %+v", st)
	}
	if _, err := os.Stat(o.CacheFile); err != nil {
		t.Fatalf("cache not written by the retry: %v", err)
	}
}

// TestKI1_MarkerUnsetAfterFailedCacheWrite: K-I1 holds after a good write, a
// 304, and a failed write.
func TestKI1_MarkerUnsetAfterFailedCacheWrite(t *testing.T) {
	e := newEnv(t)
	e.add("alice@laptop", "agent", "key-alice")
	s := e.source(e.opts())
	if err := s.Pull(context.Background()); err != nil {
		t.Fatal(err)
	}
	if keylist.Etag(s) == "" {
		t.Fatal("marker unset after a good write")
	}
	if err := keylist.CheckKI1(s); err != nil {
		t.Fatal(err)
	}
	// The cache path becomes a directory: the next write fails.
	if err := os.Remove(e.cache()); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(e.cache(), 0o700); err != nil {
		t.Fatal(err)
	}
	e.add("bob@desktop", "agent", "key-bob")
	if err := s.Pull(context.Background()); !errors.Is(err, keylist.ErrCacheWrite) {
		t.Fatalf("Pull = %v, want ErrCacheWrite", err)
	}
	if err := keylist.CheckKI1(s); err != nil {
		t.Fatal(err)
	}
	if got := keylist.Etag(s); got != "" {
		t.Fatalf("marker = %q after a failed write", got)
	}
}

// TestK13_StartRemovesTempFilesOfThisCache: Start removes the temporary files
// an interrupted write left beside the cache, and only those.
func TestK13_StartRemovesTempFilesOfThisCache(t *testing.T) {
	e := newEnv(t)
	dir := filepath.Dir(e.cache())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	mine := filepath.Join(dir, ".list.json.tmp-12345")
	other := filepath.Join(dir, ".other.json.tmp-12345")
	plain := filepath.Join(dir, "list.json.keep")
	for _, p := range []string{mine, other, plain} {
		writeFile(t, p, "partial")
	}
	s := e.source(e.opts())
	e.begin(s)
	if _, err := os.Lstat(mine); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the temporary file of this cache is still there: %v", err)
	}
	for _, p := range []string{other, plain} {
		if _, err := os.Lstat(p); err != nil {
			t.Fatalf("%s was removed: %v", p, err)
		}
	}
}

// TestK12_ShutdownCutPullIsNotAFailure: a pull cut short because Start's context
// ended is not logged as a failure and does not set LastError (K-12).
func TestK12_ShutdownCutPullIsNotAFailure(t *testing.T) {
	e := newEnv(t)
	e.add("alice@laptop", "agent", "key-alice")
	e.l.Block() // the first pull hangs at the provider
	s := e.source(e.opts())
	cancel := e.begin(s)
	eventually(t, "a pull in flight", func() bool { return e.l.Pulls() >= 1 })
	cancel()
	time.Sleep(300 * time.Millisecond) // the pull is cut short and ends
	if out := e.log.String(); strings.Contains(out, "level=WARN") || strings.Contains(out, "level=ERROR") || strings.Contains(out, "failed") {
		t.Fatalf("the cut pull was logged as a failure:\n%s", out)
	}
	if st := s.Status(); st.LastError != nil {
		t.Fatalf("LastError = %v after a pull cut short by shutdown", st.LastError)
	}
}

// TestR16_MissAfterStopIsAnsweredAtOnce: after Start's context ended, a miss is
// answered from the list at once, starts no pull, logs nothing and leaves
// LastError alone (R-16, R-I6).
func TestR16_MissAfterStopIsAnsweredAtOnce(t *testing.T) {
	e := newEnv(t)
	e.add("alice@laptop", "agent", "key-alice")
	o := e.opts()
	o.RefreshOnMiss = 20 * time.Millisecond
	s := e.source(o)
	cancel := e.begin(s)
	eventually(t, "first pull", func() bool { return e.l.Pulls() >= 1 })
	if err := s.Pull(context.Background()); err != nil { // settle
		t.Fatal(err)
	}
	cancel()
	time.Sleep(50 * time.Millisecond)
	pulls := e.l.Pulls()
	e.log.mu.Lock()
	e.log.b.Reset()
	e.log.mu.Unlock()
	start := time.Now()
	if _, _, ok := s.LookupOrRefresh(context.Background(), "unknown-token"); ok {
		t.Fatal("unknown token accepted")
	}
	mustFind(t, s, "key-alice", "alice@laptop", "agent")
	time.Sleep(100 * time.Millisecond)
	if d := time.Since(start); d > MissWaitSlack {
		t.Fatalf("answered after %v", d)
	}
	if got := e.l.Pulls() - pulls; got != 0 {
		t.Fatalf("%d pulls after the context ended", got)
	}
	if out := e.log.String(); out != "" {
		t.Fatalf("logged after the context ended:\n%s", out)
	}
	if st := s.Status(); st.LastError != nil {
		t.Fatalf("LastError = %v", st.LastError)
	}
}

// MissWaitSlack is well under keylist.MissWait: an answer "at once".
const MissWaitSlack = 1500 * time.Millisecond

// TestRI6_NoPullStartsAfterStop: after the context ended no timer tick, no miss
// and no trailing pull reaches the provider.
func TestRI6_NoPullStartsAfterStop(t *testing.T) {
	e := newEnv(t)
	e.add("alice@laptop", "agent", "key-alice")
	o := e.opts()
	o.Refresh = 20 * time.Millisecond
	o.RefreshOnMiss = 20 * time.Millisecond
	s := e.source(o)
	cancel := e.begin(s)
	eventually(t, "first pull", func() bool { return e.l.Pulls() >= 2 })
	cancel()
	// A pull in flight at the cancel may still land; wait for it.
	time.Sleep(100 * time.Millisecond)
	pulls := e.l.Pulls()
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.LookupOrRefresh(context.Background(), "unknown-token")
		}()
	}
	wg.Wait()
	time.Sleep(200 * time.Millisecond)
	if got := e.l.Pulls() - pulls; got != 0 {
		t.Fatalf("%d pulls reached the provider after the context ended", got)
	}
}

// TestKI7_StatusSaysWhereTheListCameFrom: Status gives the origin of the list
// and its age; a list served from an old cache is never age zero (K-I7).
func TestKI7_StatusSaysWhereTheListCameFrom(t *testing.T) {
	e := newEnv(t)
	e.add("alice@laptop", "agent", "key-alice")
	s := e.source(e.opts())
	if st := s.Status(); st.Origin != keylist.OriginNone || st.Age != 0 || !st.ListTime.IsZero() {
		t.Fatalf("fresh Status = %+v", st)
	}
	if err := s.Pull(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The cache now holds the list; make it 90 days old and restart with the provider down.
	old := time.Now().Add(-90 * 24 * time.Hour)
	if err := os.Chtimes(e.cache(), old, old); err != nil {
		t.Fatal(err)
	}
	e.l.FailWith(keyclienttest.Status(500, ""))
	s2 := e.source(e.opts())
	e.begin(s2)
	eventually(t, "the failing pull", func() bool { return s2.Status().LastError != nil })
	st := s2.Status()
	if st.Origin != keylist.OriginCache || st.Entries != 1 {
		t.Fatalf("Status = %+v, want the cache as origin", st)
	}
	if !st.LastSuccess.IsZero() {
		t.Fatalf("LastSuccess = %v: there was no good pull", st.LastSuccess)
	}
	if st.Age < 89*24*time.Hour || st.Age > 91*24*time.Hour || st.ListTime.Sub(old).Abs() > time.Second {
		t.Fatalf("Age = %v, ListTime = %v; want about 90 days from %v", st.Age, st.ListTime, old)
	}
	// A good pull makes the provider the origin.
	e.l.Recover()
	if err := s2.Pull(context.Background()); err != nil {
		t.Fatal(err)
	}
	st = s2.Status()
	if st.Origin != keylist.OriginProvider || st.Age > time.Minute || !st.ListTime.Equal(st.LastSuccess) {
		t.Fatalf("Status = %+v, want the provider as origin", st)
	}
}

// TestNewInsecureHTTP: plain http is refused unless the host is a loopback
// address or AllowInsecureHTTP is set (no network: New refuses before any I/O).
func TestNewInsecureHTTP(t *testing.T) {
	cases := []struct {
		url      string
		allow    bool
		wantFail bool
	}{
		{"http://provider.example/v1/lists/x", false, true},
		{"http://10.0.0.5/v1/lists/x", false, true},
		{"http://[::2]/x", false, true},
		{"http://localhost.example/x", false, true},
		{"http://127.0.0.1.example/x", false, true},
		{"http://provider.example/v1/lists/x", true, false},
		{"https://provider.example/v1/lists/x", false, false},
		{"http://localhost:8080/x", false, false},
		{"http://LOCALHOST/x", false, false},
		{"http://127.0.0.1:8080/x", false, false},
		{"http://127.9.9.9/x", false, false},
		{"http://[::1]:8080/x", false, false},
		{"http://[::ffff:127.0.0.1]/x", false, false},
	}
	for _, c := range cases {
		_, err := keylist.New(keylist.Options{URL: c.url, CacheFile: "c.json", Accept: acceptAll, AllowInsecureHTTP: c.allow})
		if (err != nil) != c.wantFail {
			t.Errorf("%s allow=%v: err = %v, wantFail %v", c.url, c.allow, err, c.wantFail)
		}
		if err != nil && strings.Contains(err.Error(), "provider.example") {
			t.Errorf("the error echoes the URL: %v", err)
		}
	}
}

// TestRI2_WaitersReturnToZero: the count of waiters is back to zero after the
// waiters time out, after the pull lands, and after the callers' contexts end.
func TestRI2_WaitersReturnToZero(t *testing.T) {
	waitersZero := func(t *testing.T, s *keylist.Source) {
		t.Helper()
		eventually(t, "waiters back to zero", func() bool { return keylist.Waiters(s) == 0 })
	}
	run := func(t *testing.T, n int, s *keylist.Source, ctxFor func(i int) (context.Context, context.CancelFunc)) {
		t.Helper()
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			ctx, cancel := ctxFor(i)
			go func() {
				defer wg.Done()
				defer cancel()
				s.LookupOrRefresh(ctx, "unknown-token")
			}()
		}
		wg.Wait()
	}
	bg := func(int) (context.Context, context.CancelFunc) { return context.WithCancel(context.Background()) }

	t.Run("pull lands", func(t *testing.T) {
		e := newEnv(t)
		o := e.opts()
		o.RefreshOnMiss = time.Hour
		s := e.source(o)
		run(t, 20, s, bg)
		waitersZero(t, s)
	})
	t.Run("waiters time out", func(t *testing.T) {
		e := newEnv(t)
		o := e.opts()
		o.RefreshOnMiss = time.Hour
		s := e.source(o)
		e.l.Block()
		run(t, 20, s, bg) // each leaves after MissWait
		waitersZero(t, s)
	})
	t.Run("callers' contexts end", func(t *testing.T) {
		e := newEnv(t)
		o := e.opts()
		o.RefreshOnMiss = time.Hour
		s := e.source(o)
		e.l.Block()
		run(t, 20, s, func(int) (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), 30*time.Millisecond)
		})
		waitersZero(t, s)
	})
	t.Run("past the cap", func(t *testing.T) {
		e := newEnv(t)
		o := e.opts()
		o.RefreshOnMiss = time.Hour
		s := e.source(o)
		e.l.Block()
		run(t, keylist.MaxMissWaiters+30, s, func(int) (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), 50*time.Millisecond)
		})
		waitersZero(t, s)
	})
}
