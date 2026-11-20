package request_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/resurgentech/keyclient/keyclienttest"
	"github.com/resurgentech/keyclient/request"
)

// noSecret fails if s holds the secret.
func noSecret(t *testing.T, where, s, secret string) {
	t.Helper()
	if secret != "" && strings.Contains(s, secret) {
		t.Errorf("%s holds the poll secret: %q", where, s)
	}
}

// T-30: no pending file: ok false, a nil error, and nothing is created.
func TestT30_PendingAbsent(t *testing.T) {
	e := newEnv(t)
	n, ok, err := request.Pending(e.opts)
	if ok || err != nil || n != (request.Notice{}) {
		t.Fatalf("Pending = %+v, %v, %v; want nothing", n, ok, err)
	}
	if _, err := os.Lstat(filepath.Dir(e.token)); !os.IsNotExist(err) {
		t.Fatalf("Pending created the token's directory (%v)", err)
	}
}

// T-30, T-I2: while another holder of the lock polls the request (a Wait false
// call's background poll), Pending reads the pending file at once, without the
// lock, and reports the same notice, marked resumed, without the secret.
func TestT30_PendingWhileAnotherHoldsTheLock(t *testing.T) {
	e := newEnv(t)
	e.p.SetInterval(100) // the background poll waits; it holds the lock all along
	o := e.opts
	o.Wait = false
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := request.Ensure(ctx, o)
	var ep *request.ErrPending
	if !errors.As(err, &ep) {
		t.Fatalf("Ensure = %v, want *ErrPending", err)
	}
	start := time.Now()
	n, ok, err := request.Pending(e.opts)
	if d := time.Since(start); d > time.Second {
		t.Fatalf("Pending took %v: it waited for the lock", d)
	}
	if !ok || err != nil {
		t.Fatalf("Pending = %v, %v; want the open request", ok, err)
	}
	want := ep.Notice
	want.Resumed = true
	if n.ApproveURL != want.ApproveURL || n.UserCode != want.UserCode || n.Service != want.Service ||
		n.Role != want.Role || n.Tool != want.Tool || !n.Resumed || !n.ExpiresAt.Equal(want.ExpiresAt) {
		t.Fatalf("Pending notice = %+v, want %+v", n, want)
	}
	noSecret(t, "the notice", fmt.Sprintf("%+v %s", n, n.String()), e.p.PollSecret())
	cancel()
	<-ep.Done()
}

// T-30: an expired pending file, and one filed for another service URL or
// override, are told apart with errors.Is; neither is removed.
func TestT30_PendingExpiredAndForeign(t *testing.T) {
	e := newEnv(t)
	writePending(t, e, 0o600, pendingJSON(e, time.Now().Add(-time.Minute), e.p.URL()+"/v1/requests/old/poll"))
	n, ok, err := request.Pending(e.opts)
	if ok || !errors.Is(err, request.ErrPendingExpired) || n != (request.Notice{}) {
		t.Fatalf("expired: %+v, %v, %v; want ErrPendingExpired", n, ok, err)
	}
	if _, err := os.Stat(request.PendingPath(e.token)); err != nil {
		t.Fatalf("Pending removed the expired file: %v", err)
	}

	writePending(t, e, 0o600, pendingJSON(e, time.Now().Add(time.Hour), e.p.URL()+"/v1/requests/old/poll"))
	other := e.opts
	other.ServiceURL = keyclienttest.NewProvider(t).URL()
	if _, ok, err := request.Pending(other); ok || !errors.Is(err, request.ErrPendingForeign) {
		t.Fatalf("another service URL: %v, %v; want ErrPendingForeign", ok, err)
	}
	other = e.opts
	other.Override = e.p.RequestURL()
	if _, ok, err := request.Pending(other); ok || !errors.Is(err, request.ErrPendingForeign) {
		t.Fatalf("another override: %v, %v; want ErrPendingForeign", ok, err)
	}
	if _, ok, err := request.Pending(e.opts); !ok || err != nil {
		t.Fatalf("the same options: %v, %v; want the open request", ok, err)
	}
}

