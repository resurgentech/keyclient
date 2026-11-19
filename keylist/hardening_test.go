package keylist_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/resurgentech/keyclient/keyclienttest"
	"github.com/resurgentech/keyclient/keylist"
)

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// serve starts an HTTP server with h, closed at the end of the test.
func serve(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func (e *env) sourceAt(url string) *keylist.Source {
	o := e.opts()
	o.URL = url
	return e.source(o)
}

// TestUnconditional304IsFailure (probe K2): a 304 to a request that carried no
// If-None-Match is a failure, and it changes nothing.
func TestUnconditional304IsFailure(t *testing.T) {
	e := newEnv(t)
	var inm []string
	var mu sync.Mutex
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		inm = append(inm, r.Header.Get("If-None-Match"))
		mu.Unlock()
		w.WriteHeader(http.StatusNotModified)
	})
	s := e.sourceAt(srv.URL)
	if err := s.Pull(context.Background()); err == nil {
		t.Fatal("a 304 to an unconditional request was a success")
	}
	if st := s.Status(); st.LastError == nil || !st.LastSuccess.IsZero() {
		t.Fatalf("Status = %+v, want an error and no success", st)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(inm) != 1 || inm[0] != "" {
		t.Fatalf("If-None-Match seen: %q", inm)
	}
}

// TestWrongContentTypeIsFailure (probe K3): a 200 whose media type is not
// application/json is a failure that keeps the list; parameters are allowed.
func TestWrongContentTypeIsFailure(t *testing.T) {
	body := `{"keys":[]}`
	cases := []struct {
		name  string
		ctype []string // nil: no Content-Type header at all
		ok    bool
	}{
		{"json", []string{"application/json"}, true},
		{"json charset", []string{"application/json; charset=utf-8"}, true},
		{"json upper case", []string{"Application/JSON"}, true},
		{"html", []string{"text/html"}, false},
		{"plain", []string{"text/plain; charset=utf-8"}, false},
		{"json suffix type", []string{"application/vnd.x+json"}, false},
		{"unparsable", []string{"application/json; ;;="}, false},
		{"missing", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.add("alice@laptop", "agent", "key-alice")
			good := true
			var mu sync.Mutex
			srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				if good {
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprintf(w, `{"keys":[{"name":"alice@laptop","role":"agent","sha256":%q}]}`, hashOf("key-alice"))
					return
				}
				w.Header()["Content-Type"] = tc.ctype // nil suppresses sniffing
				fmt.Fprint(w, body)
			})
			s := e.sourceAt(srv.URL)
			if err := s.Pull(context.Background()); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			good = false
			mu.Unlock()
			err := s.Pull(context.Background())
			if tc.ok {
				if err != nil || s.Len() != 0 {
					t.Fatalf("err = %v, Len = %d: want applied empty list", err, s.Len())
				}
				return
			}
			if err == nil {
				t.Fatal("Pull succeeded")
			}
			mustFind(t, s, "key-alice", "alice@laptop", "agent")
		})
	}
}

