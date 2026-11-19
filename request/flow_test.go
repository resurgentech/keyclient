package request_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/resurgentech/keyclient/keyclienttest"
	"github.com/resurgentech/keyclient/request"
)

// Two callers with Wait true on one TokenPath in one process: one filing, one
// notice, and the polls keep the interval (reviewer R3).
func TestTwoWaitersOneProcess(t *testing.T) {
	e := newEnv(t)
	e.p.SetInterval(2)
	e.p.Script(keyclienttest.Pending(), keyclienttest.Pending(), keyclienttest.Approved())
	var notified atomic.Int32
	e.opts.Notify = func(request.Notice) { notified.Add(1) }
	var wg sync.WaitGroup
	results := make([]request.Result, 2)
	errs := make([]error, 2)
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = request.Ensure(context.Background(), e.opts)
		}()
	}
	wg.Wait()
	for i := range 2 {
		if errs[i] != nil || results[i].Token != e.p.Key() {
			t.Errorf("caller %d: res=%+v err=%v", i, results[i], errs[i])
		}
	}
	if len(e.p.Filed()) != 1 || notified.Load() != 1 {
		t.Errorf("filed %d, notified %d; want 1 and 1", len(e.p.Filed()), notified.Load())
	}
	polls := e.p.Polls()
	if len(polls) != 3 {
		t.Errorf("polls = %d, want 3", len(polls))
	}
	for i := 1; i < len(polls); i++ {
		if gap := polls[i].Time.Sub(polls[i-1].Time); gap < 2*unit {
			t.Errorf("gap %d = %v, want >= %v", i, gap, 2*unit)
		}
	}
}

// A Wait false caller that arrives while a Wait true caller of this process
// polls gets an *ErrPending for that flow, and no second notice.
func TestWaitFalseJoinsWaitingCaller(t *testing.T) {
	e := newEnv(t)
	e.p.SetInterval(3)
	e.p.Script(keyclienttest.Pending(), keyclienttest.Approved())
	noticed := make(chan request.Notice, 2)
	e.opts.Notify = func(n request.Notice) { noticed <- n }
	type out struct {
		res request.Result
		err error
	}
	owner := make(chan out, 1)
	go func() {
		res, err := request.Ensure(context.Background(), e.opts)
		owner <- out{res, err}
	}()
	first := <-noticed
	o := e.opts
	o.Wait = false
	_, err := request.Ensure(context.Background(), o)
	var ep *request.ErrPending
	if !errors.As(err, &ep) {
		t.Fatalf("err = %v, want *ErrPending", err)
	}
	if ep.Notice != first {
		t.Errorf("notice = %+v, want %+v", ep.Notice, first)
	}
	waitDone(t, ep)
	if ep.Err() != nil || !ep.Ready() {
		t.Errorf("Err=%v Ready=%v", ep.Err(), ep.Ready())
	}
	if got := <-owner; got.err != nil || got.res.Token != e.p.Key() {
		t.Errorf("owner: %+v", got)
	}
	if len(noticed) != 0 || len(e.p.Filed()) != 1 {
		t.Errorf("a second notice (%d) or filing (%d)", len(noticed), len(e.p.Filed()))
	}
}

