package request

import (
	"context"
	"errors"
	"sync"
)

// flight is one flow (lock, file or resume, poll) for one TokenPath in this
// process. Every other caller of Ensure in this process for that path joins
// it instead of starting its own.
type flight struct {
	key   string
	ready chan struct{} // closed once ep is set, or when the flow ends without one
	done  chan struct{} // closed when the flow ends
	ep    *ErrPending   // set before ready is closed; nil if no request was filed or resumed
	res   Result        // set before done (and ready, if still open) is closed
	err   error

	// Touched only by the goroutine that runs the flow.
	readyClosed bool
	finished    bool
}

var (
	flightsMu sync.Mutex // guards flights only; never held across I/O
	flights   = map[string]*flight{}
)

// startOrJoin returns the flight for key, and whether the caller owns it (it
// was just made) or joins it.
func startOrJoin(key string) (*flight, bool) {
	flightsMu.Lock()
	defer flightsMu.Unlock()
	if f := flights[key]; f != nil {
		return f, false
	}
	f := &flight{key: key, ready: make(chan struct{}), done: make(chan struct{})}
	flights[key] = f
	return f, true
}

// pending records the notice of the filed or resumed request.
func (f *flight) pending(n Notice) *ErrPending {
	f.ep = &ErrPending{Notice: n, f: f}
	f.readyClosed = true
	close(f.ready)
	return f.ep
}

// finish ends the flight with its result. Later calls do nothing.
func (f *flight) finish(res Result, err error) {
	if f.finished {
		return
	}
	f.finished = true
	f.res, f.err = res, err
	flightsMu.Lock()
	if flights[f.key] == f {
		delete(flights, f.key)
	}
	flightsMu.Unlock()
	if !f.readyClosed {
		f.readyClosed = true
		close(f.ready)
	}
	close(f.done)
}

// join waits for the flight as a caller with this Wait: Wait true waits for
// its end, Wait false for its notice. again is true when the flight ended by
// its owner's context while ctx is still live: the caller starts over.
func (f *flight) join(ctx context.Context, wait bool) (res Result, err error, again bool) {
	if !wait {
		select {
		case <-f.ready:
			if f.ep != nil {
				return Result{}, f.ep, false
			}
		case <-ctx.Done():
			return Result{}, ctx.Err(), false
		}
	} else {
		select {
		case <-f.done:
		case <-ctx.Done():
			return Result{}, ctx.Err(), false
		}
	}
	if (errors.Is(f.err, context.Canceled) || errors.Is(f.err, context.DeadlineExceeded)) && ctx.Err() == nil {
		return Result{}, nil, true
	}
	return f.res, f.err, false
}
