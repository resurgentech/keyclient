package request_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/resurgentech/keyclient/keyclienttest"
	"github.com/resurgentech/keyclient/request"
)

// recorder is a transport that records each request and sends nothing.
type recorder struct {
	mu   sync.Mutex
	reqs []string
}

func (r *recorder) RoundTrip(q *http.Request) (*http.Response, error) {
	r.mu.Lock()
	r.reqs = append(r.reqs, q.Method+" "+q.URL.String())
	r.mu.Unlock()
	return nil, errors.New("recorded, not sent")
}

func (r *recorder) got() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.reqs...)
}

// counting is a listening plain http server that counts the requests it gets.
func counting(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprint(w, `{"status":"pending","interval":1}`)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// created201 is a valid 201 body; edit changes it.
func created201(pollURL, approveURL string, edit ...func(map[string]any)) string {
	m := map[string]any{"request_id": "r1", "poll_url": pollURL, "poll_secret": "s3cr3t-s3cr3t",
		"approve_url": approveURL, "user_code": "AAAA-BBBB", "interval": 1, "expires_in": 60}
	for _, f := range edit {
		f(m)
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func TestPlainHTTPRefusedUnlessLoopback(t *testing.T) {
	for _, c := range []struct {
		field, url string
		loopback   bool // allowed without AllowInsecureHTTP
	}{
		{"ServiceURL", "http://provider.example/", false},
		{"ServiceURL", "http://localhost.provider.example", false},
		{"ServiceURL", "http://127.0.0.1.provider.example", false},
		{"ServiceURL", "http://10.0.0.1", false},
		{"ServiceURL", "http://[::ffff:10.0.0.1]", false},
		{"Override", "http://provider.example/requests", false},
		{"ServiceURL", "http://localhost:1", true},
		{"ServiceURL", "http://LocalHost:1", true},
		{"ServiceURL", "http://127.9.9.9:1", true},
		{"ServiceURL", "http://[::1]:1", true},
		{"Override", "http://127.0.0.1:1/requests", true},
		{"ServiceURL", "https://provider.example", true},
		{"Override", "https://provider.example/requests", true},
	} {
		for _, allow := range []bool{false, true} {
			e := newEnv(t)
			rec := &recorder{}
			e.opts.HTTPClient = &http.Client{Transport: rec}
			e.opts.ServiceURL, e.opts.Override = "", ""
			if c.field == "ServiceURL" {
				e.opts.ServiceURL = c.url
			} else {
				e.opts.Override = c.url
			}
			e.opts.AllowInsecureHTTP = allow
			_, err := request.Ensure(context.Background(), e.opts)
			if err == nil {
				t.Fatalf("%s %s allow=%v: no error", c.field, c.url, allow)
			}
			refused := strings.Contains(err.Error(), "plain http")
			wantRefused := !c.loopback && !allow
			if refused != wantRefused {
				t.Errorf("%s %s allow=%v: err = %v, want refused=%v", c.field, c.url, allow, err, wantRefused)
			}
			if wantRefused {
				if !strings.Contains(err.Error(), c.field) {
					t.Errorf("%s %s: the error does not name the field: %v", c.field, c.url, err)
				}
				if n := len(rec.got()); n != 0 {
					t.Errorf("%s %s: %d requests sent before refusing", c.field, c.url, n)
				}
			} else if got := rec.got(); len(got) != 1 {
				t.Errorf("%s %s allow=%v: requests = %v, want the discovery or filing request", c.field, c.url, allow, got)
			}
		}
	}
}

func TestBadServiceURLBeforeAnyRequest(t *testing.T) {
	for _, u := range []string{"/relative", "provider.example", "ftp://provider.example", "http://"} {
		e := newEnv(t)
		rec := &recorder{}
		e.opts.HTTPClient = &http.Client{Transport: rec}
		e.opts.ServiceURL = u
		_, err := request.Ensure(context.Background(), e.opts)
		if err == nil || !strings.Contains(err.Error(), "ServiceURL") || errors.Is(err, request.ErrNoProvider) {
			t.Errorf("ServiceURL %q: err = %v", u, err)
		}
		if n := len(rec.got()); n != 0 {
			t.Errorf("ServiceURL %q: %d requests sent", u, n)
		}
	}
}

func TestNoDowngrade(t *testing.T) {
	t.Run("https service, http request_url", func(t *testing.T) { // reviewer R5
		plain, hits := counting(t)
		disc := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, `{"request_url":%q}`, plain.URL+"/requests")
		}))
		defer disc.Close()
		for _, allow := range []bool{false, true} {
			e := newEnv(t)
			e.opts.ServiceURL = disc.URL
			e.opts.HTTPClient = disc.Client()
			e.opts.AllowInsecureHTTP = allow
			_, err := request.Ensure(context.Background(), e.opts)
			if err == nil || !strings.Contains(err.Error(), "not https") {
				t.Errorf("allow=%v: err = %v", allow, err)
			}
			if hits.Load() != 0 || e.pendingExists() {
				t.Errorf("allow=%v: filed at the http server (%d) or pending written", allow, hits.Load())
			}
		}
	})
	t.Run("https request URL, http approve_url", func(t *testing.T) {
		var polls atomic.Int32
		var srv *httptest.Server
		srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/poll") {
				polls.Add(1)
				w.WriteHeader(http.StatusForbidden)
				return
			}
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, created201(srv.URL+"/poll", "http://127.0.0.1:1/approve"))
		}))
		defer srv.Close()
		for _, allow := range []bool{false, true} {
			e := newEnv(t)
			e.opts.ServiceURL, e.opts.Override = "", srv.URL+"/requests"
			e.opts.HTTPClient = srv.Client()
			e.opts.AllowInsecureHTTP = allow
			notified := false
			e.opts.Notify = func(request.Notice) { notified = true }
			_, err := request.Ensure(context.Background(), e.opts)
			if err == nil || !strings.Contains(err.Error(), "not https") {
				t.Errorf("allow=%v: err = %v", allow, err)
			}
			if notified || polls.Load() != 0 || e.pendingExists() {
				t.Errorf("allow=%v: notified=%v polls=%d pending=%v", allow, notified, polls.Load(), e.pendingExists())
			}
		}
	})
	t.Run("http service, https request URL, http poll_url", func(t *testing.T) {
		plain, hits := counting(t)
		var reqSrv *httptest.Server
		reqSrv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, created201(plain.URL+"/poll", reqSrv.URL+"/approve"))
		}))
		defer reqSrv.Close()
		disc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, `{"request_url":%q}`, reqSrv.URL+"/requests")
		}))
		defer disc.Close()
		e := newEnv(t)
		e.opts.ServiceURL = disc.URL
		e.opts.HTTPClient = reqSrv.Client()
		_, err := request.Ensure(context.Background(), e.opts)
		if err == nil || !strings.Contains(err.Error(), "not https") {
			t.Errorf("err = %v", err)
		}
		if hits.Load() != 0 || e.pendingExists() {
			t.Errorf("polled the http server (%d) or pending written", hits.Load())
		}
	})
}