// drop removes the pending file only while it holds this request's id.
func TestDropLeavesAnotherRequestsPendingFile(t *testing.T) {
	e := newEnv(t)
	e.p.Script(keyclienttest.Denied())
	e.opts.Notify = func(request.Notice) {
		// Something outside this flow replaces the pending file meanwhile.
		writePending(t, e, 0o600, pendingJSON(e, time.Now().Add(time.Hour), e.p.URL()+"/v1/requests/other/poll",
			func(m map[string]any) { m["request_id"] = "other" }))
	}
	_, err := request.Ensure(context.Background(), e.opts)
	if !errors.Is(err, request.ErrDenied) {
		t.Fatalf("err = %v", err)
	}
	b, err := os.ReadFile(request.PendingPath(e.token))
	if err != nil {
		t.Fatalf("the other request's pending file was removed: %v", err)
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil || m["request_id"] != "other" {
		t.Errorf("pending file = %s", b)
	}
}

// A token that appears while the request is polled is kept, and the received
// key is not stored over it.
func TestApprovedKeepsATokenWrittenMeanwhile(t *testing.T) {
	e := newEnv(t)
	e.p.Script(keyclienttest.Approved())
	const other = "written-by-something-else-0123456789"
	e.opts.Notify = func(request.Notice) {
		if err := os.WriteFile(e.token, []byte(other+"\n"), 0o600); err != nil {
			t.Error(err)
		}
	}
	res, err := request.Ensure(context.Background(), e.opts)
	if err != nil || res != (request.Result{Token: other}) {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if b, _ := os.ReadFile(e.token); string(b) != other+"\n" {
		t.Errorf("token file = %q, want it untouched", b)
	}
	if e.pendingExists() {
		t.Error("pending remains")
	}
}

func TestLockFile(t *testing.T) {
	e := newEnv(t)
	e.p.Script(keyclienttest.Approved())
	if _, err := request.Ensure(context.Background(), e.opts); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(e.token + ".lock")
	if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o600 {
		t.Errorf("lock file: %v %v", fi, err)
	}
}

// The pending file of another service or request URL is dropped, not resumed
// (reviewer R10).
func TestPendingOfAnotherProviderDropped(t *testing.T) {
	for _, field := range []string{"Override", "ServiceURL"} {
		t.Run(field, func(t *testing.T) {
			e := newEnv(t)
			other := keyclienttest.NewProvider(t)
			if field == "Override" {
				e.opts.ServiceURL, e.opts.Override = "", e.p.RequestURL()
			}
			ctx, cancel := context.WithCancel(context.Background())
			e.opts.Notify = func(request.Notice) { cancel() }
			if _, err := request.Ensure(ctx, e.opts); !errors.Is(err, context.Canceled) || !e.pendingExists() {
				t.Fatalf("first run: err = %v, pending = %v", err, e.pendingExists())
			}

			if field == "Override" {
				e.opts.Override = other.RequestURL()
			} else {
				e.opts.ServiceURL = other.URL()
			}
			other.Script(keyclienttest.Approved())
			var n request.Notice
			e.opts.Notify = func(x request.Notice) { n = x }
			res, err := request.Ensure(context.Background(), e.opts)
			if err != nil || res.Token != other.Key() {
				t.Fatalf("second run: res=%+v err=%v", res, err)
			}
			if n.Resumed || len(other.Filed()) != 1 || len(e.p.Polls()) != 0 {
				t.Errorf("resumed=%v filed at the new provider=%d polls at the old=%d", n.Resumed, len(other.Filed()), len(e.p.Polls()))
			}
			if !strings.HasPrefix(n.ApproveURL, other.URL()) {
				t.Errorf("approve URL %q is not the new provider's", n.ApproveURL)
			}
		})
	}
	t.Run("same service, trailing slash", func(t *testing.T) {
		e := newEnv(t)
		ctx, cancel := context.WithCancel(context.Background())
		e.opts.Notify = func(request.Notice) { cancel() }
		request.Ensure(ctx, e.opts)
		e.opts.ServiceURL = e.p.URL() + "/"
		e.p.Script(keyclienttest.Approved())
		var n request.Notice
		e.opts.Notify = func(x request.Notice) { n = x }
		if _, err := request.Ensure(context.Background(), e.opts); err != nil {
			t.Fatal(err)
		}
		if !n.Resumed || len(e.p.Filed()) != 1 {
			t.Errorf("resumed=%v filed=%d", n.Resumed, len(e.p.Filed()))
		}
	})
}

func TestExpiresInClamped(t *testing.T) {
	e := newEnv(t)
	t.Cleanup(request.SetSecond(time.Second))
	e.p.SetExpiresIn(2000000000)
	ctx, cancel := context.WithCancel(context.Background())
	var n request.Notice
	e.opts.Notify = func(x request.Notice) { n = x; cancel() }
	request.Ensure(ctx, e.opts)
	if d := time.Until(n.ExpiresAt); d > 24*time.Hour || d < 23*time.Hour {
		t.Errorf("expires in %v, want 24h", d)
	}
	b, _ := os.ReadFile(request.PendingPath(e.token))
	var m struct {
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(b, &m); err != nil || !m.ExpiresAt.Equal(n.ExpiresAt) {
		t.Errorf("pending expires_at = %v, %v", m.ExpiresAt, err)
	}
}

// The tool polls once more at the expiry rather than giving up before it
// (reviewer R6): interval 5 and expires_in 12 poll at 5, 10 and 12.
func TestPollsOnceMoreAtExpiry(t *testing.T) {
	e := newEnv(t)
	e.p.SetInterval(5)
	e.p.SetExpiresIn(12)
	start := time.Now()
	_, err := request.Ensure(context.Background(), e.opts)
	if !errors.Is(err, request.ErrExpired) {
		t.Fatalf("err = %v", err)
	}
	if d := time.Since(start); d < 12*unit {
		t.Errorf("returned after %v, before the expiry (%v)", d, 12*unit)
	}
	filed := e.p.Filed()[0].Time
	polls := e.p.Polls()
	if len(polls) != 3 {
		t.Fatalf("polls = %d, want 3", len(polls))
	}
	for i, want := range []int{5, 10, 12} {
		at := polls[i].Time.Sub(filed)
		if at < time.Duration(want)*unit || at > time.Duration(want)*unit+2*unit {
			t.Errorf("poll %d at %v, want about %v", i, at, time.Duration(want)*unit)
		}
	}
	if e.pendingExists() {
		t.Error("pending remains")
	}

	// interval equal to expires_in: one poll, at the expiry.
	e = newEnv(t)
	t.Cleanup(request.SetSecond(time.Millisecond))
	e.p.SetInterval(600)
	e.p.SetExpiresIn(600)
	_, err = request.Ensure(context.Background(), e.opts)
	if !errors.Is(err, request.ErrExpired) || len(e.p.Polls()) != 1 {
		t.Errorf("err = %v, polls = %d, want ErrExpired after one poll", err, len(e.p.Polls()))
	}
}

func TestIsTerminal(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "plain"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if request.IsTerminal(f) {
		t.Error("a regular file is not a terminal")
	}
	if request.IsTerminal(nil) {
		t.Error("nil is not a terminal")
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if request.IsTerminal(r) || request.IsTerminal(w) {
		t.Error("a pipe is not a terminal")
	}
	dn, err := os.Open(os.DevNull)
	if err != nil {
		t.Skip("no " + os.DevNull)
	}
	defer dn.Close()
	if request.IsTerminal(dn) {
		t.Error(os.DevNull + " is a character device but not a terminal")
	}
}
