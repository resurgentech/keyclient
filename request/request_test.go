package request_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/resurgentech/keyclient/keyclienttest"
	"github.com/resurgentech/keyclient/request"
)

// unit is the scaled second of the fast tests.
const unit = 40 * time.Millisecond

type env struct {
	t     *testing.T
	p     *keyclienttest.Provider
	token string
	opts  request.Options
}

// newEnv makes a provider, a token path in a directory that does not exist yet,
// and options that use them. Intervals are scaled to unit.
func newEnv(t *testing.T) *env {
	t.Helper()
	t.Cleanup(request.SetSecond(unit))
	p := keyclienttest.NewProvider(t)
	token := filepath.Join(t.TempDir(), "conf", "token")
	return &env{t: t, p: p, token: token, opts: request.Options{
		ServiceURL: p.URL(),
		TokenPath:  token,
		Role:       "agent",
		NameHint:   "alice@laptop",
		Host:       "laptop",
		Tool:       "mytool/0.3.0",
		Wait:       true,
	}}
}

// noLeak fails if any of the errors, or the strings, holds the poll secret or the key.
func (e *env) noLeak(vals ...any) {
	e.t.Helper()
	for _, v := range vals {
		var s string
		switch x := v.(type) {
		case nil:
			continue
		case error:
			s = x.Error()
		case fmt.Stringer:
			s = x.String()
		case string:
			s = x
		}
		for _, secret := range []string{e.p.PollSecret(), e.p.Key()} {
			if secret != "" && strings.Contains(s, secret) {
				e.t.Errorf("secret leaked in %q", s)
			}
		}
	}
}

func (e *env) pendingExists() bool {
	_, err := os.Stat(request.PendingPath(e.token))
	return err == nil
}

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

func waitDone(t *testing.T, ep *request.ErrPending) {
	t.Helper()
	select {
	case <-ep.Done():
	case <-time.After(20 * time.Second):
		t.Fatal("background poll did not end")
	}
}

// background starts Ensure with Wait=false and returns the pending value; the
// goroutine is cancelled and awaited at cleanup.
func (e *env) background(ctx context.Context) *request.ErrPending {
	e.t.Helper()
	e.opts.Wait = false
	_, err := request.Ensure(ctx, e.opts)
	var ep *request.ErrPending
	if !errors.As(err, &ep) {
		e.t.Fatalf("Ensure(Wait=false) = %v, want *ErrPending", err)
	}
	e.t.Cleanup(func() { waitDone(e.t, ep) })
	return ep
}

func TestDiscoveryOverrideWins(t *testing.T) {
	e := newEnv(t)
	var hits atomic.Int32
	disc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(404)
	}))
	defer disc.Close()
	e.p.SetDiscovery(false)
	e.p.Script(keyclienttest.Approved())
	e.opts.ServiceURL = disc.URL
	e.opts.Override = e.p.RequestURL()
	res, err := request.Ensure(context.Background(), e.opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.Token != e.p.Key() {
		t.Errorf("token = %q", res.Token)
	}
	if hits.Load() != 0 {
		t.Errorf("discovery called %d times with an Override", hits.Load())
	}
}

func TestDiscoveryUsed(t *testing.T) {
	e := newEnv(t)
	e.opts.ServiceURL = e.p.URL() + "/" // trailing slash trimmed
	e.p.Script(keyclienttest.Approved())
	if _, err := request.Ensure(context.Background(), e.opts); err != nil {
		t.Fatal(err)
	}
	if len(e.p.Filed()) != 1 {
		t.Errorf("filed = %d", len(e.p.Filed()))
	}
}

func TestNoProvider(t *testing.T) {
	e := newEnv(t)
	e.p.SetDiscovery(false)
	_, err := request.Ensure(context.Background(), e.opts)
	if !errors.Is(err, request.ErrNoProvider) {
		t.Errorf("404 discovery: err = %v", err)
	}
	e.opts.ServiceURL = ""
	_, err = request.Ensure(context.Background(), e.opts)
	if !errors.Is(err, request.ErrNoProvider) {
		t.Errorf("no ServiceURL, no Override: err = %v", err)
	}
	if len(e.p.Filed()) != 0 {
		t.Error("something was filed")
	}
	if e.pendingExists() {
		t.Error("pending file written")
	}
}

