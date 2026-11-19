package keylist_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/resurgentech/keyclient/keyclienttest"
	"github.com/resurgentech/keyclient/keylist"
)

// syncBuf is a log sink safe for the goroutines of a Source.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func acceptAll(keylist.Entry) (bool, string) { return true, "" }

func hashOf(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// env is a fake list, a captured log and a scratch directory. At the end of the
// test it checks that the whole log holds none of the forbidden strings (every
// hash, token and presented key the test registered).
type env struct {
	t         *testing.T
	l         *keyclienttest.List
	log       *syncBuf
	dir       string
	forbidden []string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, l: keyclienttest.NewList(t), log: &syncBuf{}, dir: t.TempDir()}
	t.Cleanup(func() {
		out := e.log.String()
		for _, f := range e.forbidden {
			if strings.Contains(out, f) {
				t.Errorf("log holds forbidden string %q:\n%s", f, out)
			}
		}
	})
	return e
}

// secret registers a key: neither it nor its hash may appear in the log.
func (e *env) secret(key string) { e.forbidden = append(e.forbidden, key, hashOf(key)) }

// add adds an entry to the fake list and registers its key as secret.
func (e *env) add(name, role, key string) {
	e.secret(key)
	e.l.Add(name, role, key)
}

func (e *env) cache() string { return filepath.Join(e.dir, "cache", "list.json") }

