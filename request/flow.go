package request

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/resurgentech/keyclient/internal/atomicfile"
)

// Ensure returns the tool's token. If TokenPath holds one it is returned and
// nothing else happens: no lock, no network, no file touched. Otherwise, when
// a request URL is known, it files a key request (or resumes the one kept in
// the pending file), calls Notify once and polls; see the package
// documentation, docs/CONTRACT.md and docs/STATES.md.
//
// With Wait false the returned error is an *[ErrPending] right after filing or
// resuming (or the result of the one poll that was already due); the poll then
// runs in a goroutine, holding the lock, until it ends or ctx ends, so ctx
// must outlive the call. With Wait true, waiting for the lock that another
// process holds is bounded by ctx; with Wait false it never delays the return.
func Ensure(ctx context.Context, o Options) (Result, error) {
	host, err := o.validate()
	if err != nil {
		return Result{}, err
	}
	tokenPath := filepath.Clean(o.TokenPath)
	if tok, err := readToken(tokenPath); err != nil || tok != "" {
		return Result{Token: tok}, err
	}
	pol := policy{insecure: o.AllowInsecureHTTP}
	switch {
	case o.Override != "":
		_, pol, err = pol.check("Override", o.Override)
	case o.ServiceURL != "":
		_, pol, err = pol.check("ServiceURL", o.ServiceURL)
	default:
		return Result{}, ErrNoProvider
	}
	if err != nil {
		return Result{}, err
	}
	for {
		f, owner := startOrJoin(tokenPath)
		if owner {
			fl := &flow{
				o: o, f: f, tokenPath: tokenPath, pendPath: PendingPath(tokenPath), host: host,
				serviceURL: strings.TrimRight(o.ServiceURL, "/"), pol: pol, client: o.client(), unit: second,
			}
			return fl.own(ctx)
		}
		res, err, again := f.join(ctx, o.Wait)
		if !again {
			return res, err
		}
	}
}

// flow is the run of one flight by its owner.
type flow struct {
	o          Options
	f          *flight
	tokenPath  string
	pendPath   string
	host       string
	serviceURL string // o.ServiceURL with any trailing / trimmed
	pol        policy // the policy after ServiceURL or Override
	client     *http.Client
	unit       time.Duration

	disc  *discovered // the request URL that discovery gave before the lock, if it ran
	shown string      // the id of the request whose notice this call has already given
}

type discovered struct {
	url string
	pol policy // the policy after it
}

// own runs the flow in the caller's goroutine and ends the flight, unless the
// flow was handed to a background goroutine. A panic (in Notify) ends the
// flight with an error for every joined caller and goes on to the caller of
// Ensure (T-14); the lock is released on the way.
func (fl *flow) own(ctx context.Context) (res Result, err error) {
	returned, handedOff := false, false
	defer func() {
		switch {
		case !returned:
			fl.f.finish(Result{}, errNotifyPanicked)
		case !handedOff:
			fl.f.finish(res, err)
		}
	}()
	res, err, handedOff = fl.begin(ctx)
	returned = true
	return res, err
}

// begin is the order of a flow up to the lock:
//
//  1. Where the platform has no lock, an error (T-5).
//  2. When the request URL must come from discovery (no Override) and there is
//     no pending file (a plain Lstat, without the lock), discovery runs first,
//     so that a service without a provider leaves nothing on disk (T-7).
//  3. The lock file is opened (made, with its directory, if missing) and the
//     lock tried without waiting (T-4). If another process holds it: T-21 or
//     T-22 (behind). Otherwise: locked.
func (fl *flow) begin(ctx context.Context) (Result, error, bool) {
	if !lockSupported {
		return Result{}, errUnsupported, false
	}
	if fl.o.Override == "" {
		if _, err := os.Lstat(fl.pendPath); errors.Is(err, fs.ErrNotExist) {
			u, rpol, err := fl.o.discover(ctx, fl.client, fl.pol)
			if err != nil {
				return Result{}, err, false
			}
			fl.disc = &discovered{url: u, pol: rpol}
		}
	}
	lk, err := openLock(fl.tokenPath + lockExt)
	if err != nil {
		return Result{}, err, false
	}
	got, err := lk.try()
	if err != nil {
		lk.release()
		return Result{}, err, false
	}
	if !got {
		return fl.behind(ctx, lk)
	}
	return fl.locked(ctx, lk, false)
}

// behind is a call that finds the lock held by another process (T-21, T-22).
// The pending file is read without the lock, and only read.
func (fl *flow) behind(ctx context.Context, lk *fileLock) (Result, error, bool) {
	n, id, current := fl.peek()
	if !fl.o.Wait {
		// T-22, T-I9: return at once.
		if !current {
			lk.release()
			return Result{}, ErrBusy, false
		}
		ep := fl.f.pending(n)
		fl.shown = id
		fl.notify(lk, n)
		go func() {
			if err := lk.wait(ctx); err != nil {
				lk.release()
				fl.f.finish(Result{}, err)
				return
			}
			res, err, _ := fl.locked(ctx, lk, true)
			fl.f.finish(res, err)
		}()
		return Result{}, ep, true
	}
	// T-21: show the notice of P, if any, then wait for the lock.
	if current {
		fl.f.pending(n)
		fl.shown = id
		fl.notify(lk, n)
	} else {
		fl.f.waitingForOther()
	}
	if err := lk.wait(ctx); err != nil {
		lk.release()
		return Result{}, err, false
	}
	return fl.locked(ctx, lk, false)
}