// TestStrictBody (probe K6): member names are matched exactly, a repeat is a
// bad body, trailing data is a bad body, unknown members are skipped whole.
func TestStrictBody(t *testing.T) {
	h := strings.Repeat("a", 64)
	entry := `{"name":"x","role":"agent","sha256":"` + h + `"}`
	bad := map[string]string{
		"keys twice":          `{"keys":[` + entry + `],"keys":[]}`,
		"KEYS beside keys":    `{"keys":[` + entry + `],"KEYS":[]}`,
		"Keys alone":          `{"Keys":[` + entry + `]}`,
		"Kelvin sign keys":    "{\"Keys\":[]}",
		"name twice":          `{"keys":[{"name":"x","name":"y","role":"agent","sha256":"` + h + `"}]}`,
		"role twice":          `{"keys":[{"name":"x","role":"agent","role":"full","sha256":"` + h + `"}]}`,
		"NAME":                `{"keys":[{"NAME":"x","role":"agent","sha256":"` + h + `"}]}`,
		"Role":                `{"keys":[{"name":"x","Role":"agent","sha256":"` + h + `"}]}`,
		"SHA256 beside sha":   `{"keys":[{"name":"x","role":"agent","sha256":"` + h + `","SHA256":"` + h + `"}]}`,
		"second object":       `{"keys":[]} {"keys":[]}`,
		"trailing comma data": `{"keys":[]},`,
		"escaped repeat":      `{"keys":[],"keys":[]}`,
		"name not string":     `{"keys":[{"name":{"a":1},"role":"agent","sha256":"` + h + `"}]}`,
		"unknown then broken": `{"x":[1,2,{"y":[}]}],"keys":[]}`,
	}
	for name, body := range bad {
		t.Run("bad/"+name, func(t *testing.T) {
			e := newEnv(t)
			e.add("alice@laptop", "agent", "key-alice")
			s := e.source(e.opts())
			e.start(s)
			e.l.FailWith(keyclienttest.Status(200, body))
			if err := s.Pull(context.Background()); err == nil {
				t.Fatal("Pull succeeded")
			}
			mustFind(t, s, "key-alice", "alice@laptop", "agent")
		})
	}
	good := map[string]string{
		"unknown nested member":  `{"meta":{"keys":[1,{"keys":2,"keys":3}],"KEYS":null},"keys":[` + entry + `]}`,
		"unknown after keys":     `{"keys":[` + entry + `],"next":[[],{}],"n":null}`,
		"unknown entry member":   `{"keys":[{"name":"x","note":{"a":[1,{"b":2}]},"role":"agent","sha256":"` + h + `"}]}`,
		"whitespace around":      " \n{ \"keys\" : [ " + entry + " ] }\n ",
		"escaped member name ok": `{"keys":[` + entry + `]}`,
	}
	for name, body := range good {
		t.Run("good/"+name, func(t *testing.T) {
			e := newEnv(t)
			e.l.FailWith(keyclienttest.Status(200, body))
			s := e.source(e.opts())
			if err := s.Pull(context.Background()); err != nil {
				t.Fatal(err)
			}
			if s.Len() != 1 {
				t.Fatalf("Len = %d, want 1", s.Len())
			}
		})
	}
}

func TestAcceptRolesHelper(t *testing.T) {
	var asked []keylist.Entry
	acc := keylist.AcceptRoles([]string{"agent", "full"}, func(e keylist.Entry) bool {
		asked = append(asked, e)
		return e.Name == "taken" || e.SHA256 == hashOf("taken-key")
	})
	cases := []struct {
		e      keylist.Entry
		ok     bool
		reason string
	}{
		{keylist.Entry{Name: "a", Role: "agent", SHA256: hashOf("1")}, true, ""},
		{keylist.Entry{Name: "b", Role: "full", SHA256: hashOf("2")}, true, ""},
		{keylist.Entry{Name: "c", Role: "Agent", SHA256: hashOf("3")}, false, "unknown role"},
		{keylist.Entry{Name: "d", Role: "root", SHA256: hashOf("4")}, false, "unknown role"},
		{keylist.Entry{Name: "taken", Role: "agent", SHA256: hashOf("5")}, false, "name or key already in the local token source"},
		{keylist.Entry{Name: "e", Role: "agent", SHA256: hashOf("taken-key")}, false, "name or key already in the local token source"},
	}
	for _, tc := range cases {
		ok, reason := acc(tc.e)
		if ok != tc.ok || reason != tc.reason {
			t.Errorf("%+v: got %v %q, want %v %q", tc.e, ok, reason, tc.ok, tc.reason)
		}
	}
	if len(asked) != 4 { // takenLocally is not asked about an unknown role
		t.Errorf("takenLocally asked %d times, want 4", len(asked))
	}
	if ok, _ := keylist.AcceptRoles([]string{"agent"}, nil)(keylist.Entry{Role: "agent"}); !ok {
		t.Error("nil takenLocally must mean nothing is taken")
	}
	if ok, _ := keylist.AcceptRoles(nil, nil)(keylist.Entry{Role: "agent"}); ok {
		t.Error("no roles must accept nothing")
	}
}