func TestDiscoveryFailures(t *testing.T) {
	for name, h := range map[string]http.HandlerFunc{
		"500":        func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) },
		"bad json":   func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("{nope")) },
		"not a url":  func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"request_url":"/relative"}`)) },
		"ftp":        func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"request_url":"ftp://host/x"}`)) },
		"empty":      func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{}`)) },
		"redirected": func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/elsewhere", 302) },
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			srv := httptest.NewServer(h)
			defer srv.Close()
			e.opts.ServiceURL = srv.URL
			_, err := request.Ensure(context.Background(), e.opts)
			if err == nil || errors.Is(err, request.ErrNoProvider) {
				t.Errorf("err = %v, want a discovery error", err)
			}
			if len(e.p.Filed()) != 0 || e.pendingExists() {
				t.Error("something was filed or written")
			}
		})
	}
	t.Run("network error", func(t *testing.T) {
		e := newEnv(t)
		e.opts.ServiceURL = "http://127.0.0.1:1"
		_, err := request.Ensure(context.Background(), e.opts)
		if err == nil || errors.Is(err, request.ErrNoProvider) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("bad override", func(t *testing.T) {
		e := newEnv(t)
		for _, o := range []string{"/relative", "ftp://x/y", "http://", "not a url"} {
			e.opts.Override = o
			if _, err := request.Ensure(context.Background(), e.opts); err == nil || errors.Is(err, request.ErrNoProvider) {
				t.Errorf("Override %q: err = %v", o, err)
			}
		}
		if len(e.p.Filed()) != 0 {
			t.Error("filed")
		}
	})
}

func TestFilingBodyAndDefaultHost(t *testing.T) {
	e := newEnv(t)
	e.p.Script(keyclienttest.Approved())
	if _, err := request.Ensure(context.Background(), e.opts); err != nil {
		t.Fatal(err)
	}
	f := e.p.Filed()
	if len(f) != 1 || f[0].NameHint != "alice@laptop" || f[0].Host != "laptop" || f[0].Tool != "mytool/0.3.0" || f[0].Role != "agent" {
		t.Errorf("filed = %+v", f)
	}

	e = newEnv(t)
	e.opts.Host = ""
	e.p.Script(keyclienttest.Approved())
	if _, err := request.Ensure(context.Background(), e.opts); err != nil {
		t.Fatal(err)
	}
	full, _ := os.Hostname()
	want, _, _ := strings.Cut(full, ".")
	if f := e.p.Filed(); len(f) != 1 || f[0].Host != want {
		t.Errorf("default host = %+v, want %q", f, want)
	}
}

func TestInvalidOptionsBeforeAnyRequest(t *testing.T) {
	long := strings.Repeat("a", 65)
	cases := []struct {
		field string
		edit  func(*request.Options)
	}{
		{"TokenPath", func(o *request.Options) { o.TokenPath = "" }},
		{"Role", func(o *request.Options) { o.Role = "" }},
		{"Role", func(o *request.Options) { o.Role = "ro le" }},
		{"Role", func(o *request.Options) { o.Role = strings.Repeat("r", 33) }},
		{"NameHint", func(o *request.Options) { o.NameHint = "" }},
		{"NameHint", func(o *request.Options) { o.NameHint = "alice laptop" }},
		{"NameHint", func(o *request.Options) { o.NameHint = long }},
		{"Tool", func(o *request.Options) { o.Tool = "" }},
		{"Tool", func(o *request.Options) { o.Tool = "my tool" }},
		{"Host", func(o *request.Options) { o.Host = "bad host" }},
		{"Host", func(o *request.Options) { o.Host = long }},
	}
	for _, c := range cases {
		e := newEnv(t)
		e.opts.ServiceURL = "http://127.0.0.1:1"
		c.edit(&e.opts)
		_, err := request.Ensure(context.Background(), e.opts)
		if err == nil || !strings.Contains(err.Error(), c.field) {
			t.Errorf("%s: err = %v", c.field, err)
		}
		if errors.Is(err, request.ErrNoProvider) {
			t.Errorf("%s: validation must come before discovery", c.field)
		}
	}
}

func TestProviderErrors(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.p.FileWith(keyclienttest.Status(400, `{"error":"bad_request","message":"role not offered"}`))
	_, err := request.Ensure(ctx, e.opts)
	var pe *request.ProviderError
	if !errors.As(err, &pe) || pe.Status != 400 || pe.Code != "bad_request" || pe.Message != "role not offered" || pe.RetryAfter != 0 {
		t.Fatalf("400: err = %#v", err)
	}
	if !strings.Contains(err.Error(), "role not offered") {
		t.Errorf("Error() = %q", err)
	}

	e.p.FileWith(keyclienttest.Status(429, `{"error":"rate_limited","message":"slow"}`).WithHeader("Retry-After", "7"))
	_, err = request.Ensure(ctx, e.opts)
	if !errors.As(err, &pe) || pe.Status != 429 || pe.RetryAfter != 7*time.Second {
		t.Errorf("429: err = %#v", err)
	}

	e.p.FileWith(keyclienttest.Status(503, `{"error":"capacity","message":"at capacity"}`))
	_, err = request.Ensure(ctx, e.opts)
	if !errors.As(err, &pe) || pe.Status != 503 || pe.Message != "at capacity" {
		t.Errorf("503: err = %#v", err)
	}

	// A body that is not an error object still gives a ProviderError.
	e.p.FileWith(keyclienttest.Status(502, "<html>bad gateway</html>"))
	_, err = request.Ensure(ctx, e.opts)
	if !errors.As(err, &pe) || pe.Status != 502 {
		t.Errorf("502: err = %#v", err)
	}
	if e.pendingExists() {
		t.Error("pending file after a refused filing")
	}
}

func TestScenarios(t *testing.T) {
	t.Run("approved", func(t *testing.T) {
		e := newEnv(t)
		e.p.Script(keyclienttest.Scenario("approved")...)
		res, err := request.Ensure(context.Background(), e.opts)
		if err != nil {
			t.Fatal(err)
		}
		if res.Token != e.p.Key() || res.Name != "alice@laptop" || res.Role != "agent" {
			t.Errorf("result = %+v", res)
		}
		b, err := os.ReadFile(e.token)
		if err != nil || string(b) != e.p.Key() {
			t.Errorf("token file = %q, %v", b, err)
		}
		if m := mode(t, e.token); m != 0o600 {
			t.Errorf("token mode = %v", m)
		}
		if m := mode(t, filepath.Dir(e.token)); m != 0o700 {
			t.Errorf("directory mode = %v", m)
		}
		if e.pendingExists() {
			t.Error("pending file remains")
		}
		if n := len(e.p.Polls()); n != 4 {
			t.Errorf("polls = %d, want 4 (the scenario's consumed step is not polled)", n)
		}
	})
	for name, want := range map[string]error{
		"denied":       request.ErrDenied,
		"expired":      request.ErrExpired,
		"lost-reply":   request.ErrConsumed,
		"unauthorized": request.ErrUnauthorized,
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.p.Script(keyclienttest.Scenario(name)...)
			res, err := request.Ensure(context.Background(), e.opts)
			if res != (request.Result{}) || !errors.Is(err, want) {
				t.Fatalf("res=%+v err=%v, want %v", res, err, want)
			}
			e.noLeak(err)
			if _, err := os.Stat(e.token); err == nil {
				t.Error("token file written")
			}
			if want == request.ErrUnauthorized {
				if !e.pendingExists() {
					t.Error("401 must keep the pending file")
				}
			} else if e.pendingExists() {
				t.Error("pending file remains after a final status")
			}
		})
	}
}

func TestPollsHonourInterval(t *testing.T) {
	e := newEnv(t)
	e.p.SetInterval(2)
	e.p.Script(keyclienttest.Pending(), keyclienttest.Pending(), keyclienttest.Approved())
	if _, err := request.Ensure(context.Background(), e.opts); err != nil {
		t.Fatal(err)
	}
	f, polls := e.p.Filed(), e.p.Polls()
	if len(polls) != 3 {
		t.Fatalf("polls = %d", len(polls))
	}
	iv := 2 * unit
	if gap := polls[0].Time.Sub(f[0].Time); gap < iv {
		t.Errorf("first poll %v after filing, want >= %v", gap, iv)
	}
	for i := 1; i < len(polls); i++ {
		if gap := polls[i].Time.Sub(polls[i-1].Time); gap < iv {
			t.Errorf("gap %d = %v, want >= %v", i, gap, iv)
		}
	}
}

func TestSlowDownRaisesInterval(t *testing.T) {
	e := newEnv(t) // interval 1
	e.p.Script(keyclienttest.SlowDown(), keyclienttest.Pending(), keyclienttest.Approved())
	if _, err := request.Ensure(context.Background(), e.opts); err != nil {
		t.Fatal(err)
	}
	polls := e.p.Polls()
	if len(polls) != 3 {
		t.Fatalf("polls = %d", len(polls))
	}
	if gap := polls[1].Time.Sub(polls[0].Time); gap < 3*unit {
		t.Errorf("gap after slow_down = %v, want >= %v", gap, 3*unit)
	}
	if gap := polls[2].Time.Sub(polls[1].Time); gap < 3*unit {
		t.Errorf("later gap = %v, want >= %v", gap, 3*unit)
	}
}

func TestSlowDownUnreadableBody(t *testing.T) {
	e := newEnv(t)
	e.p.Script(keyclienttest.Status(429, "garbage"), keyclienttest.Approved())
	if _, err := request.Ensure(context.Background(), e.opts); err != nil {
		t.Fatal(err)
	}
	polls := e.p.Polls()
	if gap := polls[1].Time.Sub(polls[0].Time); gap < 3*unit {
		t.Errorf("gap = %v, want >= interval+2 = %v", gap, 3*unit)
	}
}

func TestStopsAtExpiry(t *testing.T) {
	e := newEnv(t)
	e.p.SetExpiresIn(3)
	start := time.Now()
	_, err := request.Ensure(context.Background(), e.opts)
	if !errors.Is(err, request.ErrExpired) {
		t.Fatalf("err = %v", err)
	}
	if d := time.Since(start); d > 10*unit {
		t.Errorf("took %v, want about the expiry", d)
	}
	if e.pendingExists() {
		t.Error("pending file remains after expiry")
	}
	// Polls at 1, 2 and 3 units: the last one at the expiry, none after it.
	polls := e.p.Polls()
	if len(polls) != 3 {
		t.Errorf("polls = %d, want 3", len(polls))
	}
	for _, p := range polls {
		if p.Time.Sub(e.p.Filed()[0].Time) >= 3*unit+unit/2 {
			t.Error("polled after expiry")
		}
	}
}

func TestTransientFailuresKeepPolling(t *testing.T) {
	e := newEnv(t)
	e.p.Script(keyclienttest.Status(500, ""), keyclienttest.Status(503, `{"error":"capacity","message":"x"}`), keyclienttest.Approved())
	res, err := request.Ensure(context.Background(), e.opts)
	if err != nil || res.Token != e.p.Key() {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if n := len(e.p.Polls()); n != 3 {
		t.Errorf("polls = %d", n)
	}
}

// failFirstPoll is a transport that fails the first poll with a connection error.
type failFirstPoll struct{ n atomic.Int32 }

func (f *failFirstPoll) RoundTrip(r *http.Request) (*http.Response, error) {
	if strings.HasSuffix(r.URL.Path, "/poll") && f.n.Add(1) == 1 {
		return nil, errors.New("connection refused")
	}
	return http.DefaultTransport.RoundTrip(r)
}

func TestConnectionFailureKeepsPolling(t *testing.T) {
	e := newEnv(t)
	e.opts.HTTPClient = &http.Client{Transport: &failFirstPoll{}}
	e.p.Script(keyclienttest.Approved())
	res, err := request.Ensure(context.Background(), e.opts)
	if err != nil || res.Token != e.p.Key() {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestOtherStatusStopsAndKeepsPending(t *testing.T) {
	for _, o := range []keyclienttest.Outcome{
		keyclienttest.Status(302, "").WithHeader("Location", "http://127.0.0.1:1/elsewhere"),
		keyclienttest.Status(418, `{"message":"teapot"}`),
	} {
		e := newEnv(t)
		e.p.Script(o)
		_, err := request.Ensure(context.Background(), e.opts)
		if err == nil {
			t.Fatal("no error")
		}
		e.noLeak(err)
		if !e.pendingExists() {
			t.Error("pending file removed")
		}
		if n := len(e.p.Polls()); n != 1 {
			t.Errorf("polls = %d, want 1 (redirects not followed, no more polling)", n)
		}
		for _, code := range []string{"302", "418"} {
			if strings.Contains(err.Error(), code) {
				goto ok
			}
		}
		t.Errorf("error does not name the status: %v", err)
	ok:
	}
}

func TestRedirectNotFollowedAtFiling(t *testing.T) {
	e := newEnv(t)
	e.p.FileWith(keyclienttest.Status(307, "").WithHeader("Location", e.p.RequestURL()))
	_, err := request.Ensure(context.Background(), e.opts)
	if err == nil || len(e.p.Filed()) != 1 {
		t.Fatalf("err = %v, filed = %d", err, len(e.p.Filed()))
	}
	if e.pendingExists() {
		t.Error("pending written")
	}
}

func TestPendingFile(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	var notices []request.Notice
	e.opts.Notify = func(n request.Notice) { notices = append(notices, n) }
	ep := e.background(ctx)
	pp := request.PendingPath(e.token)
	if pp != e.token+".pending" {
		t.Errorf("PendingPath = %q", pp)
	}
	if m := mode(t, pp); m != 0o600 {
		t.Errorf("pending mode = %v", m)
	}
	b, _ := os.ReadFile(pp)
	if !strings.Contains(string(b), e.p.PollSecret()) {
		t.Error("pending file lacks the poll secret")
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"request_id", "poll_url", "poll_secret", "approve_url", "user_code", "role", "tool", "interval", "expires_at", "service_url", "override", "request_url"} {
		if _, ok := m[k]; !ok {
			t.Errorf("pending file lacks %q", k)
		}
	}
	if len(notices) != 1 || notices[0].Resumed {
		t.Fatalf("notices = %+v", notices)
	}

	// A fresh "process": the first poll ends by cancellation, the file stays.
	cancel()
	waitDone(t, ep)
	if !errors.Is(ep.Err(), context.Canceled) {
		t.Errorf("Err = %v", ep.Err())
	}
	e.p.Script(keyclienttest.Approved())
	ep2 := e.background(context.Background())
	if len(e.p.Filed()) != 1 {
		t.Errorf("filed = %d, the pending request must be resumed", len(e.p.Filed()))
	}
	if len(notices) != 2 || !notices[1].Resumed || notices[1].UserCode != notices[0].UserCode {
		t.Errorf("notices = %+v", notices)
	}
	waitDone(t, ep2)
	if ep2.Err() != nil || !ep2.Ready() {
		t.Errorf("Err = %v Ready = %v", ep2.Err(), ep2.Ready())
	}
	if b, _ := os.ReadFile(e.token); string(b) != e.p.Key() {
		t.Error("token not stored by the resumed request")
	}
	if e.pendingExists() {
		t.Error("pending remains")
	}
}

func writePending(t *testing.T, e *env, mode os.FileMode, content string) {
	t.Helper()
	pp := request.PendingPath(e.token)
	if err := os.MkdirAll(filepath.Dir(pp), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pp, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(pp, mode); err != nil {
		t.Fatal(err)
	}
}

// pendingJSON is a pending file for a request filed with e's current options,
// so it is resumed unless it has expired or something in it is edited.
func pendingJSON(e *env, expires time.Time, pollURL string, edit ...func(map[string]any)) string {
	m := map[string]any{
		"request_id": "old", "poll_url": pollURL, "poll_secret": "old-secret-old-secret",
		"approve_url": "http://127.0.0.1:1/a", "user_code": "AAAA-BBBB", "role": "agent",
		"tool": "mytool/0.3.0", "interval": 1, "expires_at": expires.Format(time.RFC3339Nano),
		"service_url": strings.TrimRight(e.opts.ServiceURL, "/"), "override": e.opts.Override,
		"request_url": e.p.RequestURL(),
	}
	for _, f := range edit {
		f(m)
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func TestExpiredPendingReplaced(t *testing.T) {
	e := newEnv(t)
	writePending(t, e, 0o600, pendingJSON(e, time.Now().Add(-time.Hour), "http://127.0.0.1:1/poll"))
	e.p.Script(keyclienttest.Approved())
	if _, err := request.Ensure(context.Background(), e.opts); err != nil {
		t.Fatal(err)
	}
	if len(e.p.Filed()) != 1 {
		t.Errorf("filed = %d, want a new filing", len(e.p.Filed()))
	}
}

func TestResumeNeedsNoDiscovery(t *testing.T) {
	e := newEnv(t)
	// The pending file points at the provider's poll URL for a request it knows nothing about: 401, kept.
	// Its service is one that does not answer, so discovery would fail.
	e.opts.ServiceURL = "http://127.0.0.1:1"
	writePending(t, e, 0o600, pendingJSON(e, time.Now().Add(time.Hour), e.p.URL()+"/v1/requests/old/poll"))
	var got request.Notice
	e.opts.Notify = func(n request.Notice) { got = n }
	_, err := request.Ensure(context.Background(), e.opts)
	if !errors.Is(err, request.ErrUnauthorized) {
		t.Fatalf("err = %v", err)
	}
	if !got.Resumed || got.UserCode != "AAAA-BBBB" {
		t.Errorf("notice = %+v", got)
	}
	if len(e.p.Filed()) != 0 || !e.pendingExists() {
		t.Error("filed, or pending removed")
	}
}

func TestPendingRefused(t *testing.T) {
	e := newEnv(t)
	writePending(t, e, 0o640, pendingJSON(e, time.Now().Add(time.Hour), e.p.URL()+"/poll"))
	_, err := request.Ensure(context.Background(), e.opts)
	if err == nil || !strings.Contains(err.Error(), request.PendingPath(e.token)) {
		t.Errorf("group-readable: err = %v", err)
	}
	if len(e.p.Filed()) != 0 {
		t.Error("filed")
	}

	e = newEnv(t)
	writePending(t, e, 0o600, `{"poll_secret": "SECRET-NOT-TO-PRINT" oops`)
	_, err = request.Ensure(context.Background(), e.opts)
	if err == nil || !strings.Contains(err.Error(), request.PendingPath(e.token)) || strings.Contains(err.Error(), "SECRET-NOT-TO-PRINT") {
		t.Errorf("corrupt: err = %v", err)
	}
	if len(e.p.Filed()) != 0 {
		t.Error("filed")
	}
}

func TestBackground(t *testing.T) {
	e := newEnv(t)
	var notified atomic.Int32
	e.opts.Notify = func(request.Notice) { notified.Add(1) }
	e.p.SetInterval(2)
	ctx := context.Background()

	e.opts.Wait = false
	res, err := request.Ensure(ctx, e.opts)
	var ep *request.ErrPending
	if res != (request.Result{}) || !errors.As(err, &ep) {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	t.Cleanup(func() { waitDone(t, ep) })
	n := ep.Notice
	if n.UserCode == "" || n.ApproveURL == "" || n.Role != "agent" || n.Tool != "mytool/0.3.0" || n.Resumed || n.ExpiresAt.IsZero() {
		t.Errorf("notice = %+v", n)
	}
	for _, want := range []string{n.ApproveURL, n.UserCode, "agent", "mytool/0.3.0"} {
		if !strings.Contains(n.String(), want) || !strings.Contains(ep.Error(), want) {
			t.Errorf("message lacks %q: %s", want, n)
		}
	}
	if !strings.HasPrefix(ep.Error(), "key request pending: ") {
		t.Errorf("Error() = %q", ep.Error())
	}
	e.noLeak(ep, n)
	if ep.Ready() || ep.Err() != nil {
		t.Error("Ready/Err before the end")
	}
	select {
	case <-ep.Done():
		t.Fatal("Done closed early")
	default:
	}

	// A second Ensure in the same process joins the poll: same value, no notice, no filing.
	_, err2 := request.Ensure(ctx, e.opts)
	var ep2 *request.ErrPending
	if !errors.As(err2, &ep2) || ep2 != ep {
		t.Errorf("second Ensure = %v, want the same *ErrPending", err2)
	}
	if notified.Load() != 1 || len(e.p.Filed()) != 1 {
		t.Errorf("notified %d, filed %d", notified.Load(), len(e.p.Filed()))
	}

	e.p.Script(keyclienttest.Approved())
	waitDone(t, ep)
	if !ep.Ready() || ep.Err() != nil {
		t.Errorf("Ready=%v Err=%v", ep.Ready(), ep.Err())
	}
	if b, _ := os.ReadFile(e.token); string(b) != e.p.Key() {
		t.Error("token not written")
	}
	// Now the token is there: Ensure returns it, no request, no name or role.
	res, err = request.Ensure(ctx, e.opts)
	if err != nil || res != (request.Result{Token: e.p.Key()}) {
		t.Errorf("after: res=%+v err=%v", res, err)
	}
	if notified.Load() != 1 || len(e.p.Filed()) != 1 {
		t.Error("a request was made with a token present")
	}
}

func TestWaitJoinsBackground(t *testing.T) {
	e := newEnv(t)
	ep := e.background(context.Background())
	e.p.Script(keyclienttest.Approved())
	e.opts.Wait = true
	res, err := request.Ensure(context.Background(), e.opts)
	if err != nil || res.Token != e.p.Key() {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	waitDone(t, ep)
	if len(e.p.Filed()) != 1 {
		t.Errorf("filed = %d", len(e.p.Filed()))
	}
}

func TestBackgroundDeniedThenNewRequest(t *testing.T) {
	e := newEnv(t)
	e.p.Script(keyclienttest.Denied())
	ep := e.background(context.Background())
	waitDone(t, ep)
	if !errors.Is(ep.Err(), request.ErrDenied) || ep.Ready() {
		t.Errorf("Err = %v Ready = %v", ep.Err(), ep.Ready())
	}
	if e.pendingExists() {
		t.Error("pending remains")
	}
	e.p.Script(keyclienttest.Approved())
	ep2 := e.background(context.Background())
	waitDone(t, ep2)
	if len(e.p.Filed()) != 2 || ep2.Err() != nil {
		t.Errorf("filed = %d, Err = %v", len(e.p.Filed()), ep2.Err())
	}
}

func TestExistingTokenMakesNoRequest(t *testing.T) {
	e := newEnv(t)
	if err := os.MkdirAll(filepath.Dir(e.token), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.token, []byte("  my-token \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.opts.ServiceURL = "http://127.0.0.1:1"
	for _, wait := range []bool{true, false} {
		e.opts.Wait = wait
		res, err := request.Ensure(context.Background(), e.opts)
		if err != nil || res != (request.Result{Token: "my-token"}) {
			t.Errorf("Wait=%v: res=%+v err=%v", wait, res, err)
		}
	}
	// A token on disk wins even over URLs the package would refuse.
	e.opts.ServiceURL = "http://provider.example"
	if res, err := request.Ensure(context.Background(), e.opts); err != nil || res.Token != "my-token" {
		t.Errorf("plain http ServiceURL: res=%+v err=%v", res, err)
	}
	e.opts.ServiceURL = e.p.URL()
	e.opts.Override = e.p.RequestURL()
	if _, err := request.Ensure(context.Background(), e.opts); err != nil {
		t.Fatal(err)
	}
	if len(e.p.Filed()) != 0 || len(e.p.Polls()) != 0 {
		t.Error("the provider was contacted")
	}
}

func TestConsumedButTokenPresent(t *testing.T) {
	e := newEnv(t)
	e.p.Script(keyclienttest.Consumed())
	e.opts.Notify = func(request.Notice) {
		// Another process sharing the pending file collects the key meanwhile.
		if err := os.WriteFile(e.token, []byte("collected-by-other-0123456789\n"), 0o600); err != nil {
			t.Error(err)
		}
	}
	// The directory is created by the pending write.
	res, err := request.Ensure(context.Background(), e.opts)
	if err != nil || res != (request.Result{Token: "collected-by-other-0123456789"}) {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if e.pendingExists() {
		t.Error("pending remains")
	}
}

func TestBadKeyWritesNothing(t *testing.T) {
	for _, key := range []string{"short-key1", "has space in it 0123456789", strings.Repeat("k", 513), "tab\tin-key-0123456789"} {
		e := newEnv(t)
		e.p.Script(keyclienttest.ApprovedWith("alice@laptop", "agent", key))
		res, err := request.Ensure(context.Background(), e.opts)
		if err == nil || res != (request.Result{}) {
			t.Fatalf("key %q: res=%+v err=%v", key, res, err)
		}
		if strings.Contains(err.Error(), key) {
			t.Errorf("error contains the key: %v", err)
		}
		e.noLeak(err)
		if _, err := os.Stat(e.token); err == nil {
			t.Errorf("key %q: token file written", key)
		}
	}
}

func TestTokenWriteFailureRemovesPending(t *testing.T) {
	e := newEnv(t)
	e.p.Script(keyclienttest.Approved())
	// After the pending file is written, a non-empty directory takes the token's
	// place, so the final rename of the key fails.
	e.opts.Notify = func(request.Notice) {
		if err := os.MkdirAll(filepath.Join(e.token, "x"), 0o700); err != nil {
			t.Error(err)
		}
	}
	res, err := request.Ensure(context.Background(), e.opts)
	if err == nil || res != (request.Result{}) {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	e.noLeak(err)
	if e.pendingExists() {
		t.Error("pending remains: the request is consumed")
	}
}

// TestRealSecond runs one request with the real time unit (interval 1).
func TestRealSecond(t *testing.T) {
	p := keyclienttest.NewProvider(t)
	token := filepath.Join(t.TempDir(), "token")
	p.Script(keyclienttest.Approved())
	start := time.Now()
	res, err := request.Ensure(context.Background(), request.Options{
		ServiceURL: p.URL(), TokenPath: token, Role: "agent", NameHint: "alice@laptop", Host: "laptop",
		Tool: "mytool/0.3.0", Wait: true,
	})
	if err != nil || res.Token != p.Key() {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if d := time.Since(start); d < time.Second || d > 5*time.Second {
		t.Errorf("took %v, want about 1s", d)
	}
}

func TestContextCancelWait(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	e.opts.Notify = func(request.Notice) { cancel() }
	_, err := request.Ensure(ctx, e.opts)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v", err)
	}
	if !e.pendingExists() {
		t.Error("pending removed on cancel")
	}
}