// T-30: a pending file the resume path would refuse gives ErrPendingUnusable,
// with the resume path's wording, naming the file; the secret appears nowhere.
func TestT30_PendingUnusable(t *testing.T) {
	const secret = "old-secret-old-secret"
	poll := func(e *env) string { return e.p.URL() + "/v1/requests/old/poll" }
	cases := map[string]func(t *testing.T, e *env){
		"bad JSON": func(t *testing.T, e *env) { writePending(t, e, 0o600, "{not json") },
		"mode 0644": func(t *testing.T, e *env) {
			writePending(t, e, 0o644, pendingJSON(e, time.Now().Add(time.Hour), poll(e)))
		},
		"a directory": func(t *testing.T, e *env) {
			if err := os.MkdirAll(request.PendingPath(e.token), 0o700); err != nil {
				t.Fatal(err)
			}
		},
		"not owned": func(t *testing.T, e *env) {
			writePending(t, e, 0o600, pendingJSON(e, time.Now().Add(time.Hour), poll(e)))
			t.Cleanup(request.SetGeteuid(func() int { return os.Geteuid() + 1 }))
		},
		"secret in approve_url": func(t *testing.T, e *env) {
			writePending(t, e, 0o600, pendingJSON(e, time.Now().Add(time.Hour), poll(e), func(m map[string]any) {
				m["approve_url"] = "http://127.0.0.1:1/a?s=" + secret
			}))
		},
		"user_code breaks its rule": func(t *testing.T, e *env) {
			writePending(t, e, 0o600, pendingJSON(e, time.Now().Add(time.Hour), poll(e), func(m map[string]any) {
				m["user_code"] = "AAAA\x1b[2K"
			}))
		},
		"plain http poll URL off loopback": func(t *testing.T, e *env) {
			writePending(t, e, 0o600, pendingJSON(e, time.Now().Add(time.Hour), "http://provider.example/v1/requests/old/poll"))
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			setup(t, e)
			n, ok, err := request.Pending(e.opts)
			if ok || n != (request.Notice{}) || !errors.Is(err, request.ErrPendingUnusable) {
				t.Fatalf("%+v, %v, %v; want ErrPendingUnusable", n, ok, err)
			}
			if !strings.Contains(err.Error(), request.PendingPath(e.token)) || !strings.Contains(err.Error(), "remove it") {
				t.Errorf("err = %v; want the file named and \"remove it\"", err)
			}
			noSecret(t, "the error", err.Error(), secret)
			if errors.Is(err, request.ErrPendingExpired) || errors.Is(err, request.ErrPendingForeign) {
				t.Errorf("err = %v also matches expired or foreign", err)
			}
		})
	}
}

// T-30 validates the options it uses exactly as Ensure does, before any I/O.
func TestT30_PendingValidatesOptions(t *testing.T) {
	e := newEnv(t)
	o := e.opts
	o.TokenPath = ""
	if _, _, err := request.Pending(o); err == nil || !strings.Contains(err.Error(), "TokenPath") {
		t.Errorf("no TokenPath: %v", err)
	}
	o = e.opts
	o.ServiceURL = ""
	if _, _, err := request.Pending(o); !errors.Is(err, request.ErrNoProvider) {
		t.Errorf("no URL: %v, want ErrNoProvider", err)
	}
	o = e.opts
	o.ServiceURL = "http://provider.example"
	if _, _, err := request.Pending(o); err == nil || !strings.Contains(err.Error(), "ServiceURL") {
		t.Errorf("plain http off loopback: %v", err)
	}
	o.AllowInsecureHTTP = true
	if _, ok, err := request.Pending(o); ok || err != nil {
		t.Errorf("plain http allowed, no pending file: %v, %v", ok, err)
	}
}
