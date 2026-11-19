package keylist

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
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
)

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
	// with the hash. Nil accepts every entry.
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

// Source is a pulled key list. Create it with [New]; it is safe for
// concurrent use.
type Source struct {
	o      Options
	log    *slog.Logger
	client *http.Client

	// now and compare are replaced only by internal tests, before use.
	now     func() time.Time
	compare func(a, b []byte) int

	list atomic.Pointer[[]held] // the live list; never nil after New

	pullMu  sync.Mutex // serialises pulls; guards the next three
	etag    string     // ETag of a 200 in this process
	failing bool       // the current streak of failures has been logged
	lastErr string     // text of the last logged failure

	mu           sync.Mutex // guards the next five
	started      bool
	baseCtx      context.Context // Start's ctx; nil before Start
	inflight     *flight         // the miss pull in progress, or nil
	lastMissPull time.Time       // start of the latest miss-triggered pull
	missStarted  bool            // lastMissPull is set

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
	if o.Refresh < 0 {
		return nil, errors.New("keylist: Options.Refresh must not be negative")
	}
	if o.RefreshOnMiss < 0 {
		return nil, errors.New("keylist: Options.RefreshOnMiss must not be negative")
	}
	if o.Refresh == 0 {
		o.Refresh = DefaultRefresh
	}
	s := &Source{o: o, now: time.Now, compare: subtle.ConstantTimeCompare}
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
	// by the older cache.
	s.pullMu.Lock()
	s.loadCache()
	s.pullMu.Unlock()
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
// Options.RefreshOnMiss, after which the key is checked again. A daemon calls
// it only after its own tokens have missed.
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
			s.mu.Unlock()
			// Throttled: no wait and no pull. One more lookup (cheap) covers
			// a pull that landed between the first lookup and the lock.
			return s.Lookup(presented)
		}
		s.lastMissPull = now
		s.missStarted = true
		f = &flight{done: make(chan struct{})}
		s.inflight = f
		base := s.baseCtx
		s.mu.Unlock()
		if base == nil {
			base = context.Background()
		}
		// Not the caller's ctx: a caller giving up must not cancel the pull
		// that others wait for.
		go s.missPull(base, f)
	} else {
		s.mu.Unlock()
	}

	timer := time.NewTimer(MissWait)
	defer timer.Stop()
	select {
	case <-f.done:
	case <-timer.C:
	case <-ctx.Done():
	}
	return s.Lookup(presented)
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

// Pull makes one conditional pull now and applies it. It returns nil on 200 or
// 304, else the failure; its text never holds the pull token, a hash or URL
// userinfo. Pulls (timer, miss, Pull) are serialised.
func (s *Source) Pull(ctx context.Context) error {
	s.pullMu.Lock()
	defer s.pullMu.Unlock()
	err := s.pull(ctx)
	s.note(err)
	return err
}

// note logs the first failure of a streak (again when its text changes) and the
// recovery, not every tick. The caller holds pullMu.
func (s *Source) note(err error) {
	if err == nil {
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
	ctx, cancel := context.WithTimeout(ctx, PullTimeout)
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
	if s.etag != "" {
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
		return nil
	case resp.StatusCode == http.StatusUnauthorized:
		return fmt.Errorf("keylist: pull: %w", errUnauthorized)
	case resp.StatusCode == http.StatusOK:
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		return fmt.Errorf("keylist: pull: provider answered %d, redirects are not followed", resp.StatusCode)
	default:
		return fmt.Errorf("keylist: pull: provider answered %d", resp.StatusCode)
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
	s.etag = resp.Header.Get("ETag")
	if err := atomicfile.Write(s.o.CacheFile, body); err != nil {
		s.log.Error("key list cache not written, the list is applied anyway", "error", err.Error())
	}
	return nil
}

// parse checks a list body. Its errors name an entry's position and the rule it
// breaks, never its content.
func parse(body []byte) ([]Entry, error) {
	var v struct {
		Keys *[]struct {
			Name   string `json:"name"`
			Role   string `json:"role"`
			SHA256 string `json:"sha256"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return nil, errors.New("bad body: not a JSON object of the expected shape")
	}
	if v.Keys == nil {
		return nil, errors.New("bad body: no keys array")
	}
	out := make([]Entry, 0, len(*v.Keys))
	for i, e := range *v.Keys {
		switch {
		case !namePattern.MatchString(e.Name):
			return nil, fmt.Errorf("bad body: entry %d: name breaks its pattern", i)
		case !rolePattern.MatchString(e.Role):
			return nil, fmt.Errorf("bad body: entry %d: role breaks its pattern", i)
		case !sha256Pattern.MatchString(e.SHA256):
			return nil, fmt.Errorf("bad body: entry %d: sha256 breaks its pattern", i)
		}
		out = append(out, Entry(e))
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
		if s.o.Accept != nil {
			if ok, reason := s.o.Accept(e); !ok {
				s.log.Warn("key list entry skipped", "name", e.Name, "reason", reason)
				continue
			}
		}
		h := held{name: e.Name, role: e.Role}
		if _, err := hex.Decode(h.sum[:], []byte(e.SHA256)); err != nil {
			continue // unreachable: the pattern was checked
		}
		live = append(live, h)
	}
	s.list.Store(&live)
}

// loadCache applies CacheFile through the same checks as a pulled list.
func (s *Source) loadCache() {
	f, err := os.Open(s.o.CacheFile)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			s.log.Info("key list cache missing, starting with an empty list")
		} else {
			s.log.Warn("key list cache ignored, cannot be opened", "error", err.Error())
		}
		return
	}
	defer f.Close()
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
