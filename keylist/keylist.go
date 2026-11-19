package keylist

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/resurgentech/keyclient/internal/atomicfile"
)

// Timing and size constants of the contract.
const (
	// DefaultRefresh is Options.Refresh when it is zero.
	DefaultRefresh = time.Minute
	// DefaultRefreshOnMiss is what a daemon's configuration should default
	// refresh_on_miss to. Options.RefreshOnMiss zero means off, not this.
	DefaultRefreshOnMiss = 5 * time.Second
	// MissWait is the longest a miss waits for an in-flight pull.
	MissWait = 2 * time.Second
	// PullTimeout bounds each pull.
	PullTimeout = 10 * time.Second
	// MaxBody is the most bytes read of a list body; a larger body is a failure.
	MaxBody = 4 << 20
	// MaxMissWaiters is the most callers of [Source.LookupOrRefresh] that wait
	// at once for an in-flight miss pull. A miss past it answers at once, as
	// if throttled, without waiting.
	MaxMissWaiters = 100
)

// ErrCacheWrite is wrapped by the error of [Source.Pull] when the list was
// applied but could not be written to the cache file. The list is live in
// memory regardless; the pull counts as a success for [Status.LastSuccess],
// but the error is [Status.LastError] and [Status.CacheFailing] is set.
var ErrCacheWrite = errors.New("key list cache not written")

// Status is the health of a [Source], from [Source.Status].
//
// There is no staleness cut-off: while the provider is down the Source keeps
// serving the last good list, however old. That is a deliberate availability
// choice, so a daemon that wants to alarm on age reads Age.
type Status struct {
	// LastSuccess is the time of the last good pull (a 200, or a 304 after a
	// conditional request) in this process; zero if there has been none. A 200
	// whose cache write failed is a good pull.
	LastSuccess time.Time
	// LastError is nil unless the latest pull attempt failed. It wraps
	// [ErrCacheWrite] when the list was applied but not cached.
	LastError error
	// Age is the time since LastSuccess; 0 when LastSuccess is zero.
	Age time.Duration
	// Entries is the number of live entries.
	Entries int
	// CacheFailing is true when the latest cache write failed (the list is
	// live in memory regardless). It clears at the next successful write.
	CacheFailing bool
}

// Entry is one entry of the key list.
type Entry struct {
	Name   string
	Role   string
	SHA256 string // lowercase hex
}

// Options configures a [Source].
type Options struct {
	// URL is the list URL, absolute, http or https. Required.
	URL string
	// TokenFile optionally holds the pull token (surrounding whitespace is
	// trimmed; it must not be empty). It is read at New and again before every
	// pull, so a rotated token is picked up.
	TokenFile string
	// CacheFile is where the last good list is kept. Required.
	CacheFile string
	// Refresh is the time between pulls; zero means DefaultRefresh.
	Refresh time.Duration
	// RefreshOnMiss is the least time between two pulls started by an unknown
	// key; zero turns refresh on miss off. A daemon passes its configured
	// value, whose default is DefaultRefreshOnMiss.
	RefreshOnMiss time.Duration
	// Accept decides about each entry of a good list (and of the cache), in
	// order, after the repeat check. A false is logged with its reason, never
	// with the hash. Required: New returns an error when it is nil, so that a
	// service cannot forget the contract's unknown-role and local-tokens-win
	// rules. [AcceptRoles] implements them. A service that really accepts every
	// entry says so with a function that returns true.
	Accept func(Entry) (ok bool, reason string)
	// HTTPClient is used for pulls. A shallow copy is made that never follows
	// redirects. Nil means a new client.
	HTTPClient *http.Client
	// Logger receives the log lines; nil means slog.Default().
	Logger *slog.Logger
}

