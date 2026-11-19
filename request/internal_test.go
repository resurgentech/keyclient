package request

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

// T-17: a Wait false caller that joins a flow gets the flow's result once the
// flow has finished, and *ErrPending only while it still runs.
func TestT17_WaitFalseJoinerGetsTheResultOfAFinishedFlow(t *testing.T) {
	ctx := context.Background()

	f, owner := startOrJoin("t17-running")
	if !owner {
		t.Fatal("not the owner")
	}
	ep := f.pending(Notice{UserCode: "AAAA-BBBB"})
	if res, err, again := f.join(ctx, false); err != ep || res != (Result{}) || again {
		t.Errorf("running: res=%+v err=%v again=%v, want the *ErrPending", res, err, again)
	}

	f.finish(Result{Token: "the-token-0123456789"}, nil)
	if res, err, again := f.join(ctx, false); err != nil || res.Token != "the-token-0123456789" || again {
		t.Errorf("finished with a token: res=%+v err=%v again=%v", res, err, again)
	}

	f, _ = startOrJoin("t17-denied")
	f.pending(Notice{UserCode: "AAAA-BBBB"})
	f.finish(Result{}, ErrDenied)
	if res, err, _ := f.join(ctx, false); !errors.Is(err, ErrDenied) || res != (Result{}) {
		t.Errorf("finished denied: res=%+v err=%v", res, err)
	}
}

// T-I5: a flow can never end with an empty token and a nil error.
func TestTI5_FlowNeverEndsWithEmptyTokenAndNilError(t *testing.T) {
	f, _ := startOrJoin("ti5")
	ep := f.pending(Notice{})
	f.finish(Result{}, nil)
	if f.err == nil || ep.Ready() || ep.Err() == nil {
		t.Errorf("err=%v Ready=%v", f.err, ep.Ready())
	}
	if res, err, _ := f.join(context.Background(), true); err == nil {
		t.Errorf("joiner: res=%+v and a nil error", res)
	}
}

// L-7: a 429 whose interval is readable and at least the current one is used
// as given (at the provider's cap it is unchanged); one that is smaller, or a
// body that cannot be read, gives the current interval + 2.
func TestL7_SlowDownAtCapKeepsInterval(t *testing.T) {
	for _, c := range []struct {
		name string
		body string
		want int
	}{
		{"same interval (at the cap)", `{"status":"slow_down","interval":10}`, 10},
		{"larger", `{"status":"slow_down","interval":14}`, 14},
		{"smaller", `{"status":"slow_down","interval":4}`, 12},
		{"no interval", `{"status":"slow_down"}`, 12},
		{"unreadable", `garbage`, 12},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTooManyRequests)
				io.WriteString(w, c.body)
			}))
			defer srv.Close()
			s := &session{
				client: srv.Client(), pendPath: filepath.Join(t.TempDir(), "token.pending"), unit: time.Millisecond,
				requestID: "r", pollURL: srv.URL, secret: "s", interval: 10,
				nextAt: now(), expiresAt: now().Add(time.Hour),
			}
			if _, err, done := s.step(context.Background()); done {
				t.Fatalf("the loop ended: %v", err)
			}
			if s.interval != c.want {
				t.Errorf("next wait = %d, want %d", s.interval, c.want)
			}
		})
	}
}

// T-5: where the lock is not supported, the error wraps errors.ErrUnsupported.
func TestT5_UnsupportedIsRecognisable(t *testing.T) {
	if !errors.Is(errUnsupported, errors.ErrUnsupported) {
		t.Errorf("%v does not wrap errors.ErrUnsupported", errUnsupported)
	}
}
