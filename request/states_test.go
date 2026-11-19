package request_test

// Tests named after the rows and invariants of docs/STATES.md (machines 2 and
// 3), so the table and the tests can be checked against each other.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/resurgentech/keyclient/keyclienttest"
	"github.com/resurgentech/keyclient/request"
)

// ensure calls request.Ensure and asserts T-I5 on what it returns.
func ensure(t testing.TB, ctx context.Context, o request.Options) (request.Result, error) {
	t.Helper()
	res, err := request.Ensure(ctx, o)
	checkTI5(t, o.TokenPath, res, err)
	return res, err
}

// checkTI5 asserts T-I5: a token with a nil error, or a non-nil error; never an
// empty token with a nil error; and Ready is true only with the token on disk.
func checkTI5(t testing.TB, tokenPath string, res request.Result, err error) {
	t.Helper()
	if err == nil {
		if res.Token == "" {
			t.Errorf("T-I5: Ensure returned an empty token with a nil error")
			return
		}
		if b, rerr := os.ReadFile(tokenPath); rerr != nil || strings.TrimSpace(string(b)) != res.Token {
			t.Errorf("T-I5: Ensure returned a token that is not the one on disk (%v)", rerr)
		}
		return
	}
	var ep *request.ErrPending
	if errors.As(err, &ep) {
		checkReady(t, tokenPath, ep)
	}
}

// checkReady asserts that Ready agrees with Err, and that Ready means the token
// is on disk.
func checkReady(t testing.TB, tokenPath string, ep *request.ErrPending) {
	t.Helper()
	select {
	case <-ep.Done():
		if ep.Ready() != (ep.Err() == nil) {
			t.Errorf("T-I5: Ready=%v but Err=%v", ep.Ready(), ep.Err())
		}
		if ep.Ready() && tokenPath != "" {
			if b, err := os.ReadFile(tokenPath); err != nil || strings.TrimSpace(string(b)) == "" {
				t.Errorf("T-I5: Ready is true but the token file holds no token (%v)", err)
			}
		}
	default:
		if ep.Ready() || ep.Err() != nil {
			t.Errorf("T-I5: Ready=%v Err=%v before Done", ep.Ready(), ep.Err())
		}
	}
}