func (e *env) opts() keylist.Options {
	return keylist.Options{
		URL:       e.l.URL(),
		CacheFile: e.cache(),
		Refresh:   time.Hour,
		Accept:    acceptAll,
		Logger:    slog.New(slog.NewTextHandler(e.log, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
}

func (e *env) source(o keylist.Options) *keylist.Source {
	e.t.Helper()
	s, err := keylist.New(o)
	if err != nil {
		e.t.Fatal(err)
	}
	return s
}

// begin starts s. At the end of the test its context is cancelled and a pull
// with that context is made, which waits (the pulls are serialised) for any
// pull in flight, so none writes into the scratch directory while it is removed.
func (e *env) begin(s *keylist.Source) context.CancelFunc {
	e.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	e.t.Cleanup(func() {
		cancel()
		s.Pull(ctx)
	})
	if err := s.Start(ctx); err != nil {
		e.t.Fatal(err)
	}
	return cancel
}

// start starts s and waits until its first pull has landed.
func (e *env) start(s *keylist.Source) {
	e.t.Helper()
	before := e.l.Pulls()
	e.begin(s)
	eventually(e.t, "first pull", func() bool { return e.l.Pulls() > before })
	// A pull is applied just after it is received; a no-op pull serialises.
	if err := s.Pull(context.Background()); err != nil {
		e.t.Fatalf("settling pull: %v", err)
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func mustFind(t *testing.T, s *keylist.Source, key, wantName, wantRole string) {
	t.Helper()
	name, role, ok := s.Lookup(key)
	if !ok || name != wantName || role != wantRole {
		t.Fatalf("Lookup(%q) = %q, %q, %v; want %q, %q, true", key, name, role, ok, wantName, wantRole)
	}
}

func mustMiss(t *testing.T, s *keylist.Source, key string) {
	t.Helper()
	if name, role, ok := s.Lookup(key); ok {
		t.Fatalf("Lookup(%q) = %q, %q, true; want a miss", key, name, role)
	}
}

// TestRefreshOnMissAcceptsNewKeyOnFirstUse is requirement 1.
func TestRefreshOnMissAcceptsNewKeyOnFirstUse(t *testing.T) {
	e := newEnv(t)
	e.add("alice@laptop", "agent", "key-alice")
	o := e.opts()
	o.RefreshOnMiss = keylist.DefaultRefreshOnMiss
	s := e.source(o)
	e.start(s)
	pulls := e.l.Pulls()
	mustFind(t, s, "key-alice", "alice@laptop", "agent")

	e.add("bob@desktop", "full", "key-bob")
	mustMiss(t, s, "key-bob") // plain Lookup does not pull
	name, role, ok := s.LookupOrRefresh(context.Background(), "key-bob")
	if !ok || name != "bob@desktop" || role != "full" {
		t.Fatalf("LookupOrRefresh = %q, %q, %v", name, role, ok)
	}
	if got := e.l.Pulls() - pulls; got != 1 {
		t.Fatalf("extra pulls = %d, want 1", got)
	}
	reqs := e.l.Requests()
	if reqs[0].IfNoneMatch != "" {
		t.Errorf("first pull sent If-None-Match %q", reqs[0].IfNoneMatch)
	}
	if last := reqs[len(reqs)-1]; last.IfNoneMatch == "" {
		t.Errorf("miss pull sent no If-None-Match")
	}
}

func TestRefreshOnMissOffIsLookup(t *testing.T) {
	e := newEnv(t)
	s := e.source(e.opts()) // RefreshOnMiss zero
	e.start(s)
	pulls := e.l.Pulls()
	e.add("bob@desktop", "full", "key-bob")
	if _, _, ok := s.LookupOrRefresh(context.Background(), "key-bob"); ok {
		t.Fatal("refresh on miss off still found a key that is not pulled")
	}
	if e.l.Pulls() != pulls {
		t.Fatal("pulled with refresh on miss off")
	}
}

func TestEmptyPresentedNeverPulls(t *testing.T) {
	e := newEnv(t)
	o := e.opts()
	o.RefreshOnMiss = time.Millisecond
	s := e.source(o)
	e.start(s)
	pulls := e.l.Pulls()
	if _, _, ok := s.LookupOrRefresh(context.Background(), ""); ok {
		t.Fatal("empty key matched")
	}
	if e.l.Pulls() != pulls {
		t.Fatal("an empty key caused a pull")
	}
	// Even a list holding the hash of the empty string must not match it.
	e.l.Set(keyclienttest.Entry{Name: "empty", Role: "agent", SHA256: hashOf("")})
	if err := s.Pull(context.Background()); err != nil {
		t.Fatal(err)
	}
	mustMiss(t, s, "")
}

// TestBurstOfBadTokensCausesOnePull is requirement 2 (the window half, with a
// controlled clock, is in internal_test.go).
func TestBurstOfBadTokensCausesOnePull(t *testing.T) {
	e := newEnv(t)
	e.add("alice@laptop", "agent", "key-alice")
	o := e.opts()
	o.RefreshOnMiss = time.Hour
	s := e.source(o)
	e.start(s)
	pulls := e.l.Pulls()

	var wg sync.WaitGroup
	for i := 0; i < 1000; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, ok := s.LookupOrRefresh(context.Background(), fmt.Sprintf("bad-%d", i)); ok {
				t.Error("a bad token matched")
			}
		}()
	}
	wg.Wait()
	if got := e.l.Pulls() - pulls; got != 1 {
		t.Fatalf("a burst of 1000 bad tokens caused %d pulls, want 1", got)
	}
}

// TestThrottledMissReturnsAtOnce is requirement 3.
func TestThrottledMissReturnsAtOnce(t *testing.T) {
	e := newEnv(t)
	o := e.opts()
	o.RefreshOnMiss = time.Hour
	s := e.source(o)
	e.start(s)
	s.LookupOrRefresh(context.Background(), "bad-1") // completes a miss pull
	pulls := e.l.Pulls()
	e.l.Block() // any pull from here hangs

	start := time.Now()
	_, _, ok := s.LookupOrRefresh(context.Background(), "bad-2")
	if ok {
		t.Fatal("bad token matched")
	}
	if d := time.Since(start); d > 50*time.Millisecond {
		t.Fatalf("throttled miss took %v", d)
	}
	if e.l.Pulls() != pulls {
		t.Fatal("throttled miss started a pull")
	}
}

// TestJoinersWaitAtMostMissWait is requirement 4.
func TestJoinersWaitAtMostMissWait(t *testing.T) {
	e := newEnv(t)
	o := e.opts()
	o.RefreshOnMiss = time.Hour
	s := e.source(o)
	e.start(s)
	pulls := e.l.Pulls()
	release := e.l.Block()

	const n = 50
	durs := make([]time.Duration, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			start := time.Now()
			if _, _, ok := s.LookupOrRefresh(context.Background(), fmt.Sprintf("bad-%d", i)); ok {
				t.Error("a bad token matched")
			}
			durs[i] = time.Since(start)
		}()
	}
	wg.Wait()
	for i, d := range durs {
		if d < keylist.MissWait-100*time.Millisecond || d > keylist.MissWait+500*time.Millisecond {
			t.Errorf("miss %d took %v, want about %v", i, d, keylist.MissWait)
		}
	}
	if got := e.l.Pulls() - pulls; got != 1 {
		t.Fatalf("%d concurrent misses made %d pulls, want 1", n, got)
	}

	// The pull is still in flight; a key added before its release is found.
	e.add("late@host", "agent", "key-late")
	release()
	eventually(t, "late key", func() bool { _, _, ok := s.Lookup("key-late"); return ok })
	s.Pull(context.Background()) // serialised after the pull's cache write
}

// TestCallerGivingUpDoesNotCancelPull: the pull is not the caller's.
func TestCallerGivingUpDoesNotCancelPull(t *testing.T) {
	e := newEnv(t)
	o := e.opts()
	o.RefreshOnMiss = time.Hour
	s := e.source(o)
	e.start(s)
	e.add("late@host", "agent", "key-late")
	release := e.l.Block()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, _, ok := s.LookupOrRefresh(ctx, "key-late"); ok {
		t.Fatal("found before the pull landed")
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("caller's ctx not honoured: %v", d)
	}
	release()
	eventually(t, "pull to land after the caller left", func() bool { _, _, ok := s.Lookup("key-late"); return ok })
	s.Pull(context.Background()) // serialised after the pull's cache write
}

// TestRevokedKeyDies is requirement 5.
func TestRevokedKeyDies(t *testing.T) {
	e := newEnv(t)
	e.add("alice@laptop", "agent", "key-alice")
	e.add("bob@desktop", "agent", "key-bob")
	o := e.opts()
	o.RefreshOnMiss = time.Hour
	s := e.source(o)
	e.start(s)
	mustFind(t, s, "key-alice", "alice@laptop", "agent")

	s.LookupOrRefresh(context.Background(), "bad") // opens the throttle window
	e.l.Remove("alice@laptop")
	if err := s.Pull(context.Background()); err != nil {
		t.Fatal(err)
	}
	mustMiss(t, s, "key-alice")
	mustFind(t, s, "key-bob", "bob@desktop", "agent")

	pulls := e.l.Pulls()
	e.l.Block()
	start := time.Now()
	if _, _, ok := s.LookupOrRefresh(context.Background(), "key-alice"); ok {
		t.Fatal("revoked key accepted")
	}
	if time.Since(start) > 50*time.Millisecond || e.l.Pulls() != pulls {
		t.Fatal("a revoked key inside the window was not an immediate miss")
	}
}

func TestRevokedKeyDiesAtTimerTick(t *testing.T) {
	e := newEnv(t)
	e.add("alice@laptop", "agent", "key-alice")
	o := e.opts()
	o.Refresh = 50 * time.Millisecond
	s := e.source(o)
	e.start(s)
	mustFind(t, s, "key-alice", "alice@laptop", "agent")
	e.l.Remove("alice@laptop")
	eventually(t, "revocation at a tick", func() bool { _, _, ok := s.Lookup("key-alice"); return !ok })
}

// TestETagAnd304 is requirement 7.
func TestETagAnd304(t *testing.T) {
	e := newEnv(t)
	e.l.LoadFixture("full")
	const etag = `"725d544c16cbd3c19288ac55547e4f2b"` // the fixture's
	s := e.source(e.opts())
	ctx := context.Background()
	e.start(s) // the first pull, then the settling pull
	if s.Len() != 4 {
		t.Fatalf("Len = %d, want 4", s.Len())
	}
	reqs := e.l.Requests()
	if len(reqs) != 2 || reqs[0].IfNoneMatch != "" || reqs[1].IfNoneMatch != etag {
		t.Fatalf("requests = %+v: want unconditional, then If-None-Match %s", reqs, etag)
	}
	if err := s.Pull(ctx); err != nil { // 304 is success
		t.Fatalf("304: %v", err)
	}
	if s.Len() != 4 {
		t.Fatalf("a 304 changed the list: Len = %d", s.Len())
	}

	// A new Source with the same cache sends no If-None-Match at first: the
	// ETag is not cached.
	s2 := e.source(e.opts())
	n := e.l.Pulls()
	e.start(s2)
	if got := e.l.Requests()[n].IfNoneMatch; got != "" {
		t.Fatalf("restarted Source sent If-None-Match %q", got)
	}
	if s2.Len() != 4 {
		t.Fatalf("Len = %d", s2.Len())
	}
}

// TestFailuresKeepLastGoodList is requirement 8.
func TestFailuresKeepLastGoodList(t *testing.T) {
	good := `{"keys":[{"name":"x","role":"agent","sha256":"` + strings.Repeat("a", 64) + `"}]}`
	cases := []struct {
		name string
		o    keyclienttest.Outcome
	}{
		{"bad json", keyclienttest.Status(200, "not json")},
		{"array not object", keyclienttest.Status(200, "[]")},
		{"trailing garbage", keyclienttest.Status(200, good+"x")},
		{"bad name", keyclienttest.Status(200, `{"keys":[{"name":"a b","role":"agent","sha256":"`+strings.Repeat("a", 64)+`"}]}`)},
		{"empty name", keyclienttest.Status(200, `{"keys":[{"name":"","role":"agent","sha256":"`+strings.Repeat("a", 64)+`"}]}`)},
		{"bad role", keyclienttest.Status(200, `{"keys":[{"name":"x","role":"a/b","sha256":"`+strings.Repeat("a", 64)+`"}]}`)},
		{"upper hex", keyclienttest.Status(200, `{"keys":[{"name":"x","role":"agent","sha256":"`+strings.Repeat("A", 64)+`"}]}`)},
		{"short hash", keyclienttest.Status(200, `{"keys":[{"name":"x","role":"agent","sha256":"abc"}]}`)},
		{"wrong type", keyclienttest.Status(200, `{"keys":[{"name":1,"role":"agent","sha256":"`+strings.Repeat("a", 64)+`"}]}`)},
		{"null entry", keyclienttest.Status(200, `{"keys":[null]}`)},
		{"one bad entry among good", keyclienttest.Status(200, `{"keys":[{"name":"y","role":"agent","sha256":"`+strings.Repeat("b", 64)+`"},{"name":"z","role":"","sha256":"`+strings.Repeat("c", 64)+`"}]}`)},
		{"missing keys", keyclienttest.Status(200, `{"entries":[]}`)},
		{"null keys", keyclienttest.Status(200, `{"keys":null}`)},
		{"keys not array", keyclienttest.Status(200, `{"keys":{}}`)},
		{"5xx", keyclienttest.Status(503, "")},
		{"404 is not empty", keyclienttest.Status(404, `{"error":"not_found","message":"x"}`)},
		{"401", keyclienttest.Status(401, `{"error":"unauthorized","message":"x"}`)},
		{"204", keyclienttest.Status(204, "")},
		{"over MaxBody", keyclienttest.Status(200, `{"keys":[]}`+strings.Repeat(" ", keylist.MaxBody))},
		{"redirect not followed", keyclienttest.Status(302, "").WithHeader("Location", "/elsewhere")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.add("alice@laptop", "agent", "key-alice")
			s := e.source(e.opts())
			e.start(s)
			e.l.FailWith(tc.o)
			err := s.Pull(context.Background())
			if err == nil {
				t.Fatal("Pull returned nil on a failure")
			}
			mustFind(t, s, "key-alice", "alice@laptop", "agent")
			if s.Len() != 1 {
				t.Fatalf("Len = %d, want 1", s.Len())
			}
			e.l.Recover()
			if err := s.Pull(context.Background()); err != nil {
				t.Fatalf("recovery: %v", err)
			}
		})
	}
}

func TestRedirectIsNotFollowedWithCustomClient(t *testing.T) {
	e := newEnv(t)
	target := keyclienttest.NewList(t)
	e.l.FailWith(keyclienttest.Status(307, "").WithHeader("Location", target.URL()))
	o := e.opts()
	o.HTTPClient = &http.Client{Timeout: time.Minute} // default CheckRedirect would follow
	s := e.source(o)
	if err := s.Pull(context.Background()); err == nil {
		t.Fatal("redirect not a failure")
	}
	if target.Pulls() != 0 {
		t.Fatal("redirect was followed")
	}
	if o.HTTPClient.CheckRedirect != nil {
		t.Fatal("the caller's client was modified")
	}
}

func TestTimeoutKeepsLastGoodList(t *testing.T) {
	e := newEnv(t)
	e.add("alice@laptop", "agent", "key-alice")
	s := e.source(e.opts())
	e.start(s)
	e.l.Block()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if err := s.Pull(ctx); err == nil {
		t.Fatal("blocked pull succeeded")
	}
	mustFind(t, s, "key-alice", "alice@laptop", "agent")

	// And through the HTTP client's own timeout.
	o := e.opts()
	o.HTTPClient = &http.Client{Timeout: 150 * time.Millisecond}
	s2 := e.source(o)
	if err := s2.Pull(context.Background()); err == nil {
		t.Fatal("blocked pull succeeded")
	}
}

func TestEmptyListRevokesAll(t *testing.T) {
	e := newEnv(t)
	e.add("alice@laptop", "agent", "key-alice")
	s := e.source(e.opts())
	e.start(s)
	mustFind(t, s, "key-alice", "alice@laptop", "agent")
	e.l.LoadFixture("empty")
	if err := s.Pull(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.Len() != 0 {
		t.Fatalf("Len = %d, want 0", s.Len())
	}
	mustMiss(t, s, "key-alice")
}

func TestUnknownMembersAreIgnored(t *testing.T) {
	e := newEnv(t)
	e.l.FailWith(keyclienttest.Status(200, `{"version":2,"keys":[{"name":"x","role":"agent","sha256":"`+hashOf("kx")+`","note":"n"}]}`))
	e.secret("kx")
	s := e.source(e.opts())
	if err := s.Pull(context.Background()); err != nil {
		t.Fatal(err)
	}
	mustFind(t, s, "kx", "x", "agent")
}

// TestCache is requirement 9.
func TestCache(t *testing.T) {
	e := newEnv(t)
	e.add("alice@laptop", "agent", "key-alice")
	e.add("bob@desktop", "full", "key-bob")
	s := e.source(e.opts())
	cancel := e.begin(s)
	eventually(t, "cache file", func() bool { _, err := os.Stat(e.cache()); return err == nil })
	if err := s.Pull(context.Background()); err != nil { // settle
		t.Fatal(err)
	}
	cancel()

	st, err := os.Stat(e.cache())
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("cache mode = %v, want 0600", st.Mode().Perm())
	}
	got, _ := os.ReadFile(e.cache())
	resp, err := http.Get(e.l.URL())
	if err != nil {
		t.Fatal(err)
	}
	want, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Equal(got, want) {
		t.Fatalf("cache is not the exact body:\n%s\nwant\n%s", got, want)
	}

	// Restart with the provider down: the cache serves.
	e.l.FailWith(keyclienttest.Status(500, ""))
	s2 := e.source(e.opts())
	e.begin(s2)
	mustFind(t, s2, "key-alice", "alice@laptop", "agent") // at once: Start loads the cache synchronously
	mustFind(t, s2, "key-bob", "bob@desktop", "full")
	if err := s2.Start(context.Background()); err == nil {
		t.Fatal("second Start returned nil")
	}
}

func TestCacheMissingAndCorrupt(t *testing.T) {
	e := newEnv(t)
	e.l.FailWith(keyclienttest.Status(500, ""))
	s := e.source(e.opts())
	e.begin(s)
	if s.Len() != 0 {
		t.Fatal("missing cache is not an empty list")
	}

	if err := os.MkdirAll(filepath.Dir(e.cache()), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{"garbage", `{"keys":[{"name":"x","role":"agent","sha256":"zz"}]}`, `{}`} {
		if err := os.WriteFile(e.cache(), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		e.log.mu.Lock()
		e.log.b.Reset()
		e.log.mu.Unlock()
		s := e.source(e.opts())
		e.begin(s)
		if s.Len() != 0 {
			t.Fatalf("corrupt cache %q applied", body)
		}
		if out := e.log.String(); !strings.Contains(out, "cache ignored") {
			t.Fatalf("corrupt cache %q not logged:\n%s", body, out)
		}
	}
}

func TestCacheGoesThroughAccept(t *testing.T) {
	e := newEnv(t)
	e.add("alice@laptop", "agent", "key-alice")
	e.add("root@host", "root", "key-root")
	o := e.opts()
	o.Accept = func(en keylist.Entry) (bool, string) { return en.Role == "agent", "unknown role" }
	s := e.source(o)
	e.start(s)
	e.l.FailWith(keyclienttest.Status(500, ""))
	s2 := e.source(o)
	e.begin(s2)
	mustFind(t, s2, "key-alice", "alice@laptop", "agent")
	mustMiss(t, s2, "key-root")
}

func TestFailedCacheWriteStillApplies(t *testing.T) {
	e := newEnv(t)
	e.add("alice@laptop", "agent", "key-alice")
	blocker := filepath.Join(e.dir, "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	o := e.opts()
	o.CacheFile = filepath.Join(blocker, "list.json") // a directory cannot be made under a file
	s := e.source(o)
	if err := s.Pull(context.Background()); !errors.Is(err, keylist.ErrCacheWrite) {
		t.Fatalf("Pull = %v, want ErrCacheWrite", err)
	}
	mustFind(t, s, "key-alice", "alice@laptop", "agent")
	if out := e.log.String(); !strings.Contains(out, "cache not written") {
		t.Fatalf("failed write not logged:\n%s", out)
	}
}

// TestAcceptRepeatsAndLogs is requirement 10.
func TestAcceptRepeatsAndLogs(t *testing.T) {
	e := newEnv(t)
	for _, k := range []string{"k-alice", "k-mallory", "k-bob", "k-dup1", "k-dup2", "k-dup3", "k-same"} {
		e.secret(k)
	}
	e.secret("pull-token-xyz")
	e.l.Set(
		keyclienttest.Entry{Name: "alice@laptop", Role: "agent", SHA256: hashOf("k-alice")},
		keyclienttest.Entry{Name: "mallory@laptop", Role: "root", SHA256: hashOf("k-mallory")},
		keyclienttest.Entry{Name: "bob@workstation", Role: "agent", SHA256: hashOf("k-bob")},
		keyclienttest.Entry{Name: "dup", Role: "agent", SHA256: hashOf("k-dup1")},
		keyclienttest.Entry{Name: "dup", Role: "full", SHA256: hashOf("k-dup2")},
		keyclienttest.Entry{Name: "other", Role: "agent", SHA256: hashOf("k-alice")},
		keyclienttest.Entry{Name: "clash", Role: "agent", SHA256: hashOf("k-same")},
	)
	var seen []string
	o := e.opts()
	o.Accept = func(en keylist.Entry) (bool, string) {
		seen = append(seen, en.Name)
		switch {
		case en.Role != "agent" && en.Role != "full":
			return false, "unknown role " + en.Role
		case en.Name == "bob@workstation":
			return false, "name is in the local tokens"
		case en.SHA256 == hashOf("k-same"):
			return false, "key is in the local tokens"
		}
		return true, ""
	}
	s := e.source(o)
	if err := s.Pull(context.Background()); err != nil {
		t.Fatal(err)
	}
	mustFind(t, s, "k-alice", "alice@laptop", "agent")
	mustFind(t, s, "k-dup1", "dup", "agent") // the first of the repeats wins
	mustMiss(t, s, "k-dup2")
	mustMiss(t, s, "k-mallory")
	mustMiss(t, s, "k-bob")
	mustMiss(t, s, "k-same")
	if s.Len() != 2 {
		t.Fatalf("Len = %d, want 2", s.Len())
	}
	// Accept runs after the repeat check, in list order (sorted by name).
	if got := strings.Join(seen, ","); got != "alice@laptop,bob@workstation,clash,dup,mallory@laptop" {
		t.Errorf("Accept saw %s", got)
	}
	out := e.log.String()
	for _, want := range []string{"mallory@laptop", "unknown role root", "bob@workstation", "name is in the local tokens", "clash", "key is in the local tokens", "repeated"} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
	// The hashes of every entry (registered in newEnv/secret) must be absent;
	// the cleanup of newEnv asserts that over the whole log.
}

func TestLogsHoldNoSecretsAcrossFailures(t *testing.T) {
	e := newEnv(t)
	const tok = "pull-token-xyz"
	e.secret(tok)
	e.add("alice@laptop", "agent", "key-alice")
	e.secret("presented-key-1")
	e.l.RequireToken(tok)
	tokenFile := filepath.Join(e.dir, "token")
	os.WriteFile(tokenFile, []byte(tok+"\n"), 0o600)
	o := e.opts()
	o.TokenFile = tokenFile
	o.RefreshOnMiss = time.Millisecond
	s := e.source(o)
	e.start(s)

	// A 401, a 5xx and a bad body, each with its text checked.
	os.WriteFile(tokenFile, []byte("wrong-token-abc\n"), 0o600)
	e.secret("wrong-token-abc")
	errs := []error{s.Pull(context.Background())}
	os.WriteFile(tokenFile, []byte(tok), 0o600)
	e.l.FailWith(keyclienttest.Status(500, "boom "+tok+" "+hashOf("key-alice")))
	errs = append(errs, s.Pull(context.Background()))
	e.l.FailWith(keyclienttest.Status(200, `{"keys":[{"name":"x","role":"r","sha256":"`+hashOf("key-alice")+`x"}]}`))
	errs = append(errs, s.Pull(context.Background()))
	s.LookupOrRefresh(context.Background(), "presented-key-1")
	for i, err := range errs {
		if err == nil {
			t.Fatalf("pull %d did not fail", i)
		}
		for _, f := range e.forbidden {
			if strings.Contains(err.Error(), f) {
				t.Errorf("error %d holds %q: %v", i, f, err)
			}
		}
	}
	if out := e.log.String(); !strings.Contains(out, "needs an operator") || !strings.Contains(out, "level=ERROR") {
		t.Errorf("401 not logged as an error needing an operator:\n%s", out)
	}
}

func TestFailureLoggedOnceThenRecovery(t *testing.T) {
	e := newEnv(t)
	s := e.source(e.opts())
	e.start(s)
	e.l.FailWith(keyclienttest.Status(500, ""))
	for i := 0; i < 5; i++ {
		s.Pull(context.Background())
	}
	e.l.Recover()
	s.Pull(context.Background())
	out := e.log.String()
	if n := strings.Count(out, "pull failed"); n != 1 {
		t.Errorf("failure logged %d times, want 1:\n%s", n, out)
	}
	if n := strings.Count(out, "recovered"); n != 1 {
		t.Errorf("recovery logged %d times, want 1:\n%s", n, out)
	}
}

func TestErrorTextHasNoUserinfo(t *testing.T) {
	e := newEnv(t)
	u := strings.Replace(e.l.URL(), "http://", "http://svc:userinfo-secret@", 1)
	o := e.opts()
	o.URL = u
	e.l.FailWith(keyclienttest.Status(500, ""))
	s := e.source(o)
	err := s.Pull(context.Background())
	if err == nil || strings.Contains(err.Error(), "userinfo-secret") {
		t.Fatalf("err = %v", err)
	}
	// A dead server gives a *url.Error: still no userinfo.
	o.URL = "http://svc:userinfo-secret@127.0.0.1:1/x"
	s = e.source(o)
	err = s.Pull(context.Background())
	if err == nil || strings.Contains(err.Error(), "userinfo-secret") {
		t.Fatalf("err = %v", err)
	}
	if _, nerr := keylist.New(keylist.Options{URL: "ftp://svc:userinfo-secret@host/x", CacheFile: "c", Accept: acceptAll}); nerr == nil || strings.Contains(nerr.Error(), "userinfo-secret") {
		t.Fatalf("New err = %v", nerr)
	}
	if strings.Contains(e.log.String(), "userinfo-secret") {
		t.Fatal("log holds userinfo")
	}
}

// TestPullToken is requirement 11.
func TestPullToken(t *testing.T) {
	e := newEnv(t)
	e.secret("tok1")
	e.secret("tok2")
	e.l.RequireToken("tok1")
	tokenFile := filepath.Join(e.dir, "token")
	if err := os.WriteFile(tokenFile, []byte("  tok1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	o := e.opts()
	o.TokenFile = tokenFile
	s := e.source(o)
	if err := s.Pull(context.Background()); err != nil {
		t.Fatalf("with the right token: %v", err)
	}
	if got := e.l.Requests()[0].Authorization; got != "Bearer tok1" {
		t.Fatalf("Authorization = %q", got)
	}

	// Rotation is picked up by the next pull.
	e.l.RequireToken("tok2")
	if err := s.Pull(context.Background()); err == nil {
		t.Fatal("old token accepted by a provider that rotated")
	}
	os.WriteFile(tokenFile, []byte("tok2"), 0o600)
	if err := s.Pull(context.Background()); err != nil {
		t.Fatalf("after rotation: %v", err)
	}
	if got := e.l.Requests()[2].Authorization; got != "Bearer tok2" {
		t.Fatalf("Authorization = %q", got)
	}

	// A token file that goes missing is a pull failure, not a panic.
	os.Remove(tokenFile)
	if err := s.Pull(context.Background()); err == nil {
		t.Fatal("pull without a readable token file succeeded")
	}

	// No token file: no Authorization header.
	e.l.RequireToken("")
	s2 := e.source(e.opts())
	n := e.l.Pulls()
	s2.Pull(context.Background())
	if got := e.l.Requests()[n].Authorization; got != "" {
		t.Fatalf("Authorization = %q without a token file", got)
	}
}

func TestNewFailsOnBadTokenFile(t *testing.T) {
	e := newEnv(t)
	empty := filepath.Join(e.dir, "empty")
	os.WriteFile(empty, []byte(" \n\t"), 0o600)
	for _, p := range []string{empty, filepath.Join(e.dir, "missing")} {
		o := e.opts()
		o.TokenFile = p
		if _, err := keylist.New(o); err == nil {
			t.Errorf("New accepted token file %s", p)
		}
	}
}

// TestNewValidates is requirement 13.
func TestNewValidates(t *testing.T) {
	ok := keylist.Options{URL: "https://provider.example/v1/lists/x", CacheFile: "cache.json", Accept: acceptAll}
	if _, err := keylist.New(ok); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*keylist.Options){
		"missing URL":       func(o *keylist.Options) { o.URL = "" },
		"relative URL":      func(o *keylist.Options) { o.URL = "/v1/lists/x" },
		"no host":           func(o *keylist.Options) { o.URL = "https:///x" },
		"ftp":               func(o *keylist.Options) { o.URL = "ftp://provider.example/x" },
		"unparsable":        func(o *keylist.Options) { o.URL = "http://[::1" },
		"missing CacheFile": func(o *keylist.Options) { o.CacheFile = "" },
		"negative Refresh":  func(o *keylist.Options) { o.Refresh = -time.Second },
		"negative OnMiss":   func(o *keylist.Options) { o.RefreshOnMiss = -time.Second },
		"missing Accept":    func(o *keylist.Options) { o.Accept = nil },
	}
	for name, mod := range cases {
		o := ok
		mod(&o)
		if _, err := keylist.New(o); err == nil {
			t.Errorf("%s: New returned nil", name)
		}
	}
	o := ok
	o.URL = "http://127.0.0.1:8080/x"
	if _, err := keylist.New(o); err != nil {
		t.Errorf("http URL: %v", err)
	}
}

func TestStartTwiceFails(t *testing.T) {
	e := newEnv(t)
	s := e.source(e.opts())
	e.start(s)
	if err := s.Start(context.Background()); err == nil {
		t.Fatal("second Start succeeded")
	}
}

func TestServiceStartsWhenProviderDown(t *testing.T) {
	e := newEnv(t)
	o := e.opts()
	o.URL = "http://127.0.0.1:1/x" // nothing listens
	s := e.source(o)
	e.begin(s)
	mustMiss(t, s, "anything")
}