var (
	namePattern   = regexp.MustCompile(`^[A-Za-z0-9_.@-]{1,64}$`)
	rolePattern   = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,32}$`)
	sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// held is one live entry: the name and role to return, and the hash to compare.
type held struct {
	name, role string
	sum        [sha256.Size]byte
}

// flight is one miss-triggered pull in progress.
type flight struct{ done chan struct{} }

// AcceptRoles returns an [Options.Accept] that honours the contract's
// unknown-role and local-tokens-win rules. It accepts an entry only when its
// Role is one of roles (exact match) and takenLocally returns false for it. The
// reasons it gives are "unknown role" and "name or key already in the local
// token source". takenLocally gets the whole Entry, so it can check the name
// and the SHA-256; nil means nothing is taken locally.
func AcceptRoles(roles []string, takenLocally func(Entry) bool) func(Entry) (ok bool, reason string) {
	known := append([]string(nil), roles...)
	return func(e Entry) (bool, string) {
		found := false
		for _, r := range known {
			if r == e.Role {
				found = true
				break
			}
		}
		if !found {
			return false, "unknown role"
		}
		if takenLocally != nil && takenLocally(e) {
			return false, "name or key already in the local token source"
		}
		return true, ""
	}
}

// Source is a pulled key list. Create it with [New]; it is safe for
// concurrent use. While the provider is down it keeps serving the last good
// list with no staleness cut-off, a deliberate availability choice; see
// [Source.Status].
type Source struct {
	o      Options
	log    *slog.Logger
	client *http.Client

	// now and compare are replaced only by internal tests, before use.
	now         func() time.Time
	compare     func(a, b []byte) int
	pullTimeout time.Duration // PullTimeout; internal tests shrink it

	list atomic.Pointer[[]held] // the live list; never nil after New

	pullMu  sync.Mutex // serialises pulls; guards the next three
	etag    string     // ETag of a 200 in this process
	failing bool       // the current streak of failures has been logged
	lastErr string     // text of the last logged failure
	applied bool       // a 200 has been applied in this process

	statusMu     sync.Mutex // guards the next three
	lastSuccess  time.Time
	lastPullErr  error
	cacheFailing bool

	mu           sync.Mutex // guards the next eight
	started      bool
	baseCtx      context.Context // Start's ctx; nil before Start
	inflight     *flight         // the miss pull in progress, or nil
	lastMissPull time.Time       // start of the latest miss-triggered pull
	missStarted  bool            // lastMissPull is set
	waiters      int             // callers waiting for an in-flight miss pull
	trailing     *time.Timer     // the trailing pull of the current window, or nil
	trailGen     int             // identifies the current trailing timer

	timerPulls atomic.Int64 // pulls started by Start's timer (tests)
}

// New checks the options, reads the token file when set, and returns a Source
// holding an empty list. Nothing is loaded or pulled until [Source.Start].
func New(o Options) (*Source, error) {
	u, err := url.Parse(o.URL)
	if o.URL == "" || err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		// The URL is not echoed: it may hold userinfo.
		return nil, errors.New("keylist: Options.URL must be an absolute http or https URL")
	}
	if o.CacheFile == "" {
		return nil, errors.New("keylist: Options.CacheFile is required")
	}
	if o.Accept == nil {
		return nil, errors.New("keylist: Options.Accept is required (see AcceptRoles)")
	}
	if o.Refresh < 0 {
		return nil, errors.New("keylist: Options.Refresh must not be negative")
	}
	if o.RefreshOnMiss < 0 {
		return nil, errors.New("keylist: Options.RefreshOnMiss must not be negative")
	}
	if o.Refresh == 0 {
		o.Refresh = DefaultRefresh
	}
	s := &Source{o: o, now: time.Now, compare: subtle.ConstantTimeCompare, pullTimeout: PullTimeout}
	if o.TokenFile != "" {
		if _, err := s.readToken(); err != nil {
			return nil, err
		}
	}
	s.log = o.Logger
	if s.log == nil {
		s.log = slog.Default()
	}
	if o.HTTPClient != nil {
		c := *o.HTTPClient
		s.client = &c
	} else {
		s.client = &http.Client{}
	}
	s.client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	empty := []held{}
	s.list.Store(&empty)
	return s, nil
}

// readToken reads the pull token file. Errors name the file, never its content.
func (s *Source) readToken() (string, error) {
	b, err := os.ReadFile(s.o.TokenFile)
	if err != nil {
		return "", fmt.Errorf("keylist: read token file: %w", err)
	}
	tok := strings.TrimSpace(string(b))
	if tok == "" {
		return "", errors.New("keylist: token file is empty")
	}
	return tok, nil
}

// Start loads CacheFile (synchronously), then in a goroutine pulls at once and
// every Refresh until ctx ends. It returns an error only if called twice. The
// service starts even when the provider is down.
func (s *Source) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return errors.New("keylist: Start called twice")
	}
	s.started = true
	s.baseCtx = ctx
	s.mu.Unlock()

	// Under pullMu, so a miss pull started before Start cannot be overwritten
	// by the older cache. A list already pulled in this process is newer than
	// any cache, so the cache is then not loaded at all.
	s.pullMu.Lock()
	if !s.applied {
		s.loadCache()
	}
	s.pullMu.Unlock()
	// The trailing pull is not started once ctx has ended.
	context.AfterFunc(ctx, func() {
		s.mu.Lock()
		s.stopTrailingLocked()
		s.mu.Unlock()
	})
	tick := time.NewTicker(s.o.Refresh) // created once; the miss path never touches it
	go func() {
		defer tick.Stop()
		s.timerPull(ctx)
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				s.timerPull(ctx)
			}
		}
	}()
	return nil
}

func (s *Source) timerPull(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	s.timerPulls.Add(1)
	_ = s.Pull(ctx) // a failure is logged by Pull
}

// Len is the number of live entries.
func (s *Source) Len() int { return len(*s.list.Load()) }

// Lookup hashes presented with SHA-256 and compares the hash in constant time
// with every entry's hash, with no early exit. An empty presented never
// matches.
func (s *Source) Lookup(presented string) (name, role string, ok bool) {
	if presented == "" {
		return "", "", false
	}
	sum := sha256.Sum256([]byte(presented))
	match := 0
	var hit *held
	list := *s.list.Load()
	for i := range list {
		if s.compare(sum[:], list[i].sum[:]) == 1 {
			match = 1
			hit = &list[i]
		}
	}
	if match == 0 {
		return "", "", false
	}
	return hit.name, hit.role, true
}

// LookupOrRefresh is [Source.Lookup], and on a miss it refreshes on miss: one
// conditional pull, joined by concurrent misses, at most one started per
// Options.RefreshOnMiss, after which the key is checked again. At most
// [MaxMissWaiters] callers wait at once; a miss past that answers at once. When
// a miss was refused a pull, one more miss-triggered pull starts when its
// window ends. A daemon calls it only after its own tokens have missed.
func (s *Source) LookupOrRefresh(ctx context.Context, presented string) (name, role string, ok bool) {
	if name, role, ok = s.Lookup(presented); ok {
		return name, role, true
	}
	if presented == "" || s.o.RefreshOnMiss == 0 {
		return "", "", false
	}

	s.mu.Lock()
	f := s.inflight
	if f == nil {
		now := s.now()
		if s.missStarted && now.Sub(s.lastMissPull) < s.o.RefreshOnMiss {
			s.scheduleTrailingLocked(now)
			s.mu.Unlock()
			// Throttled: no wait and no pull. One more lookup (cheap) covers
			// a pull that landed between the first lookup and the lock.
			return s.Lookup(presented)
		}
		var base context.Context
		f, base = s.startFlightLocked(now)
		wait := s.waiters < MaxMissWaiters
		if wait {
			s.waiters++
		}
		s.mu.Unlock()
		// Not the caller's ctx: a caller giving up must not cancel the pull
		// that others wait for.
		go s.missPull(base, f)
		if !wait {
			return s.Lookup(presented)
		}
	} else {
		if s.waiters >= MaxMissWaiters {
			// Refused without a pull: the window's trailing pull covers it.
			s.scheduleTrailingLocked(s.now())
			s.mu.Unlock()
			return s.Lookup(presented)
		}
		s.waiters++
		s.mu.Unlock()
	}
	defer func() {
		s.mu.Lock()
		s.waiters--
		s.mu.Unlock()
	}()

	timer := time.NewTimer(MissWait)
	defer timer.Stop()
	select {
	case <-f.done:
	case <-timer.C:
	case <-ctx.Done():
	}
	return s.Lookup(presented)
}

// startFlightLocked records a miss-triggered pull starting at now and returns
// its flight and the context to pull with. The caller holds mu and starts
// missPull after unlocking.
func (s *Source) startFlightLocked(now time.Time) (*flight, context.Context) {
	s.lastMissPull = now
	s.missStarted = true
	s.stopTrailingLocked() // a new window: any trailing pull belonged to the old one
	f := &flight{done: make(chan struct{})}
	s.inflight = f
	base := s.baseCtx
	if base == nil {
		base = context.Background()
	}
	return f, base
}

func (s *Source) stopTrailingLocked() {
	if s.trailing != nil {
		s.trailing.Stop()
		s.trailing = nil
	}
	s.trailGen++
}

// scheduleTrailingLocked arranges one miss-triggered pull at the end of the
// current window, unless one is already arranged or Start's ctx has ended. The
// caller holds mu.
func (s *Source) scheduleTrailingLocked(now time.Time) {
	if s.trailing != nil || (s.baseCtx != nil && s.baseCtx.Err() != nil) {
		return
	}
	d := s.lastMissPull.Add(s.o.RefreshOnMiss).Sub(now)
	if d < 0 {
		d = 0
	}
	gen := s.trailGen
	s.trailing = time.AfterFunc(d, func() { s.trailingPull(gen) })
}

// trailingPull is the miss-triggered pull at the end of a window in which a
// miss was refused a pull. It counts as the next window's pull.
func (s *Source) trailingPull(gen int) {
	s.mu.Lock()
	if gen != s.trailGen || s.trailing == nil {
		s.mu.Unlock()
		return
	}
	s.trailing = nil
	now := s.now()
	if s.inflight != nil || now.Sub(s.lastMissPull) < s.o.RefreshOnMiss ||
		(s.baseCtx != nil && s.baseCtx.Err() != nil) {
		s.mu.Unlock()
		return
	}
	f, base := s.startFlightLocked(now)
	s.mu.Unlock()
	s.missPull(base, f)
}

func (s *Source) missPull(ctx context.Context, f *flight) {
	defer func() {
		s.mu.Lock()
		s.inflight = nil
		close(f.done)
		s.mu.Unlock()
	}()
	_ = s.Pull(ctx) // a failure is logged by Pull
}

// Status reports the health of the Source: when the last good pull was, whether
// the latest attempt failed, how old the list is, how many entries are live,
// and whether the cache is failing. A daemon can expose it on a health
// endpoint. There is no staleness cut-off: the last good list is served for as
// long as the provider is down.
func (s *Source) Status() Status {
	s.statusMu.Lock()
	st := Status{LastSuccess: s.lastSuccess, LastError: s.lastPullErr, CacheFailing: s.cacheFailing}
	s.statusMu.Unlock()
	if !st.LastSuccess.IsZero() {
		st.Age = s.now().Sub(st.LastSuccess)
	}
	st.Entries = s.Len()
	return st
}

// Pull makes one conditional pull now and applies it. It returns nil on 200 or
// 304, else the failure; its text never holds the pull token, a hash or URL
// userinfo. When the list was applied but could not be cached, the error wraps
// [ErrCacheWrite]: the list is live in memory regardless. Pulls (timer, miss,
// Pull) are serialised.
func (s *Source) Pull(ctx context.Context) error {
	s.pullMu.Lock()
	defer s.pullMu.Unlock()
	err := s.pull(ctx)
	s.note(err)
	s.statusMu.Lock()
	if err == nil || errors.Is(err, ErrCacheWrite) {
		s.lastSuccess = s.now()
	}
	s.lastPullErr = err
	s.statusMu.Unlock()
	return err
}

// note logs the first failure of a streak (again when its text changes) and the
// recovery, not every tick. The caller holds pullMu.
func (s *Source) note(err error) {
	if err == nil || errors.Is(err, ErrCacheWrite) { // the latter was logged where it happened
		if s.failing {
			s.log.Info("key list pull recovered")
		}
		s.failing, s.lastErr = false, ""
		return
	}
	msg := err.Error()
	if s.failing && msg == s.lastErr {
		return
	}
	s.failing, s.lastErr = true, msg
	if errors.Is(err, errUnauthorized) {
		s.log.Error("key list pull refused, the pull token needs an operator", "error", msg)
		return
	}
	s.log.Warn("key list pull failed, keeping the last good list", "error", msg)
}

var errUnauthorized = errors.New("provider answered 401")

// pull does one request. The caller holds pullMu.
func (s *Source) pull(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, s.pullTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.o.URL, nil)
	if err != nil {
		return errors.New("keylist: pull: cannot build request")
	}
	req.Header.Set("Accept", "application/json")
	if s.o.TokenFile != "" {
		tok, err := s.readToken()
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	conditional := s.etag != ""
	if conditional {
		req.Header.Set("If-None-Match", s.etag)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		// A *url.Error carries the URL, which may hold userinfo: keep only
		// the cause.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("keylist: pull: %w", err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotModified:
		if !conditional {
			return errors.New("keylist: pull: provider answered 304 to a request without If-None-Match")
		}
		return nil
	case resp.StatusCode == http.StatusUnauthorized:
		return fmt.Errorf("keylist: pull: %w", errUnauthorized)
	case resp.StatusCode == http.StatusOK:
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		return fmt.Errorf("keylist: pull: provider answered %d, redirects are not followed", resp.StatusCode)
	default:
		return fmt.Errorf("keylist: pull: provider answered %d", resp.StatusCode)
	}

	if mt, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		return errors.New("keylist: pull: the 200 is not application/json")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxBody+1))
	if err != nil {
		return fmt.Errorf("keylist: pull: reading body: %w", err)
	}
	if len(body) > MaxBody {
		return fmt.Errorf("keylist: pull: body larger than %d bytes", MaxBody)
	}
	entries, err := parse(body)
	if err != nil {
		return fmt.Errorf("keylist: pull: %w", err)
	}
	s.apply(entries)
	s.applied = true
	s.etag = resp.Header.Get("ETag")
	if err := atomicfile.Write(s.o.CacheFile, body); err != nil {
		// The cache must never hold a list older than the one now live: take
		// the old one away, or if that fails empty it.
		s.dropCache()
		s.log.Error("key list cache not written, the list is applied anyway", "error", err.Error())
		s.setCacheFailing(true)
		return fmt.Errorf("keylist: pull: %w: %w", ErrCacheWrite, err)
	}
	s.setCacheFailing(false)
	return nil
}

func (s *Source) setCacheFailing(v bool) {
	s.statusMu.Lock()
	s.cacheFailing = v
	s.statusMu.Unlock()
}

// dropCache removes the stale cache file, or if it cannot (a read-only
// directory), truncates it, which needs only write permission on the file.
func (s *Source) dropCache() {
	err := os.Remove(s.o.CacheFile)
	if err == nil || errors.Is(err, os.ErrNotExist) {
		return
	}
	if err := os.Truncate(s.o.CacheFile, 0); err != nil && !errors.Is(err, os.ErrNotExist) {
		s.log.Error("key list stale cache cannot be removed or emptied, a restart may load an older list", "error", err.Error())
	}
}

// parse checks a list body, strictly: one JSON object, no member name twice
// and none that differs from a known name only in letter case, nothing after
// it. Unknown members are skipped. Its errors name an entry's position and the
// rule it breaks, never its content.
func parse(body []byte) ([]Entry, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, errors.New("bad body: not a JSON object")
	}
	var out []Entry
	haveKeys := false
	seen := map[string]bool{}
	for dec.More() {
		k, err := memberName(dec, seen, "keys")
		if err != nil {
			return nil, err
		}
		if k != "keys" {
			if err := skipValue(dec); err != nil {
				return nil, err
			}
			continue
		}
		if out, err = parseEntries(dec); err != nil {
			return nil, err
		}
		haveKeys = true
	}
	if err := closeObject(dec); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("bad body: data after the object")
	}
	if !haveKeys {
		return nil, errors.New("bad body: no keys array")
	}
	return out, nil
}

// memberName reads an object member name and refuses a repeat, or a name that
// differs from one of known only in letter case.
func memberName(dec *json.Decoder, seen map[string]bool, known ...string) (string, error) {
	t, err := dec.Token()
	k, isString := t.(string)
	if err != nil || !isString {
		return "", errors.New("bad body: not valid JSON")
	}
	if seen[k] {
		return "", errors.New("bad body: a member name is repeated")
	}
	seen[k] = true
	for _, n := range known {
		if k != n && strings.EqualFold(k, n) {
			return "", errors.New("bad body: a member name differs from a known one only in letter case")
		}
	}
	return k, nil
}

func closeObject(dec *json.Decoder) error {
	if t, err := dec.Token(); err != nil || t != json.Delim('}') {
		return errors.New("bad body: not valid JSON")
	}
	return nil
}

// skipValue consumes one value, nested ones included.
func skipValue(dec *json.Decoder) error {
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		return errors.New("bad body: not valid JSON")
	}
	return nil
}

func parseEntries(dec *json.Decoder) ([]Entry, error) {
	if t, err := dec.Token(); err != nil || t != json.Delim('[') {
		return nil, errors.New("bad body: keys is not an array")
	}
	out := []Entry{}
	for i := 0; dec.More(); i++ {
		if t, err := dec.Token(); err != nil || t != json.Delim('{') {
			return nil, fmt.Errorf("bad body: entry %d: not an object", i)
		}
		var e Entry
		seen := map[string]bool{}
		for dec.More() {
			k, err := memberName(dec, seen, "name", "role", "sha256")
			if err != nil {
				return nil, fmt.Errorf("entry %d: %w", i, err)
			}
			var dst *string
			switch k {
			case "name":
				dst = &e.Name
			case "role":
				dst = &e.Role
			case "sha256":
				dst = &e.SHA256
			default:
				if err := skipValue(dec); err != nil {
					return nil, err
				}
				continue
			}
			t, err := dec.Token()
			v, isString := t.(string)
			if err != nil || !isString {
				return nil, fmt.Errorf("bad body: entry %d: %s is not a string", i, k)
			}
			*dst = v
		}
		if err := closeObject(dec); err != nil {
			return nil, err
		}
		switch {
		case !namePattern.MatchString(e.Name):
			return nil, fmt.Errorf("bad body: entry %d: name breaks its pattern", i)
		case !rolePattern.MatchString(e.Role):
			return nil, fmt.Errorf("bad body: entry %d: role breaks its pattern", i)
		case !sha256Pattern.MatchString(e.SHA256):
			return nil, fmt.Errorf("bad body: entry %d: sha256 breaks its pattern", i)
		}
		out = append(out, e)
	}
	if t, err := dec.Token(); err != nil || t != json.Delim(']') {
		return nil, errors.New("bad body: not valid JSON")
	}
	return out, nil
}

// apply skips repeats and refused entries (logging name and reason, never a
// hash) and swaps the rest in at once.
func (s *Source) apply(entries []Entry) {
	names := make(map[string]bool, len(entries))
	sums := make(map[string]bool, len(entries))
	live := make([]held, 0, len(entries))
	for _, e := range entries {
		if names[e.Name] {
			s.log.Warn("key list entry skipped", "name", e.Name, "reason", "name repeated in the list")
			continue
		}
		if sums[e.SHA256] {
			s.log.Warn("key list entry skipped", "name", e.Name, "reason", "key repeated in the list")
			continue
		}
		names[e.Name], sums[e.SHA256] = true, true
		if ok, reason := s.o.Accept(e); !ok {
			s.log.Warn("key list entry skipped", "name", e.Name, "reason", reason)
			continue
		}
		h := held{name: e.Name, role: e.Role}
		if _, err := hex.Decode(h.sum[:], []byte(e.SHA256)); err != nil {
			continue // unreachable: the pattern was checked
		}
		live = append(live, h)
	}
	s.list.Store(&live)
}

// loadCache applies CacheFile through the same checks as a pulled list. The
// file is opened without blocking (a FIFO must not hang Start) and must be a
// regular file that group and others cannot write.
func (s *Source) loadCache() {
	f, err := os.OpenFile(s.o.CacheFile, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			s.log.Info("key list cache missing, starting with an empty list")
		} else {
			s.log.Warn("key list cache ignored, cannot be opened", "error", err.Error())
		}
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		s.log.Warn("key list cache ignored, cannot be examined", "error", err.Error())
		return
	}
	if !st.Mode().IsRegular() {
		s.log.Warn("key list cache ignored, not a regular file")
		return
	}
	if cacheWritableByOthers(st.Mode()) {
		s.log.Warn("key list cache ignored, group or others can write it", "mode", st.Mode().Perm().String())
		return
	}
	body, err := io.ReadAll(io.LimitReader(f, MaxBody+1))
	if err != nil || len(body) > MaxBody {
		s.log.Warn("key list cache ignored, cannot be read or is too large")
		return
	}
	entries, err := parse(body)
	if err != nil {
		s.log.Warn("key list cache ignored", "error", err.Error())
		return
	}
	s.apply(entries)
	s.log.Info("key list cache loaded", "entries", s.Len())
}