// readPendingMap returns the pending file of e as a map.
func readPendingMap(t *testing.T, e *env) map[string]any {
	t.Helper()
	b, err := os.ReadFile(request.PendingPath(e.token))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// fileAndStop files a request with Wait false and ends its background poll at
// once, as a tool that prints the notice and exits does. The pending file is
// left for a later run.
func fileAndStop(t *testing.T, e *env) request.Notice {
	t.Helper()
	o := e.opts
	o.Wait = false
	ctx, cancel := context.WithCancel(context.Background())
	_, err := ensure(t, ctx, o)
	cancel()
	var ep *request.ErrPending
	if !errors.As(err, &ep) {
		t.Fatalf("Ensure(Wait=false) = %v, want *ErrPending", err)
	}
	waitDone(t, ep)
	if !e.pendingExists() {
		t.Fatal("no pending file after the first run")
	}
	return ep.Notice
}

// T-16, T-I10 (the reviewer's S1): a tool that prints the notice and exits
// (Wait false, its process ends right after Ensure returns), run again every two
// intervals while the request is approved, collects the key.
func TestTI10_ShortWaitFalseRunsCollect(t *testing.T) {
	e := newEnv(t)
	e.p.Script(keyclienttest.Approved()) // approved from the start
	e.opts.Wait = false
	for run := 1; run <= 6; run++ {
		ctx, cancel := context.WithCancel(context.Background())
		res, err := ensure(t, ctx, e.opts)
		var ep *request.ErrPending
		isPending := errors.As(err, &ep)
		cancel() // the process exits
		if isPending {
			waitDone(t, ep)
		}
		if err == nil {
			if res.Token != e.p.Key() {
				t.Fatalf("run %d: token %q", run, res.Token)
			}
			if len(e.p.Filed()) != 1 {
				t.Errorf("filed %d, want 1", len(e.p.Filed()))
			}
			return
		}
		if !isPending {
			t.Fatalf("run %d: err = %v", run, err)
		}
		time.Sleep(2 * unit)
	}
	t.Fatalf("six Wait=false runs over twelve intervals, approved all along: no key collected (polls %d)", len(e.p.Polls()))
}

// T-16: with Wait false, when a poll is already due (a resumed request), that
// one poll is made before Ensure returns; if it settles the request its result
// is returned, else *ErrPending.
func TestT16_WaitFalseMakesTheDuePoll(t *testing.T) {
	t.Run("settles", func(t *testing.T) {
		e := newEnv(t)
		fileAndStop(t, e)
		time.Sleep(2 * unit) // the next poll is due
		e.p.Script(keyclienttest.Approved())
		e.opts.Wait = false
		res, err := ensure(t, context.Background(), e.opts)
		if err != nil || res.Token != e.p.Key() {
			t.Fatalf("res=%+v err=%v, want the key", res, err)
		}
		if e.pendingExists() {
			t.Error("pending remains")
		}
	})
	t.Run("denied", func(t *testing.T) {
		e := newEnv(t)
		fileAndStop(t, e)
		time.Sleep(2 * unit)
		e.p.Script(keyclienttest.Denied())
		e.opts.Wait = false
		if _, err := ensure(t, context.Background(), e.opts); !errors.Is(err, request.ErrDenied) {
			t.Fatalf("err = %v, want ErrDenied", err)
		}
	})
	t.Run("still pending", func(t *testing.T) {
		e := newEnv(t)
		fileAndStop(t, e)
		time.Sleep(2 * unit)
		e.p.Script(keyclienttest.Pending())
		e.opts.Wait = false
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		_, err := ensure(t, ctx, e.opts)
		var ep *request.ErrPending
		if !errors.As(err, &ep) {
			t.Fatalf("err = %v, want *ErrPending", err)
		}
		if n := len(e.p.Polls()); n != 1 {
			t.Errorf("polls made before Ensure returned = %d, want 1", n)
		}
		cancel()
		waitDone(t, ep)
	})
	t.Run("not due: no poll", func(t *testing.T) {
		e := newEnv(t)
		e.p.SetInterval(50)
		fileAndStop(t, e)
		e.opts.Wait = false
		ctx, cancel := context.WithCancel(context.Background())
		_, err := ensure(t, ctx, e.opts)
		var ep *request.ErrPending
		if !errors.As(err, &ep) || !ep.Notice.Resumed {
			t.Fatalf("err = %v, want a resumed *ErrPending", err)
		}
		if n := len(e.p.Polls()); n != 0 {
			t.Errorf("polls = %d, want none before the next poll is due", n)
		}
		cancel()
		waitDone(t, ep)
	})
}

// L-22: after each reply that keeps the loop going, the pending file records
// the current interval and when the next poll is due.
func TestL22_PendingRecordsPace(t *testing.T) {
	e := newEnv(t) // interval 1
	m := func() map[string]any {
		fileAndStop(t, e)
		return readPendingMap(t, e)
	}()
	if m["interval"] != float64(1) {
		t.Errorf("after filing: interval = %v, want 1", m["interval"])
	}
	filedNext, err := time.Parse(time.RFC3339Nano, fmt.Sprint(m["next_poll_at"]))
	if err != nil {
		t.Fatalf("after filing: next_poll_at = %v: %v", m["next_poll_at"], err)
	}
	if d := filedNext.Sub(e.p.Filed()[0].Time); d < unit || d > 3*unit {
		t.Errorf("after filing: next_poll_at is %v after the filing, want about one interval", d)
	}

	e.p.Script(keyclienttest.SlowDown()) // interval becomes 3
	ctx, cancel := context.WithTimeout(context.Background(), 2*unit)
	defer cancel()
	if _, err := ensure(t, ctx, e.opts); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	polls := e.p.Polls()
	if len(polls) != 1 {
		t.Fatalf("polls = %d, want 1", len(polls))
	}
	m = readPendingMap(t, e)
	if m["interval"] != float64(3) {
		t.Errorf("after slow_down: interval = %v, want 3", m["interval"])
	}
	next, err := time.Parse(time.RFC3339Nano, fmt.Sprint(m["next_poll_at"]))
	if err != nil {
		t.Fatalf("after slow_down: next_poll_at = %v: %v", m["next_poll_at"], err)
	}
	if d := next.Sub(polls[0].Time); d < 3*unit || d > 5*unit {
		t.Errorf("after slow_down: next_poll_at is %v after the poll, want about 3 intervals", d)
	}
}

// L-I1 across runs, L-2: the gap between the last poll of one run and the first
// of the next is at least the current interval, and after a slow_down the next
// run uses the raised interval.
func TestLI1_PaceHoldsAcrossRuns(t *testing.T) {
	for _, c := range []struct {
		name  string
		first keyclienttest.Outcome
		iv    time.Duration
	}{
		{"pending", keyclienttest.Pending(), 2 * unit},
		{"slow_down", keyclienttest.SlowDown(), 4 * unit},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			e.p.SetInterval(2) // slow_down raises it to 4
			e.p.Script(c.first)
			ctx, cancel := context.WithTimeout(context.Background(), 3*unit) // one poll, at 2
			_, err := ensure(t, ctx, e.opts)
			cancel()
			if !errors.Is(err, context.DeadlineExceeded) || len(e.p.Polls()) != 1 {
				t.Fatalf("run 1: err = %v polls = %d", err, len(e.p.Polls()))
			}
			e.p.Script(keyclienttest.Approved())
			if _, err := ensure(t, context.Background(), e.opts); err != nil {
				t.Fatalf("run 2: %v", err)
			}
			polls := e.p.Polls()
			if len(polls) != 2 {
				t.Fatalf("polls = %d", len(polls))
			}
			if gap := polls[1].Time.Sub(polls[0].Time); gap < c.iv {
				t.Errorf("gap across runs = %v, want >= %v", gap, c.iv)
			}
		})
	}
}