func TestStatus(t *testing.T) {
	e := newEnv(t)
	e.add("alice@laptop", "agent", "key-alice")
	s := e.source(e.opts())
	if st := s.Status(); !st.LastSuccess.IsZero() || st.LastError != nil || st.Age != 0 || st.Entries != 0 || st.CacheFailing {
		t.Fatalf("fresh Status = %+v", st)
	}
	before := time.Now()
	if err := s.Pull(context.Background()); err != nil {
		t.Fatal(err)
	}
	st := s.Status()
	if st.LastSuccess.Before(before) || st.LastError != nil || st.Entries != 1 || st.Age < 0 || st.Age > time.Minute || st.CacheFailing {
		t.Fatalf("after 200: %+v", st)
	}
	first := st.LastSuccess
	time.Sleep(5 * time.Millisecond)
	if err := s.Pull(context.Background()); err != nil { // 304
		t.Fatal(err)
	}
	if st = s.Status(); !st.LastSuccess.After(first) {
		t.Fatalf("a 304 did not move LastSuccess: %+v", st)
	}
	last := st.LastSuccess
	e.l.FailWith(keyclienttest.Status(500, ""))
	if err := s.Pull(context.Background()); err == nil {
		t.Fatal("500 succeeded")
	}
	st = s.Status()
	if st.LastError == nil || !st.LastSuccess.Equal(last) || st.Entries != 1 {
		t.Fatalf("after 500: %+v", st)
	}
	e.l.Recover()
	if err := s.Pull(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st = s.Status(); st.LastError != nil {
		t.Fatalf("LastError not cleared: %+v", st)
	}
}

func TestCacheWriteFailureInStatus(t *testing.T) {
	e := newEnv(t)
	e.add("alice@laptop", "agent", "key-alice")
	o := e.opts()
	o.CacheFile = filepath.Join(e.dir, "file", "list.json")
	writeFile(t, filepath.Join(e.dir, "file"), "") // a directory cannot be made under a file
	s := e.source(o)
	err := s.Pull(context.Background())
	if !errors.Is(err, keylist.ErrCacheWrite) {
		t.Fatalf("Pull = %v", err)
	}
	st := s.Status()
	if !st.CacheFailing || !errors.Is(st.LastError, keylist.ErrCacheWrite) || st.LastSuccess.IsZero() || st.Entries != 1 {
		t.Fatalf("Status = %+v", st)
	}
	if out := e.log.String(); !strings.Contains(out, "level=ERROR") {
		t.Fatalf("not logged at error level:\n%s", out)
	}
	// A later write that works clears it.
	o2 := e.opts()
	s2 := e.source(o2)
	if err := s2.Pull(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st := s2.Status(); st.CacheFailing || st.LastError != nil {
		t.Fatalf("Status = %+v", st)
	}
}

// TestMissWaitersCapped (probe K5, the waiting part): with the provider hung,
// at most MaxMissWaiters misses wait; the rest return at once.
func TestMissWaitersCapped(t *testing.T) {
	e := newEnv(t)
	o := e.opts()
	o.RefreshOnMiss = time.Hour
	s := e.source(o)
	e.start(s)
	e.l.Block()
	pulls := e.l.Pulls()

	const n = MaxMissWaitersPlus
	var wg sync.WaitGroup
	var mu sync.Mutex
	waited, fast := 0, 0
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			start := time.Now()
			if _, _, ok := s.LookupOrRefresh(context.Background(), fmt.Sprintf("bad-%d", i)); ok {
				t.Error("a bad token matched")
			}
			d := time.Since(start)
			mu.Lock()
			defer mu.Unlock()
			if d > keylist.MissWait/2 {
				waited++
			} else {
				fast++
			}
		}()
	}
	wg.Wait()
	if waited != keylist.MaxMissWaiters || fast != n-keylist.MaxMissWaiters {
		t.Fatalf("%d waited and %d returned at once, want %d and %d", waited, fast, keylist.MaxMissWaiters, n-keylist.MaxMissWaiters)
	}
	if got := e.l.Pulls() - pulls; got != 1 {
		t.Fatalf("%d pulls, want 1", got)
	}
}

