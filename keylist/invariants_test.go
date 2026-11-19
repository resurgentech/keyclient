package keylist

import (
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
	"time"
)

// This file is compiled into the tests only. It gives the external tests the
// invariants of docs/STATES.md as checks, and a few hooks.

// CheckKI1 checks K-I1: the version marker is set only if the live list is the
// one that came with it and the cache file holds that same body. It returns nil
// when the marker is unset. It waits for a pull in flight.
func CheckKI1(s *Source) error {
	s.pullMu.Lock()
	defer s.pullMu.Unlock()
	if s.etag == "" {
		return nil
	}
	f, err := os.OpenFile(s.o.CacheFile, os.O_RDONLY|syscall.O_NONBLOCK, 0) // a FIFO must not hang the check
	if err != nil {
		return fmt.Errorf("K-I1: the marker %q is set but the cache cannot be opened: %w", s.etag, err)
	}
	defer f.Close()
	body, err := io.ReadAll(f)
	if err != nil {
		return fmt.Errorf("K-I1: the marker %q is set but the cache cannot be read: %w", s.etag, err)
	}
	entries, err := parse(body)
	if err != nil {
		return fmt.Errorf("K-I1: the marker is set but the cache is not a valid list: %w", err)
	}
	want := s.build(entries, false)
	got := *s.list.Load()
	if len(want) != len(got) {
		return fmt.Errorf("K-I1: the marker is set; the cache holds %d accepted entries, the live list %d", len(want), len(got))
	}
	for i := range want {
		if want[i] != got[i] {
			return errors.New("K-I1: the marker is set; the cache and the live list differ")
		}
	}
	return nil
}

// Waiters is the number of callers waiting for an in-flight miss pull (R-I2).
func Waiters(s *Source) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.waiters
}

// SetSwapHook makes s call f just before it swaps the live list.
func SetSwapHook(s *Source, f func()) { s.swapHook = f }

// TrailingArmed reports whether a trailing pull is armed.
func TrailingArmed(s *Source) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.trailing != nil
}

// Etag is the version marker E.
func Etag(s *Source) string {
	s.pullMu.Lock()
	defer s.pullMu.Unlock()
	return s.etag
}

// SetMissState puts s in the middle of a miss pull: one in flight with the
// given number of waiters, the latest miss pull having started at start.
func SetMissState(s *Source, waiters int, start time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inflight = &flight{done: make(chan struct{})}
	s.waiters = waiters
	s.missStarted = true
	s.lastMissPull = start
}

// EndMissState undoes SetMissState.
func EndMissState(s *Source) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopTrailingLocked()
	close(s.inflight.done)
	s.inflight = nil
	s.waiters = 0
}
