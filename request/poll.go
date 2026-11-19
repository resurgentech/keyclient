package request

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/resurgentech/keyclient/internal/atomicfile"
)

// session is one open request being polled.
type session struct {
	client    *http.Client
	tokenPath string
	pendPath  string
	unit      time.Duration

	pf        pendingFile // what the pending file holds for this request
	requestID string
	pollURL   string
	secret    string
	interval  int       // the current interval I, in units of s.unit
	nextAt    time.Time // next: when the next poll may be sent
	expiresAt time.Time
}

func (s *session) wait() time.Duration { return time.Duration(s.interval) * s.unit }

// due reports whether the next poll (or the final one at the expiry) may be
// sent now.
func (s *session) due() bool {
	at := s.nextAt
	if s.expiresAt.Before(at) {
		at = s.expiresAt
	}
	return !at.After(now())
}

// ours reports whether the pending file still holds this request. Only the
// lock holder calls it, and only it changes the file (T-I2).
func (s *session) ours() bool {
	b, exists, err := readPending(s.pendPath)
	if err != nil || !exists {
		return false
	}
	var pf struct {
		RequestID string `json:"request_id"`
	}
	return json.Unmarshal(b, &pf) == nil && pf.RequestID == s.requestID
}

// drop removes the pending file once the request has settled, but only while
// it still holds this request's id: a pending file of another request is left
// alone.
func (s *session) drop() {
	if s.ours() {
		os.Remove(s.pendPath)
	}
}

// save rewrites the pending file with the current interval and the time the
// next poll is due (L-22), so that a later run keeps the pace. A failure to
// write it is not an error of the loop: the pace holds in this run, and a later
// run polls no sooner than the interval the file holds after the next poll that
// is due by it.
func (s *session) save() {
	if !s.ours() {
		return
	}
	s.pf.Interval, s.pf.NextPollAt = s.interval, s.nextAt
	if b, err := json.Marshal(s.pf); err == nil {
		atomicfile.Write(s.pendPath, b)
	}
}

// pollReply is the body of a poll reply.
type pollReply struct {
	Status   string `json:"status"`
	Interval int    `json:"interval"`
	Name     string `json:"name"`
	Role     string `json:"role"`
	Key      string `json:"key"`
	Message  string `json:"message"`
}

// run polls until the request settles, expires, or ctx ends. When the next
// poll would come at or after the expiry, it polls once more at the expiry
// and only then, if that reply is not final, returns ErrExpired.
func (s *session) run(ctx context.Context) (Result, error) {
	for {
		if res, err, done := s.step(ctx); done {
			return res, err
		}
	}
}

// step waits for the next poll (or the final one at the expiry), sends it and
// handles the reply. done is true when the loop has ended, with its result;
// otherwise the pending file now records the pace (L-22).
func (s *session) step(ctx context.Context) (res Result, err error, done bool) {
	at, last := s.nextAt, false
	if !at.Before(s.expiresAt) {
		at, last = s.expiresAt, true
	}
	if err := sleepUntil(ctx, at); err != nil {
		return Result{}, err, true
	}
	status, body, over, err := s.post(ctx)
	arrived := now()
	if err != nil && ctx.Err() != nil {
		return Result{}, ctx.Err(), true
	}
	if err == nil { // a failure to get a reply is transient
		var b pollReply
		readable := !over && json.Unmarshal(body, &b) == nil

		switch {
		case status == http.StatusAccepted:
			if readable && b.Interval >= 1 {
				s.interval = min(b.Interval, maxSeconds)
			}
		case status == http.StatusTooManyRequests:
			if readable && b.Interval > s.interval {
				s.interval = min(b.Interval, maxSeconds)
			} else {
				s.interval = min(s.interval+2, maxSeconds)
			}
		case status == http.StatusOK:
			if over {
				return Result{}, errors.New("key request: the 200 reply is larger than 64 KiB; nothing was stored and the request is kept"), true
			}
			res, err := s.approved(readable, b)
			return res, err, true
		case status == http.StatusForbidden:
			s.drop()
			return Result{}, ErrDenied, true
		case status == http.StatusGone:
			s.drop()
			if readable && b.Status == "consumed" {
				if tok, err := readToken(s.tokenPath); err == nil && tok != "" {
					return Result{Token: tok}, nil, true
				}
				return Result{}, ErrConsumed, true
			}
			// expired, or a 410 whose body we cannot read: final either way.
			return Result{}, ErrExpired, true
		case status == http.StatusUnauthorized:
			msg := ""
			if readable {
				msg = shown(b.Message, s.secret, maxMessage)
			}
			if msg == "" {
				return Result{}, ErrUnauthorized, true
			}
			return Result{}, fmt.Errorf("%w: %s", ErrUnauthorized, msg), true
		case status >= 500:
			// transient
		default:
			return Result{}, fmt.Errorf("key request: poll answered unexpected status %d; the request is kept", status), true
		}
	}
	if last {
		s.drop()
		return Result{}, ErrExpired, true
	}
	s.nextAt = arrived.Add(s.wait())
	s.save()
	return Result{}, nil, false
}

// approved stores the key of a 200. If a token has appeared at TokenPath
// meanwhile (only something outside this package can write it while the
// request is polled), that token is kept and returned, and the received key is
// not stored.
func (s *session) approved(readable bool, b pollReply) (Result, error) {
	if !readable || b.Status != "approved" {
		return Result{}, errors.New("key request: the 200 reply is not an approved reply; the request is kept")
	}
	key := b.Key
	if !reKey.MatchString(key) {
		// The provider has handed the key out, so the request is spent.
		s.drop()
		return Result{}, errors.New("key request: the provider sent a key that breaks the key rule (16 to 512 printable ASCII characters, no white space); nothing was stored")
	}
	if tok, err := readToken(s.tokenPath); err == nil && tok != "" {
		s.drop()
		return Result{Token: tok}, nil
	}
	if err := atomicfile.Write(s.tokenPath, []byte(key)); err != nil {
		s.drop() // the key was handed out: the request is consumed
		return Result{}, fmt.Errorf("key request: approved, but the key could not be stored: %w", err)
	}
	s.drop()
	res := Result{Token: key}
	if reName.MatchString(b.Name) && !strings.Contains(b.Name, key) {
		res.Name = b.Name
	}
	if reRole.MatchString(b.Role) && !strings.Contains(b.Role, key) {
		res.Role = b.Role
	}
	return res, nil
}

// post sends one poll. A non-nil error is a failure to get a reply; over is
// true when the body was larger than MaxReply (then body is nil).
func (s *session) post(ctx context.Context) (status int, body []byte, over bool, err error) {
	cctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodPost, s.pollURL, nil)
	if err != nil {
		return 0, nil, false, errors.New("bad poll URL")
	}
	req.Header.Set("Authorization", "Bearer "+s.secret)
	req.Header.Set("Accept", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, nil, false, errors.New("poll failed") // the error text is not used; it would name the URL
	}
	defer resp.Body.Close()
	body, over, err = readReply(resp.Body)
	if err != nil {
		return 0, nil, false, errors.New("poll reply unreadable")
	}
	return resp.StatusCode, body, over, nil
}

// sleepUntil waits until t, or returns ctx's error if it ends first.
func sleepUntil(ctx context.Context, t time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	d := t.Sub(now())
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