// L-2: a resumed run polls when P says the next poll is due; a pending file of
// the older format (no next_poll_at) means now; a next_poll_at further away
// than one interval is cut to one interval from now.
func TestL2_ResumeUsesRecordedNext(t *testing.T) {
	for _, c := range []struct {
		name     string
		interval int
		next     func() any
		lo, hi   time.Duration
	}{
		{"older format: now", 8, nil, 0, 2 * unit},
		{"in the past: now", 8, func() any { return time.Now().Add(-time.Hour).Format(time.RFC3339Nano) }, 0, 2 * unit},
		{"in 3 units", 8, func() any { return time.Now().Add(3 * unit).Format(time.RFC3339Nano) }, 3 * unit, 6 * unit},
		{"far future: one interval", 2, func() any { return time.Now().Add(time.Hour).Format(time.RFC3339Nano) }, 2 * unit, 5 * unit},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			writePending(t, e, 0o600, pendingJSON(e, time.Now().Add(time.Hour), e.p.URL()+"/v1/requests/old/poll", func(m map[string]any) {
				m["interval"] = c.interval
				if c.next != nil {
					m["next_poll_at"] = c.next()
				}
			}))
			start := time.Now()
			if _, err := ensure(t, context.Background(), e.opts); !errors.Is(err, request.ErrUnauthorized) {
				t.Fatalf("err = %v", err)
			}
			polls := e.p.Polls()
			if len(polls) != 1 {
				t.Fatalf("polls = %d", len(polls))
			}
			if d := polls[0].Time.Sub(start); d < c.lo || d > c.hi {
				t.Errorf("first poll %v after the start, want between %v and %v", d, c.lo, c.hi)
			}
		})
	}
}

// T-7: a service without a provider leaves nothing on disk: no directory, no
// lock file.
func TestT7_NoProviderLeavesNothing(t *testing.T) {
	e := newEnv(t)
	e.p.SetDiscovery(false)
	dir := filepath.Dir(e.token)
	for _, wait := range []bool{true, false} {
		e.opts.Wait = wait
		if _, err := ensure(t, context.Background(), e.opts); !errors.Is(err, request.ErrNoProvider) {
			t.Fatalf("Wait=%v: err = %v", wait, err)
		}
		if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
			ents, _ := os.ReadDir(dir)
			t.Errorf("Wait=%v: the directory was made (%v), holding %v", wait, err, ents)
		}
	}
}

// discoveryWriting is a discovery endpoint that, as a racing process would,
// writes a pending file before it answers with the provider's request URL.
func discoveryWriting(t *testing.T, e *env, content func(serviceURL string) string) *httptest.Server {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writePending(t, e, 0o600, content(srv.URL))
		fmt.Fprintf(w, `{"request_url":%q}`, e.p.RequestURL())
	}))
	t.Cleanup(srv.Close)
	return srv
}