// The 201 (over https) points poll_url at a listening plain http server: it
// must get nothing, and Ensure must fail at once, not after an interval.
func TestPollURLMustBeHTTPSWhenRequestURLIs(t *testing.T) {
	plain, hits := counting(t)
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, created201(plain.URL+"/poll", srv.URL+"/approve"))
	}))
	defer srv.Close()
	for _, allow := range []bool{false, true} {
		e := newEnv(t)
		t.Cleanup(request.SetSecond(time.Second)) // the interval is a real second
		e.opts.Override = srv.URL + "/requests"
		e.opts.ServiceURL = ""
		e.opts.HTTPClient = srv.Client()
		e.opts.AllowInsecureHTTP = allow
		start := time.Now()
		_, err := request.Ensure(context.Background(), e.opts)
		if d := time.Since(start); d > 500*time.Millisecond {
			t.Errorf("allow=%v: took %v, want well under one interval (1s)", allow, d)
		}
		if err == nil || !strings.Contains(err.Error(), "not https") || strings.Contains(err.Error(), "s3cr3t") {
			t.Fatalf("allow=%v: err = %v", allow, err)
		}
		if e.pendingExists() || hits.Load() != 0 {
			t.Errorf("allow=%v: pending written or the http server polled (%d)", allow, hits.Load())
		}
	}
}

