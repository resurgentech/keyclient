package request

import (
	"context"
	"errors"
	"sync"
)

// errNoToken stands in for a flow that would otherwise end with an empty token
// and a nil error (T-I5); it cannot happen by design.
var errNoToken = errors.New("key request: the flow ended without a token")

// errNotifyPanicked is what the callers that joined a flow get when the
// owner's Notify panicked (T-14); the panic itself reaches the owner's caller.
var errNotifyPanicked = errors.New("key request: the flow ended because Notify panicked")

// flight is one flow (lock, file or resume, poll) for one TokenPath in this
// process. Every other caller of Ensure in this process for that path joins
// it instead of starting its own.
type flight struct {
	key   string
	ready chan struct{} // closed once a notice exists, or when the flow ends
	busy  chan struct{} // closed when the owner waits for another process's lock with no notice to show
	done  chan struct{} // closed when the flow ends
	res   Result        // set before done is closed
	err   error

	mu sync.Mutex
	ep *ErrPending // the latest notice; nil until one exists

	// Touched only by the goroutine that runs the flow at the time (the
	// owner's, then the background one it hands over to).
	readyClosed bool
	busyClosed  bool
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
	f := &flight{key: key, ready: make(chan struct{}), busy: make(chan struct{}), done: make(chan struct{})}
	flights[key] = f
	return f, true
}

// pending records the notice of the filed or resumed request and returns the
// *ErrPending that callers get from now on.
func (f *flight) pending(n Notice) *ErrPending {
	ep := &ErrPending{Notice: n, f: f}
	f.mu.Lock()
	f.ep = ep
	f.mu.Unlock()
	if !f.readyClosed {
		f.readyClosed = true
		close(f.ready)
	}
	return ep
}

// pendingErr is the latest *ErrPending, or nil.
func (f *flight) pendingErr() *ErrPending {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ep
}

// waitingForOther records that the owner waits for another process's lock
// without a notice to show: Wait false joiners get ErrBusy (T-I9).
func (f *flight) waitingForOther() {
	if !f.busyClosed {
		f.busyClosed = true
		close(f.busy)
	}
}

// finish ends the flight with its result. Later calls do nothing.
func (f *flight) finish(res Result, err error) {
	if f.finished {
		return
	}
	f.finished = true
	if err == nil && res.Token == "" {
		err = errNoToken
	}
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

// join waits for the flight as a caller with this Wait. Wait true waits for its
// end. Wait false waits for its notice, and gets the flow's result instead if
// the flow has already ended (T-17), or ErrBusy if the owner waits for another
// process with no notice to show. again is true when the flight ended for a
// reason that is not this caller's (T-19): by its owner's context while ctx is
// still live, or, for a Wait true caller, with ErrBusy (the background flow of
// a Wait false call found its request gone); the caller starts over as owner.
func (f *flight) join(ctx context.Context, wait bool) (res Result, err error, again bool) {
	if !wait {
		select {
		case <-f.ready:
		default:
			select {
			case <-f.ready:
			case <-f.busy:
				select {
				case <-f.ready: // a notice came after all
				default:
					return Result{}, ErrBusy, false
				}
			case <-ctx.Done():
				return Result{}, ctx.Err(), false
			}
		}
		select {
		case <-f.done:
		default:
			if ep := f.pendingErr(); ep != nil {
				return Result{}, ep, false
			}
			// ready was closed by finish, which closes done next.
			<-f.done
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
	if wait && errors.Is(f.err, ErrBusy) && ctx.Err() == nil {
		return Result{}, nil, true
	}
	return f.res, f.err, false
}
