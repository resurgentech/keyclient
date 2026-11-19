package request

import (
	"context"
	"errors"
	"testing"
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

// T-5: where the lock is not supported, the error wraps errors.ErrUnsupported.
func TestT5_UnsupportedIsRecognisable(t *testing.T) {
	if !errors.Is(errUnsupported, errors.ErrUnsupported) {
		t.Errorf("%v does not wrap errors.ErrUnsupported", errUnsupported)
	}
}