const MaxMissWaitersPlus = keylist.MaxMissWaiters + 50

// TestTrailingPullAfterThrottledMiss: a miss refused a pull gets one pull when
// its window ends, which picks up a key added meanwhile.
func TestTrailingPullAfterThrottledMiss(t *testing.T) {
	e := newEnv(t)
	o := e.opts()
	o.RefreshOnMiss = 400 * time.Millisecond
	s := e.source(o)
	e.start(s)
	s.LookupOrRefresh(context.Background(), "bad-1") // opens a window with a pull
	pulls := e.l.Pulls()

	e.add("late@host", "agent", "key-late")
	if _, _, ok := s.LookupOrRefresh(context.Background(), "key-late"); ok {
		t.Fatal("found inside the window without a pull")
	}
	if e.l.Pulls() != pulls {
		t.Fatal("a throttled miss pulled")
	}
	eventually(t, "trailing pull to pick up the key", func() bool { _, _, ok := s.Lookup("key-late"); return ok })
	mustFind(t, s, "key-late", "late@host", "agent")
	// The trailing pull opened a window of its own and nothing was throttled
	// in it: no more pulls.
	time.Sleep(1000 * time.Millisecond)
	if got := e.l.Pulls() - pulls; got != 1 {
		t.Fatalf("%d pulls after the first miss pull, want exactly 1 (the trailing one)", got)
	}
}

func TestTrailingPullOnceForABurst(t *testing.T) {
	e := newEnv(t)
	o := e.opts()
	o.RefreshOnMiss = time.Second
	s := e.source(o)
	e.start(s)
	s.LookupOrRefresh(context.Background(), "bad-0")
	pulls := e.l.Pulls()
	burstMisses(s, 1000)
	if e.l.Pulls() != pulls {
		t.Fatal("throttled misses pulled")
	}
	time.Sleep(1700 * time.Millisecond)
	if got := e.l.Pulls() - pulls; got != 1 {
		t.Fatalf("a burst of 1000 throttled misses led to %d pulls, want 1", got)
	}
}

func burstMisses(s *keylist.Source, n int) {
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.LookupOrRefresh(context.Background(), fmt.Sprintf("burst-%d", i))
		}()
	}
	wg.Wait()
}

func TestNoTrailingPullWithoutThrottledMiss(t *testing.T) {
	e := newEnv(t)
	o := e.opts()
	o.RefreshOnMiss = 200 * time.Millisecond
	s := e.source(o)
	e.start(s)
	s.LookupOrRefresh(context.Background(), "bad-1")
	pulls := e.l.Pulls()
	time.Sleep(700 * time.Millisecond)
	if got := e.l.Pulls() - pulls; got != 0 {
		t.Fatalf("%d pulls with no throttled miss", got)
	}
	// A miss after the window is not throttled: its own pull, no trailing one.
	s.LookupOrRefresh(context.Background(), "bad-2")
	time.Sleep(500 * time.Millisecond)
	if got := e.l.Pulls() - pulls; got != 1 {
		t.Fatalf("%d pulls, want 1", got)
	}
}

func TestNoTrailingPullAfterStartContextEnds(t *testing.T) {
	e := newEnv(t)
	o := e.opts()
	o.RefreshOnMiss = 300 * time.Millisecond
	s := e.source(o)
	cancel := e.begin(s)
	eventually(t, "first pull", func() bool { return e.l.Pulls() >= 1 })
	s.LookupOrRefresh(context.Background(), "bad-1")
	pulls := e.l.Pulls()
	s.LookupOrRefresh(context.Background(), "bad-2") // throttled: a trailing pull is due
	cancel()
	time.Sleep(700 * time.Millisecond)
	if got := e.l.Pulls() - pulls; got != 0 {
		t.Fatalf("%d pulls after Start's context ended", got)
	}
}