// T-7 order: discovery runs before the lock; a pending file that appears
// meanwhile is found under the lock and resumed, and the discovered URL is not
// used. A pending file of another service is removed (T-12) and the discovered
// URL is used.
func TestT7_DiscoveryBeforeLockThenRecheck(t *testing.T) {
	t.Run("current P appears: resumed", func(t *testing.T) {
		e := newEnv(t)
		srv := discoveryWriting(t, e, func(svc string) string {
			e2 := *e
			e2.opts.ServiceURL = svc
			return pendingJSON(&e2, time.Now().Add(time.Hour), e.p.URL()+"/v1/requests/old/poll")
		})
		e.opts.ServiceURL = srv.URL
		var n request.Notice
		e.opts.Notify = func(x request.Notice) { n = x }
		if _, err := ensure(t, context.Background(), e.opts); !errors.Is(err, request.ErrUnauthorized) {
			t.Fatalf("err = %v, want the resumed request's 401", err)
		}
		if len(e.p.Filed()) != 0 || !n.Resumed {
			t.Errorf("filed %d, resumed %v: want the pending request resumed, nothing filed", len(e.p.Filed()), n.Resumed)
		}
	})
	t.Run("foreign P appears: removed, discovered URL used", func(t *testing.T) {
		e := newEnv(t)
		srv := discoveryWriting(t, e, func(string) string {
			return pendingJSON(e, time.Now().Add(time.Hour), e.p.URL()+"/v1/requests/old/poll",
				func(m map[string]any) { m["service_url"] = "https://other.example" })
		})
		e.opts.ServiceURL = srv.URL
		e.p.Script(keyclienttest.Approved())
		var n request.Notice
		e.opts.Notify = func(x request.Notice) { n = x }
		if _, err := ensure(t, context.Background(), e.opts); err != nil {
			t.Fatal(err)
		}
		if len(e.p.Filed()) != 1 || n.Resumed {
			t.Errorf("filed %d, resumed %v", len(e.p.Filed()), n.Resumed)
		}
	})
}

// T-25: the next lock holder removes the temporary files of this token and of
// its pending file, and no others.
func TestT25_LockHolderRemovesTemps(t *testing.T) {
	e := newEnv(t)
	dir := filepath.Dir(e.token)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	temps := []string{".token.tmp-123", ".token.pending.tmp-456"}
	keep := []string{".other.tmp-789", "token.tmp-1"}
	for _, n := range append(append([]string{}, temps...), keep...) {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	e.p.Script(keyclienttest.Approved())
	if _, err := ensure(t, context.Background(), e.opts); err != nil {
		t.Fatal(err)
	}
	for _, n := range temps {
		if _, err := os.Lstat(filepath.Join(dir, n)); err == nil {
			t.Errorf("%s remains", n)
		}
	}
	for _, n := range keep {
		if _, err := os.Lstat(filepath.Join(dir, n)); err != nil {
			t.Errorf("%s was removed: %v", n, err)
		}
	}
}

// T-1, T-27: with T and P both on disk, Ensure returns the token and touches
// nothing (no lock file is made, P stays).
func TestT27_TokenWithLeftoverPendingTouchesNothing(t *testing.T) {
	e := newEnv(t)
	writePending(t, e, 0o600, pendingJSON(e, time.Now().Add(time.Hour), e.p.URL()+"/v1/requests/old/poll"))
	if err := os.WriteFile(e.token, []byte("on-disk-token-0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, wait := range []bool{true, false} {
		e.opts.Wait = wait
		res, err := ensure(t, context.Background(), e.opts)
		if err != nil || res.Token != "on-disk-token-0123456789" {
			t.Fatalf("Wait=%v: res=%+v err=%v", wait, res, err)
		}
	}
	if !e.pendingExists() {
		t.Error("T-1 removed the pending file")
	}
	if _, err := os.Lstat(e.token + ".lock"); err == nil {
		t.Error("T-1 made the lock file")
	}
	if len(e.p.Polls()) != 0 || len(e.p.Filed()) != 0 {
		t.Error("the provider was contacted")
	}
}

// Notice.Service: the host (and port) of the service the key is for, from the
// configuration: ServiceURL, or the Override's host when Override is used. A
// resumed notice takes it from the pending file.
func TestNoticeService(t *testing.T) {
	e := newEnv(t)
	u, _ := url.Parse(e.p.URL())
	n := fileAndStop(t, e)
	if n.Service != u.Host {
		t.Errorf("Service = %q, want %q", n.Service, u.Host)
	}
	if !strings.Contains(n.String(), u.Host) {
		t.Errorf("String() lacks the service: %s", n)
	}
	e.p.SetInterval(50)
	e.opts.Wait = false
	ctx, cancel := context.WithCancel(context.Background())
	_, err := ensure(t, ctx, e.opts)
	cancel()
	var ep *request.ErrPending
	if !errors.As(err, &ep) || !ep.Notice.Resumed || ep.Notice.Service != u.Host {
		t.Errorf("resumed: err = %v", err)
	}
	if ep != nil {
		waitDone(t, ep)
	}

	e = newEnv(t)
	e.opts.ServiceURL = "https://service.example:8443"
	e.opts.Override = e.p.RequestURL()
	u, _ = url.Parse(e.p.URL())
	n = fileAndStop(t, e)
	if n.Service != u.Host {
		t.Errorf("with Override: Service = %q, want the Override's host %q", n.Service, u.Host)
	}
}

// pctEncode percent-encodes every byte of s.
func pctEncode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		fmt.Fprintf(&b, "%%%02X", s[i])
	}
	return b.String()
}