func TestInvalid201Replies(t *testing.T) {
	const loop = "http://127.0.0.1:1"
	set := func(k string, v any) func(map[string]any) { return func(m map[string]any) { m[k] = v } }
	del := func(k string) func(map[string]any) { return func(m map[string]any) { delete(m, k) } }
	long := loop + "/" + strings.Repeat("a", 2048-len(loop)-1) // exactly 2048 bytes
	for _, c := range []struct {
		name string
		edit func(map[string]any)
	}{
		{"missing request_id", del("request_id")},
		{"missing poll_secret", del("poll_secret")},
		{"missing user_code", del("user_code")},
		{"missing approve_url", del("approve_url")},
		{"interval 0", set("interval", 0)},
		{"interval a string", set("interval", "1")},
		{"expires_in 0", set("expires_in", 0)},
		{"relative poll_url", set("poll_url", "/poll")},
		{"ftp poll_url", set("poll_url", "ftp://127.0.0.1/poll")},
		{"plain http poll_url off loopback", set("poll_url", "http://provider.example/poll")},
		{"relative approve_url", set("approve_url", "/a")},
		{"ftp approve_url", set("approve_url", "ftp://x/a")},
		{"plain http approve_url off loopback", set("approve_url", "http://provider.example/a")},
		{"approve_url with a space", set("approve_url", loop+"/a b")},
		{"approve_url with ESC", set("approve_url", loop+"/a\x1b[2K")},
		{"approve_url with DEL", set("approve_url", loop+"/a\x7f")},
		{"approve_url not ASCII", set("approve_url", loop+"/ä")},
		{"approve_url with a bidi override", set("approve_url", loop+"/a‮gnp.exe")},
		{"approve_url with user information", set("approve_url", "http://provider.example@127.0.0.1:1/a")},
		{"approve_url over 2048 bytes", set("approve_url", long+"a")},
		{"request_id with a space", set("request_id", "r 1")},
		{"request_id with a slash", set("request_id", "r1/../x")},
		{"request_id of 129", set("request_id", strings.Repeat("r", 129))},
		{"user_code with a space", set("user_code", "AAAA BBBB")},
		{"user_code with ESC", set("user_code", "AAAA\x1b[2K")},
		{"user_code with CR", set("user_code", "AAAA\rBBBB")},
		{"user_code of 33", set("user_code", strings.Repeat("A", 33))},
		{"user_code not ASCII", set("user_code", "ÄÄÄÄ")},
		{"user_code of 60000", set("user_code", strings.Repeat("A", 60000))},
		{"poll_secret with a newline", set("poll_secret", "s3cr3t\ns3cr3t")},
		{"secret in request_id", set("request_id", "r1-s3cr3t-s3cr3t")},
		{"secret in user_code", set("user_code", "X-s3cr3t-s3cr3t")},
		{"secret in approve_url", set("approve_url", loop+"/a?s=s3cr3t-s3cr3t")},
	} {
		e := newEnv(t)
		e.p.FileWith(keyclienttest.Status(201, created201(loop+"/poll", loop+"/a", c.edit)))
		notified := false
		e.opts.Notify = func(request.Notice) { notified = true }
		_, err := request.Ensure(context.Background(), e.opts)
		if err == nil || strings.Contains(err.Error(), "s3cr3t") {
			t.Errorf("%s: err = %v", c.name, err)
		}
		if notified || e.pendingExists() {
			t.Errorf("%s: notified=%v pending=%v", c.name, notified, e.pendingExists())
		}
	}
	// The controls: the unedited reply, and an approve_url of exactly 2048
	// bytes, pass the checks (the poll then ends by cancellation).
	for name, edit := range map[string]func(map[string]any){
		"unedited":                  func(map[string]any) {},
		"approve_url of 2048 bytes": set("approve_url", long),
	} {
		e := newEnv(t)
		e.p.FileWith(keyclienttest.Status(201, created201(loop+"/poll", loop+"/a", edit)))
		ctx, cancel := context.WithCancel(context.Background())
		var got request.Notice
		e.opts.Notify = func(n request.Notice) { got = n; cancel() }
		_, err := request.Ensure(ctx, e.opts)
		if !errors.Is(err, context.Canceled) || got.UserCode != "AAAA-BBBB" {
			t.Errorf("%s: err = %v, notice = %+v", name, err, got)
		}
	}
}

