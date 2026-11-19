package request

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

	pollURL   string
	secret    string
	interval  int       // seconds, in units of s.unit
	nextAt    time.Time // when the next poll may be sent
	expiresAt time.Time
}

func (s *session) wait() time.Duration { return time.Duration(s.interval) * s.unit }

// drop removes the pending file: the request has settled.
func (s *session) drop() { os.Remove(s.pendPath) }

// run polls until the request settles, expires, or ctx ends.
func (s *session) run(ctx context.Context) (string, error) {
	for {
		if !s.nextAt.Before(s.expiresAt) {
			s.drop()
			return "", ErrExpired
		}
		if err := sleepUntil(ctx, s.nextAt); err != nil {
			return "", err
		}
		status, body, err := s.post(ctx)
		arrived := now()
		if err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			s.nextAt = arrived.Add(s.wait()) // transient
			continue
		}
		var b struct {
			Status   string `json:"status"`
			Interval int    `json:"interval"`
			Name     string `json:"name"`
			Role     string `json:"role"`
			Key      string `json:"key"`
			Error    string `json:"error"`
			Message  string `json:"message"`
		}
		readable := json.Unmarshal(body, &b) == nil

		switch {
		case status == http.StatusAccepted:
			if readable && b.Interval >= 1 {
				s.interval = clampSeconds(b.Interval)
			}
		case status == http.StatusTooManyRequests:
			if readable && b.Interval > s.interval {
				s.interval = clampSeconds(b.Interval)
			} else {
				s.interval = clampSeconds(s.interval + 2)
			}
		case status == http.StatusOK:
			return s.approved(readable, b.Status, b.Key)
		case status == http.StatusForbidden:
			s.drop()
			return "", ErrDenied
		case status == http.StatusGone:
			if readable && b.Status == "consumed" {
				if tok, err := readToken(s.tokenPath); err == nil && tok != "" {
					s.drop()
					return tok, nil
				}
				s.drop()
				return "", ErrConsumed
			}
			// expired, or a 410 whose body we cannot read: final either way.
			s.drop()
			return "", ErrExpired
		case status == http.StatusUnauthorized:
			msg := ""
			if readable {
				msg = sanitize(redact(b.Message, s.secret), maxMessage)
			}
			if msg == "" {
				return "", ErrUnauthorized
			}
			return "", fmt.Errorf("%w: %s", ErrUnauthorized, msg)
		case status >= 500:
			// transient
		default:
			return "", fmt.Errorf("key request: poll answered unexpected status %d; the request is kept", status)
		}
		s.nextAt = arrived.Add(s.wait())
	}
}

// approved stores the key of a 200.
func (s *session) approved(readable bool, status, key string) (string, error) {
	if !readable || status != "approved" {
		return "", errors.New("key request: the 200 reply is not an approved reply; the request is kept")
	}
	if !reKey.MatchString(key) {
		// The provider has handed the key out, so the request is spent.
		s.drop()
		return "", errors.New("key request: the provider sent a key that breaks the key rule (16 to 512 printable ASCII characters, no white space); nothing was stored")
	}
	if err := atomicfile.Write(s.tokenPath, []byte(key)); err != nil {
		s.drop() // the key was handed out: the request is consumed
		return "", fmt.Errorf("key request: approved, but the key could not be stored: %w", err)
	}
	s.drop()
	return key, nil
}

// post sends one poll. A non-nil error is a failure to get a reply.
func (s *session) post(ctx context.Context) (int, []byte, error) {
	cctx, cancel := context.WithTimeout(ctx, RequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodPost, s.pollURL, nil)
	if err != nil {
		return 0, nil, errors.New("bad poll URL")
	}
	req.Header.Set("Authorization", "Bearer "+s.secret)
	req.Header.Set("Accept", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, nil, errors.New("poll failed") // the error text is not used; it would name the URL
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxReply))
	if err != nil {
		return 0, nil, errors.New("poll reply unreadable")
	}
	return resp.StatusCode, body, nil
}

// redact removes the secret from s.
func redact(s, secret string) string {
	if secret == "" {
		return s
	}
	return strings.ReplaceAll(s, secret, "[redacted]")
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