// T-I7, T-I8: an approve_url that holds the poll secret URL-escaped is refused,
// in a 201 and in a resumed pending file, even with a broken escape beside it
// or escaped twice.
func TestTI7_EscapedSecretInApproveURLRefused(t *testing.T) {
	const loop = "http://127.0.0.1:1"
	const sec = "s3cr3t-s3cr3t"
	enc := pctEncode(sec)
	twice := strings.ReplaceAll(enc, "%", "%25")
	for name, au := range map[string]string{
		"query":                loop + "/a?s=" + enc,
		"path":                 loop + "/a/" + enc,
		"with a broken escape": loop + "/a?s=" + enc + "&x=%zz",
		"partly escaped":       loop + "/a?s=" + strings.Replace(enc, "%2D", "-", 1),
		"escaped twice":        loop + "/a?s=" + twice,
	} {
		e := newEnv(t)
		e.p.FileWith(keyclienttest.Status(201, created201(loop+"/poll", loop+"/a", func(m map[string]any) { m["approve_url"] = au })))
		notified := false
		e.opts.Notify = func(request.Notice) { notified = true }
		_, err := ensure(t, context.Background(), e.opts)
		if err == nil || notified || e.pendingExists() {
			t.Errorf("201 %s: err = %v notified = %v", name, err, notified)
		}

		e = newEnv(t)
		writePending(t, e, 0o600, pendingJSON(e, time.Now().Add(time.Hour), e.p.URL()+"/v1/requests/old/poll", func(m map[string]any) {
			m["poll_secret"] = sec
			m["approve_url"] = au
		}))
		_, err = ensure(t, context.Background(), e.opts)
		if err == nil || !strings.Contains(err.Error(), "cannot be used") || len(e.p.Polls()) != 0 {
			t.Errorf("pending %s: err = %v polls = %d", name, err, len(e.p.Polls()))
		}
	}
}

// T-11 with the 24-hour client limit: a resumed pending file whose expires_at
// is far away is treated as expiring at most 24 hours from now.
func TestT11_ResumedExpiryClamped(t *testing.T) {
	e := newEnv(t) // unit is 40ms, so "24 hours" is 86400 units
	writePending(t, e, 0o600, pendingJSON(e, time.Now().Add(1000*time.Hour), e.p.URL()+"/v1/requests/old/poll"))
	ctx, cancel := context.WithCancel(context.Background())
	var n request.Notice
	e.opts.Notify = func(x request.Notice) { n = x; cancel() }
	ensure(t, ctx, e.opts)
	if !n.Resumed {
		t.Fatalf("notice = %+v", n)
	}
	if limit := time.Now().Add(86400 * unit); n.ExpiresAt.After(limit) {
		t.Errorf("ExpiresAt %v is after the 24-unit-hours limit %v", n.ExpiresAt, limit)
	}
}