func TestResumedPendingRevalidated(t *testing.T) {
	const secret = "old-secret-old-secret"
	set := func(k string, v any) func(map[string]any) { return func(m map[string]any) { m[k] = v } }
	for _, c := range []struct {
		name string
		edit func(map[string]any)
	}{
		{"request_id with a space", set("request_id", "old id")},
		{"user_code with ESC", set("user_code", "AAAA\x1b[2K")},
		{"approve_url with user information", set("approve_url", "http://u@127.0.0.1:1/a")},
		{"approve_url with a space", set("approve_url", "http://127.0.0.1:1/a b")},
		{"plain http approve_url off loopback", set("approve_url", "http://provider.example/a")},
		{"plain http poll_url off loopback", set("poll_url", "http://provider.example/poll")},
		{"secret in approve_url", set("approve_url", "http://127.0.0.1:1/a?"+secret)},
		{"secret in user_code", set("user_code", "X-"+secret)},
		{"secret in request_id", set("request_id", "id-"+secret)},
		{"empty poll_secret", set("poll_secret", "")},
		{"role with a space", set("role", "bad role")},
		{"tool with a space", set("tool", "bad tool")},
		{"interval 0", set("interval", 0)},
	} {
		e := newEnv(t)
		writePending(t, e, 0o600, pendingJSON(e, time.Now().Add(time.Hour), e.p.URL()+"/v1/requests/old/poll", c.edit))
		notified := false
		e.opts.Notify = func(request.Notice) { notified = true }
		_, err := request.Ensure(context.Background(), e.opts)
		if err == nil || !strings.Contains(err.Error(), "cannot be used; remove it") || strings.Contains(err.Error(), secret) {
			t.Errorf("%s: err = %v", c.name, err)
		}
		if notified || len(e.p.Filed()) != 0 || len(e.p.Polls()) != 0 {
			t.Errorf("%s: notified=%v filed=%d polls=%d", c.name, notified, len(e.p.Filed()), len(e.p.Polls()))
		}
	}
}

func TestPendingNotARegularFile(t *testing.T) {
	e := newEnv(t)
	if err := os.MkdirAll(request.PendingPath(e.token), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := request.Ensure(context.Background(), e.opts)
	// Refused by the regular-file rule itself, not by a later read error.
	if err == nil || !strings.Contains(err.Error(), request.PendingPath(e.token)) || !strings.Contains(err.Error(), "must be a regular file") {
		t.Errorf("a directory: err = %v", err)
	}
	if len(e.p.Filed()) != 0 {
		t.Error("filed")
	}
}

func TestProviderMessageSanitised(t *testing.T) {
	e := newEnv(t)
	msg := "a\u0007b\x1b[31mred\nline‮" + strings.Repeat("x", 300)
	b, _ := json.Marshal(map[string]string{"error": "bad\x1b[2K_request", "message": msg})
	e.p.FileWith(keyclienttest.Status(400, string(b)))
	_, err := request.Ensure(context.Background(), e.opts)
	var pe *request.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v", err)
	}
	if n := len([]rune(pe.Message)); n > 200 {
		t.Errorf("message has %d runes", n)
	}
	for _, s := range []string{pe.Message, pe.Code, err.Error()} {
		for _, r := range s {
			if r < ' ' || r == 0x7f || r == '‮' {
				t.Errorf("control character %q in %q", r, s)
			}
		}
	}
}