// locked runs the flow once the lock is held. background is the goroutine of
// T-22: it only collects the token or resumes the request it reported.
func (fl *flow) locked(ctx context.Context, lk *fileLock, background bool) (res Result, err error, handedOff bool) {
	defer func() {
		if !handedOff {
			lk.release()
		}
	}()
	// T-25: temporary files of an interrupted write of this token.
	atomicfile.RemoveTemps(fl.tokenPath)
	atomicfile.RemoveTemps(fl.pendPath)
	if tok, err := readToken(fl.tokenPath); err != nil || tok != "" {
		if tok != "" {
			os.Remove(fl.pendPath) // T-6, T-27: a leftover of a crash
		}
		return Result{Token: tok}, err, false
	}
	s, n, err := fl.start(ctx, background)
	if err != nil {
		return Result{}, err, false
	}
	ep := fl.f.pendingErr()
	if s.requestID != fl.shown {
		ep = fl.f.pending(n)
		fl.notify(lk, n)
	}
	if fl.o.Wait || background {
		res, err := s.run(ctx)
		return res, err, false
	}
	// T-16: a poll of a resumed request that is already due is made before
	// returning. (A request filed by this call is not polled here, however long
	// Notify took: Wait false then always returns *ErrPending after filing.)
	if n.Resumed && s.due() {
		if res, err, done := s.step(ctx); done {
			return res, err, false
		}
	}
	go func() {
		res, err := s.run(ctx)
		lk.release()
		fl.f.finish(res, err)
	}()
	return Result{}, ep, true
}

// notify calls Notify. If it panics, the lock is released before the panic
// goes on.
func (fl *flow) notify(lk *fileLock, n Notice) {
	if fl.o.Notify == nil {
		return
	}
	ok := false
	defer func() {
		if !ok {
			lk.release()
		}
	}()
	fl.o.Notify(n)
	ok = true
}

// peek reads the pending file without the lock and reports whether it is
// current, with its notice (marked resumed) and request id. It changes nothing.
func (fl *flow) peek() (Notice, string, bool) {
	pf, state, err := parsePending(fl.pendPath, fl.serviceURL, fl.o.Override, fl.pol, fl.unit)
	if err != nil || state != pendingCurrent {
		return Notice{}, "", false
	}
	return noticeOf(pf, true), pf.RequestID, true
}

// start resumes the pending request (T-11), or files a new one and stores it
// (T-10). The caller holds the lock. With background, only the request this
// call has reported is resumed; anything else is ErrBusy.
func (fl *flow) start(ctx context.Context, background bool) (*session, Notice, error) {
	pf, resumed, err := loadPending(fl.pendPath, fl.serviceURL, fl.o.Override, fl.pol, fl.unit)
	if err != nil {
		return nil, Notice{}, err
	}
	if background && (!resumed || pf.RequestID != fl.shown) {
		return nil, Notice{}, ErrBusy
	}
	s := &session{client: fl.client, tokenPath: fl.tokenPath, pendPath: fl.pendPath, unit: fl.unit}
	if resumed {
		// L-2: the interval and next poll P recorded. A next in the past (or
		// absent: an older file) means now; one further away than an interval
		// is cut to an interval from now.
		s.interval = pf.Interval
		t := now()
		next := pf.NextPollAt
		if next.Before(t) {
			next = t
		}
		if limit := t.Add(s.wait()); next.After(limit) {
			next = limit
		}
		s.nextAt = next
	} else {
		reqURL, rpol := fl.o.Override, fl.pol
		if reqURL == "" {
			if fl.disc != nil {
				reqURL, rpol = fl.disc.url, fl.disc.pol
			} else if reqURL, rpol, err = fl.o.discover(ctx, fl.client, fl.pol); err != nil {
				return nil, Notice{}, err
			}
		}
		c, arrived, err := fileRequest(ctx, fl.client, reqURL, rpol, fl.o, fl.host)
		if err != nil {
			return nil, Notice{}, err
		}
		pf = c.pendingFile
		pf.ExpiresAt = arrived.Add(time.Duration(min(c.expiresIn, maxExpiresIn)) * fl.unit)
		pf.ServiceURL, pf.Override, pf.RequestURL = fl.serviceURL, fl.o.Override, reqURL
		s.interval = pf.Interval
		s.nextAt = arrived.Add(s.wait()) // L-1
		pf.NextPollAt = s.nextAt
		data, err := json.Marshal(pf)
		if err != nil {
			return nil, Notice{}, err
		}
		if err := atomicfile.Write(fl.pendPath, data); err != nil {
			return nil, Notice{}, fmt.Errorf("key request: cannot store the pending request: %w", err)
		}
	}
	s.pf = pf
	s.requestID, s.pollURL, s.secret, s.expiresAt = pf.RequestID, pf.PollURL, pf.PollSecret, pf.ExpiresAt
	return s, noticeOf(pf, resumed), nil
}

// noticeOf is the notice of the request in pf.
func noticeOf(pf pendingFile, resumed bool) Notice {
	return Notice{
		Service: serviceHost(pf.ServiceURL, pf.Override), ApproveURL: pf.ApproveURL, UserCode: pf.UserCode,
		Role: pf.Role, Tool: pf.Tool, ExpiresAt: pf.ExpiresAt, Resumed: resumed,
	}
}

// serviceHost is the host (and port) of the Override when it is set, else of
// the ServiceURL, made safe to show.
func serviceHost(serviceURL, override string) string {
	raw := override
	if raw == "" {
		raw = serviceURL
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return sanitize(u.Host, 255)
}
