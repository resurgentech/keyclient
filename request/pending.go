package request

import (
	"errors"
	"path/filepath"
	"strings"
)

var (
	// ErrPendingExpired is returned by [Pending] when the pending file records
	// a request whose time has run out.
	ErrPendingExpired = errors.New("key request: the pending request has expired")
	// ErrPendingForeign is returned by [Pending] when the pending file records
	// a request filed for another ServiceURL or Override.
	ErrPendingForeign = errors.New("key request: the pending request was filed for another service URL or override")
	// ErrPendingUnusable is matched (with errors.Is) by the error of [Pending]
	// and of [Ensure] when the pending file cannot be used: its type, mode,
	// owner, size or values break the rules. The error names the file; the
	// user removes it.
	ErrPendingUnusable = errors.New("key request: the pending file cannot be used")
)

// Pending reports the open request recorded beside the token file for these
// options, without taking the lock, polling, or changing anything.
//
// It checks the options it uses as [Ensure] does (TokenPath; ServiceURL or
// Override and the transport rule; no URL is [ErrNoProvider]) and needs no
// network. It reads the pending file the way a call waiting for another
// process's lock does (STATES.md T-30, like T-21 and T-22): without the lock,
// read-only, with every check the resume path applies (file type, mode, owner,
// size, field rules, transport rule, the poll secret held in no shown field).
//
//   - ok true: the pending file records a request that is current for these
//     options (same ServiceURL and Override, not expired); n is its notice,
//     marked Resumed, built only from values that passed the rules.
//   - ok false, err nil: there is no pending file.
//   - err matches [ErrPendingExpired], [ErrPendingForeign] or
//     [ErrPendingUnusable] (errors.Is); the unusable error names the file and
//     says to remove it, in the words Ensure uses.
//
// It never returns, logs or embeds the poll secret. It does not look at the
// token file: a caller checks that itself.
func Pending(o Options) (n Notice, ok bool, err error) {
	if o.TokenPath == "" {
		return Notice{}, false, errors.New("key request: TokenPath is required")
	}
	pol := policy{insecure: o.AllowInsecureHTTP}
	switch {
	case o.Override != "":
		_, pol, err = pol.check("Override", o.Override)
	case o.ServiceURL != "":
		_, pol, err = pol.check("ServiceURL", o.ServiceURL)
	default:
		return Notice{}, false, ErrNoProvider
	}
	if err != nil {
		return Notice{}, false, err
	}
	pendPath := PendingPath(filepath.Clean(o.TokenPath))
	pf, state, err := parsePending(pendPath, strings.TrimRight(o.ServiceURL, "/"), o.Override, pol, second)
	switch {
	case err != nil:
		return Notice{}, false, err
	case state == pendingCurrent:
		return noticeOf(pf, true), true, nil
	case state == pendingExpired:
		return Notice{}, false, ErrPendingExpired
	case state == pendingForeign:
		return Notice{}, false, ErrPendingForeign
	}
	return Notice{}, false, nil
}

// unusableError is an error about the pending file that matches
// ErrPendingUnusable, with the wording of the resume path.
type unusableError struct{ err error }

func unusable(err error) error { return &unusableError{err} }

func (e *unusableError) Error() string        { return e.err.Error() }
func (e *unusableError) Unwrap() error        { return e.err }
func (e *unusableError) Is(target error) bool { return target == ErrPendingUnusable }