func TestUnauthorizedMessageSanitised(t *testing.T) {
	e := newEnv(t)
	msg := "no\u0007such\x1b[31mrequest\nhere‮" + strings.Repeat("x", 300)
	b, _ := json.Marshal(map[string]string{"error": "unauthorized", "message": msg})
	e.p.Script(keyclienttest.Status(401, string(b)))
	_, err := request.Ensure(context.Background(), e.opts)
	if !errors.Is(err, request.ErrUnauthorized) {
		t.Fatalf("err = %v", err)
	}
	shown := strings.TrimPrefix(err.Error(), request.ErrUnauthorized.Error()+": ")
	if !strings.HasPrefix(shown, "no") || !strings.Contains(shown, "such") {
		t.Errorf("the message is not shown: %q", err)
	}
	if n := len([]rune(shown)); n > 200 {
		t.Errorf("message has %d runes", n)
	}
	for _, r := range err.Error() {
		if r < ' ' || r == 0x7f || r == '‮' {
			t.Errorf("control character %q in %q", r, err)
		}
	}
}

// leaks reports whether s holds secret, or any 12 bytes of it in a row.
func leaks(s, secret string) bool {
	if secret == "" {
		return false
	}
	if len(secret) <= 12 {
		return strings.Contains(s, secret)
	}
	for i := 0; i+12 <= len(secret); i++ {
		if strings.Contains(s, secret[i:i+12]) {
			return true
		}
	}
	return false
}