// T-11, T-I8: the URLs of a resumed request pass the transport rule again,
// downgrade included; nothing is polled otherwise.
func TestTI8_ResumedURLsKeepTheTransportRule(t *testing.T) {
	for _, c := range []struct {
		name    string
		service string
		edit    func(map[string]any)
	}{
		{"http request_url off loopback", "", func(m map[string]any) { m["request_url"] = "http://provider.example/requests" }},
		{"http poll_url off loopback", "", func(m map[string]any) { m["poll_url"] = "http://provider.example/poll" }},
		{"http approve_url off loopback", "", func(m map[string]any) { m["approve_url"] = "http://provider.example/a" }},
		{"https service, http loopback poll_url", "https://127.0.0.1:1", func(m map[string]any) {
			m["request_url"] = "https://127.0.0.1:1/requests"
			m["approve_url"] = "https://127.0.0.1:1/a"
		}},
		{"https request_url, http loopback approve_url", "", func(m map[string]any) {
			m["request_url"] = "https://127.0.0.1:1/requests"
		}},
	} {
		e := newEnv(t)
		if c.service != "" {
			e.opts.ServiceURL = c.service
		}
		writePending(t, e, 0o600, pendingJSON(e, time.Now().Add(time.Hour), e.p.URL()+"/v1/requests/old/poll", c.edit))
		notified := false
		e.opts.Notify = func(request.Notice) { notified = true }
		_, err := ensure(t, context.Background(), e.opts)
		if err == nil || !strings.Contains(err.Error(), "cannot be used") || notified || len(e.p.Polls()) != 0 || len(e.p.Filed()) != 0 {
			t.Errorf("%s: err = %v notified = %v polls = %d filed = %d", c.name, err, notified, len(e.p.Polls()), len(e.p.Filed()))
		}
	}
}

// Host defaults to the short hostname; an empty or broken result is an error
// that names Host.
func TestHostRequired(t *testing.T) {
	for name, hn := range map[string]func() (string, error){
		"empty":      func() (string, error) { return "", nil },
		"dot first":  func() (string, error) { return ".example", nil },
		"bad":        func() (string, error) { return "bad host", nil },
		"unknown":    func() (string, error) { return "", errors.New("no name") },
		"over 64":    func() (string, error) { return strings.Repeat("h", 65), nil },
		"control ch": func() (string, error) { return "h\x1bost", nil },
	} {
		e := newEnv(t)
		t.Cleanup(request.SetHostname(hn))
		e.opts.Host = ""
		_, err := ensure(t, context.Background(), e.opts)
		if err == nil || !strings.Contains(err.Error(), "Host") || strings.ContainsRune(err.Error(), 0x1b) {
			t.Errorf("%s: err = %v", name, err)
		}
		if len(e.p.Filed()) != 0 {
			t.Errorf("%s: filed", name)
		}
	}
	e := newEnv(t)
	t.Cleanup(request.SetHostname(func() (string, error) { return "box.example.org", nil }))
	e.opts.Host = ""
	e.p.Script(keyclienttest.Approved())
	if _, err := ensure(t, context.Background(), e.opts); err != nil {
		t.Fatal(err)
	}
	if f := e.p.Filed(); len(f) != 1 || f[0].Host != "box" {
		t.Errorf("filed = %+v", f)
	}
}

// T-14: a panic in Notify reaches the owner's caller; every joined caller gets
// an error (never an empty token with a nil error); the lock is released and P
// kept.
func TestT14_NotifyPanic(t *testing.T) {
	e := newEnv(t)
	entered := make(chan struct{})
	owner := e.opts
	owner.Notify = func(request.Notice) {
		close(entered)
		time.Sleep(200 * time.Millisecond)
		panic("boom in Notify")
	}
	recovered := make(chan any, 1)
	go func() {
		defer func() { recovered <- recover() }()
		request.Ensure(context.Background(), owner)
	}()
	<-entered
	type out struct {
		res request.Result
		err error
	}
	joined := make(chan out, 2)
	for _, wait := range []bool{true, false} {
		o := e.opts
		o.Wait = wait
		go func() {
			res, err := request.Ensure(context.Background(), o)
			if ep := (*request.ErrPending)(nil); errors.As(err, &ep) {
				<-ep.Done()
				err = ep.Err()
			}
			joined <- out{res, err}
		}()
	}
	if r := <-recovered; r != "boom in Notify" {
		t.Errorf("the owner's caller got panic %v, want the Notify panic", r)
	}
	for range 2 {
		select {
		case got := <-joined:
			if got.err == nil {
				t.Errorf("a joined caller got res=%+v and a nil error", got.res)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a joined caller hung")
		}
	}
	if !e.pendingExists() {
		t.Error("P was removed")
	}
	// The lock was released: the next run resumes at once.
	e.p.Script(keyclienttest.Approved())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if res, err := ensure(t, ctx, e.opts); err != nil || res.Token != e.p.Key() {
		t.Errorf("after the panic: res=%+v err=%v", res, err)
	}
}

// countingTransport counts the requests a client sends.
type countingTransport struct{ n atomic.Int32 }

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.n.Add(1)
	return http.DefaultTransport.RoundTrip(r)
}