// TestSecretsNeverLeak has the provider echo the session's real poll secret
// (and the key, where the tool holds it) in every place it controls, and checks
// every error, notice and pending text. Filing errors are answered before any
// poll secret or key exists, so they cannot echo one the tool holds; their
// sanitising is TestProviderMessageSanitised.
func TestSecretsNeverLeak(t *testing.T) {
	check := func(t *testing.T, secrets []string, vals ...any) {
		t.Helper()
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
			for _, sec := range secrets {
				if leaks(s, sec) {
					t.Errorf("secret %q leaked in %q", sec, s)
				}
			}
		}
	}
	split := func(s, sep string) string { return strings.Join(strings.Split(s, ""), sep) }

	// 401 replies built from the secret the provider has just handed out.
	for name, mk := range map[string]func(secret string) string{
		"plain":                 func(s string) string { return "bad secret " + s },
		"split by NUL":          func(s string) string { return "bad secret " + split(s, "\x00") }, // reviewer R2
		"split by ESC":          func(s string) string { return "bad secret " + split(s, "\x1b") },
		"split by zero width":   func(s string) string { return "bad secret " + split(s, "​") },
		"split by bidi":         func(s string) string { return split(s, "‮") },
		"across the 200 cut":    func(s string) string { return strings.Repeat("x", 180) + s },
		"twice, both split":     func(s string) string { return split(s, "\x00") + " and " + split(s, "​") },
		"in the code and twice": func(s string) string { return s + s },
	} {
		t.Run("401 "+name, func(t *testing.T) {
			for _, wait := range []bool{true, false} {
				e := newEnv(t)
				var notice request.Notice
				e.opts.Notify = func(n request.Notice) {
					notice = n
					sec := e.p.PollSecret()
					b, _ := json.Marshal(map[string]string{"error": sec, "message": mk(sec)})
					e.p.Script(keyclienttest.Status(401, string(b)).WithHeader("X-Echo", sec))
				}
				var err error
				if wait {
					_, err = request.Ensure(context.Background(), e.opts)
				} else {
					ep := e.background(context.Background())
					waitDone(t, ep)
					check(t, []string{e.p.PollSecret()}, ep, ep.Notice)
					err = ep.Err()
				}
				if !errors.Is(err, request.ErrUnauthorized) {
					t.Fatalf("wait=%v: err = %v", wait, err)
				}
				check(t, []string{e.p.PollSecret(), e.p.Key()}, err, notice)
			}
		})
	}

	t.Run("other statuses echo it", func(t *testing.T) {
		for _, status := range []int{302, 418, 400} {
			e := newEnv(t)
			e.opts.Notify = func(request.Notice) {
				sec := e.p.PollSecret()
				b, _ := json.Marshal(map[string]string{"error": sec, "message": sec, "status": sec})
				e.p.Script(keyclienttest.Status(status, string(b)).WithHeader("Location", "http://127.0.0.1:1/"+sec).WithHeader("X-Echo", sec))
			}
			_, err := request.Ensure(context.Background(), e.opts)
			if err == nil {
				t.Fatalf("%d: no error", status)
			}
			check(t, []string{e.p.PollSecret()}, err)
		}
	})

	t.Run("the 201 echoes it", func(t *testing.T) {
		const sec = "echoed-poll-secret-0123456789"
		for name, edit := range map[string]func(map[string]any){
			"approve_url": func(m map[string]any) { m["approve_url"] = "http://127.0.0.1:1/a?s=" + sec },
			"user_code":   func(m map[string]any) { m["user_code"] = sec },
			"request_id":  func(m map[string]any) { m["request_id"] = sec + "-1" },
		} {
			for _, wait := range []bool{true, false} {
				e := newEnv(t)
				e.opts.Wait = wait
				e.p.FileWith(keyclienttest.Status(201, created201("http://127.0.0.1:1/poll", "http://127.0.0.1:1/a",
					func(m map[string]any) { m["poll_secret"] = sec }, edit)).WithHeader("X-Echo", sec))
				var notice *request.Notice
				e.opts.Notify = func(n request.Notice) { notice = &n }
				_, err := request.Ensure(context.Background(), e.opts)
				var ep *request.ErrPending
				if err == nil || errors.As(err, &ep) || notice != nil {
					t.Fatalf("%s wait=%v: err = %v, notice = %v: the 201 must be refused", name, wait, err, notice)
				}
				check(t, []string{sec}, err)
				if e.pendingExists() {
					t.Errorf("%s: pending written", name)
				}
			}
		}
	})

	t.Run("the approved reply echoes the key", func(t *testing.T) {
		e := newEnv(t)
		key := e.p.Key()
		e.p.Script(keyclienttest.ApprovedWith(key, "agent", key))
		res, err := request.Ensure(context.Background(), e.opts)
		if err != nil || res.Token != key {
			t.Fatalf("res=%+v err=%v", res, err)
		}
		if res.Name != "" || res.Role != "agent" {
			t.Errorf("a name that holds the key must be left empty: %+v", res)
		}
	})

	t.Run("final statuses", func(t *testing.T) {
		for _, sc := range []string{"denied", "expired", "unauthorized", "lost-reply"} {
			e := newEnv(t)
			e.p.Script(keyclienttest.Scenario(sc)...)
			_, err := request.Ensure(context.Background(), e.opts)
			if err == nil {
				t.Errorf("%s: nil error", sc)
			}
			check(t, []string{e.p.PollSecret(), e.p.Key()}, err)
		}
	})
}

func TestApprovedNameAndRoleChecked(t *testing.T) {
	for _, c := range []struct{ name, role, wantName, wantRole string }{
		{"alice@laptop", "agent", "alice@laptop", "agent"},
		{"alice laptop", "agent", "", "agent"},
		{"alice@laptop", "ag\x1bent", "alice@laptop", ""},
		{strings.Repeat("n", 65), strings.Repeat("r", 33), "", ""},
		{"", "", "", ""},
	} {
		e := newEnv(t)
		e.p.Script(keyclienttest.ApprovedWith(c.name, c.role, e.p.Key()))
		res, err := request.Ensure(context.Background(), e.opts)
		if err != nil || res != (request.Result{Token: e.p.Key(), Name: c.wantName, Role: c.wantRole}) {
			t.Errorf("name %q role %q: res=%+v err=%v", c.name, c.role, res, err)
		}
	}
}

// pad is a JSON member that takes a reply over MaxReply.
var pad = `"pad":"` + strings.Repeat("x", request.MaxReply) + `"`

func TestReplyLimit(t *testing.T) {
	t.Run("201", func(t *testing.T) {
		e := newEnv(t)
		body := created201("http://127.0.0.1:1/poll", "http://127.0.0.1:1/a")
		e.p.FileWith(keyclienttest.Status(201, body[:len(body)-1]+","+pad+"}"))
		notified := false
		e.opts.Notify = func(request.Notice) { notified = true }
		_, err := request.Ensure(context.Background(), e.opts)
		if err == nil || !strings.Contains(err.Error(), "larger than") {
			t.Errorf("err = %v", err)
		}
		if notified || e.pendingExists() {
			t.Error("notified or pending written")
		}
	})
	t.Run("discovery", func(t *testing.T) {
		e := newEnv(t)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, `{"request_url":%q,%s}`, e.p.RequestURL(), pad)
		}))
		defer srv.Close()
		e.opts.ServiceURL = srv.URL
		_, err := request.Ensure(context.Background(), e.opts)
		if err == nil || !strings.Contains(err.Error(), "larger than") || len(e.p.Filed()) != 0 {
			t.Errorf("err = %v, filed = %d", err, len(e.p.Filed()))
		}
	})
	t.Run("202 is read as unreadable: polling goes on", func(t *testing.T) {
		e := newEnv(t)
		e.p.Script(keyclienttest.Status(202, `{"status":"pending","interval":1,`+pad+`}`), keyclienttest.Approved())
		res, err := request.Ensure(context.Background(), e.opts)
		if err != nil || res.Token != e.p.Key() || len(e.p.Polls()) != 2 {
			t.Errorf("res=%+v err=%v polls=%d", res, err, len(e.p.Polls()))
		}
	})
	t.Run("200 is read as unreadable: nothing stored, request kept", func(t *testing.T) {
		e := newEnv(t)
		e.p.Script(keyclienttest.Status(200, fmt.Sprintf(`{"status":"approved","name":"alice@laptop","role":"agent","key":%q,%s}`, e.p.Key(), pad)))
		_, err := request.Ensure(context.Background(), e.opts)
		if err == nil || !strings.Contains(err.Error(), "larger than") {
			t.Errorf("err = %v", err)
		}
		if _, serr := os.Stat(e.token); serr == nil {
			t.Error("token written")
		}
		if !e.pendingExists() {
			t.Error("pending removed")
		}
	})
}

func TestRequestTimeout(t *testing.T) {
	t.Cleanup(request.SetRequestTimeout(150 * time.Millisecond))
	t.Run("hanging discovery", func(t *testing.T) {
		e := newEnv(t)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
		defer srv.Close()
		e.opts.ServiceURL = srv.URL
		start := time.Now()
		_, err := request.Ensure(context.Background(), e.opts)
		if err == nil || errors.Is(err, request.ErrNoProvider) || time.Since(start) > 3*time.Second {
			t.Errorf("err = %v after %v", err, time.Since(start))
		}
	})
	t.Run("hanging filing", func(t *testing.T) {
		e := newEnv(t)
		e.p.FileWith(keyclienttest.Created().After(time.Minute))
		start := time.Now()
		_, err := request.Ensure(context.Background(), e.opts)
		if err == nil || time.Since(start) > 3*time.Second || e.pendingExists() {
			t.Errorf("err = %v after %v", err, time.Since(start))
		}
	})
	t.Run("hanging poll is transient", func(t *testing.T) {
		e := newEnv(t)
		e.p.Script(keyclienttest.Pending().After(time.Minute), keyclienttest.Approved())
		start := time.Now()
		res, err := request.Ensure(context.Background(), e.opts)
		if err != nil || res.Token != e.p.Key() || time.Since(start) > 3*time.Second {
			t.Errorf("res=%+v err=%v after %v", res, err, time.Since(start))
		}
		if n := len(e.p.Polls()); n != 2 {
			t.Errorf("polls = %d", n)
		}
	})
}
