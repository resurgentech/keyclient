package request

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/resurgentech/keyclient/internal/atomicfile"
)

const (
	// RequestTimeout bounds each HTTP call (discovery, filing, one poll).
	RequestTimeout = 30 * time.Second
	// MaxReply is the most bytes read of any reply.
	MaxReply = 64 << 10
)

var (
	// ErrNoProvider means no request URL is known: no Override, and discovery
	// answered 404 (or ServiceURL and Override are both empty). The caller keeps
	// its usual "no token" error.
	ErrNoProvider = errors.New("key request: no key provider for this service")
	// ErrDenied means a person denied the request.
	ErrDenied = errors.New("key request: denied")
	// ErrExpired means the request expired, at the provider or here.
	ErrExpired = errors.New("key request: expired")
	// ErrConsumed means the key was handed out and not received here; the next
	// use files a new request.
	ErrConsumed = errors.New("key request: the key was already handed out and not received; request again")
	// ErrUnauthorized means the provider refused the poll secret (401). The
	// request is kept.
	ErrUnauthorized = errors.New("key request: poll refused as unauthorized")
)

// second is the unit of intervals and of expires_in; tests shrink it.
var second = time.Second

// now is the clock.
var now = time.Now

const (
	maxSeconds   = 1 << 24 // clamp for interval and expires_in, so a Duration cannot overflow
	maxMessage   = 200
	maxCode      = 64
	pendingExt   = ".pending"
	keyPattern   = `^[!-~]{16,512}$`
	discoveryURI = "/v1/key-request"
)

var (
	reNameHint = regexp.MustCompile(`^[A-Za-z0-9_.@-]{1,64}$`)
	reHost     = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
	reTool     = regexp.MustCompile(`^[A-Za-z0-9_.@/+-]{1,64}$`)
	reRole     = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,32}$`)
	reKey      = regexp.MustCompile(keyPattern)
)

// ProviderError is a filing error reply (4xx or 5xx, usually with
// {"error","message"}).
type ProviderError struct {
	Status     int
	Code       string        // for logs; a tool does not branch on it
	Message    string        // sanitised: printable characters only, at most 200 runes
	RetryAfter time.Duration // from Retry-After on 429, else 0
}

func (e *ProviderError) Error() string {
	s := fmt.Sprintf("key provider refused the request (status %d)", e.Status)
	if e.Message != "" {
		s += ": " + e.Message
	}
	if e.RetryAfter > 0 {
		s += fmt.Sprintf(" (retry after %s)", e.RetryAfter)
	}
	return s
}

// Options are the inputs of [Ensure].
type Options struct {
	// ServiceURL is the service's base URL; discovery is GET ServiceURL +
	// "/v1/key-request" (a trailing / is trimmed).
	ServiceURL string
	// Override is the request URL from the tool's flag or env var; when set,
	// discovery is not called.
	Override string
	// TokenPath is required: the file the tool reads its token from; the key is
	// written here.
	TokenPath string
	// Role is required: the role asked for (each tool documents its default).
	Role string
	// NameHint is required, for example user@host.
	NameHint string
	// Host is optional; the default is os.Hostname() up to the first dot.
	Host string
	// Tool is required: tool name and version, for example mytool/0.3.0.
	Tool string
	// HTTPClient may be nil (a new client). Either way redirects are never
	// followed: a 3xx is "any other status".
	HTTPClient *http.Client
	// Notify is called once per Ensure that files or resumes a request, before
	// polling. Nil: nothing.
	Notify func(Notice)
	// Wait true polls until the request settles. False returns *ErrPending at
	// once and polls in the background.
	Wait bool
}

// Notice is what the user is told when a request is filed or resumed.
type Notice struct {
	ApproveURL string
	UserCode   string
	Role       string // asked for
	Tool       string
	ExpiresAt  time.Time
	Resumed    bool // an earlier request was resumed from the pending file
}

// String is the one message for the user: approve at ApproveURL and check that
// the code UserCode matches, with the role and tool asked for and when the
// request expires. It never holds the poll URL, the poll secret or a key.
func (n Notice) String() string {
	s := fmt.Sprintf("Approve this key request at %s and check that the code %s matches. Asked for: role %s, tool %s. The request expires at %s.",
		n.ApproveURL, n.UserCode, n.Role, n.Tool, n.ExpiresAt.UTC().Format(time.RFC3339))
	if n.Resumed {
		s = "Resuming an earlier key request. " + s
	}
	return s
}

// ErrPending is returned by [Ensure] with Wait false after it filed or resumed
// a request. The poll goes on in the background.
type ErrPending struct {
	Notice Notice

	done chan struct{}
	err  error // set before done is closed
}

func (e *ErrPending) Error() string { return "key request pending: " + e.Notice.String() }

// Ready reports that the background poll has stored the key at TokenPath.
func (e *ErrPending) Ready() bool {
	select {
	case <-e.done:
		return e.err == nil
	default:
		return false
	}
}

// Done is closed when the background poll ends, key stored or not.
func (e *ErrPending) Done() <-chan struct{} { return e.done }

// Err is nil until Done is closed; then nil if the key was stored, else why
// (ErrDenied, ErrExpired, the context's error, ...).
func (e *ErrPending) Err() error {
	select {
	case <-e.done:
		return e.err
	default:
		return nil
	}
}

// IsTerminal reports whether f is a character device (os.ModeCharDevice). The
// check is that of the standard library only: /dev/null is a character device
// too, so it counts. A calling tool uses it to choose Wait true, or printing the
// notice and exiting 2.
func IsTerminal(f *os.File) bool {
	if f == nil {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// PendingPath returns TokenPath + ".pending".
func PendingPath(tokenPath string) string { return tokenPath + pendingExt }

var (
	ensureMu sync.Mutex
	running  = map[string]*ErrPending{} // cleaned TokenPath -> background poll in this process
)

// pendingFile is the content of the pending file.
type pendingFile struct {
	RequestID  string    `json:"request_id"`
	PollURL    string    `json:"poll_url"`
	PollSecret string    `json:"poll_secret"`
	ApproveURL string    `json:"approve_url"`
	UserCode   string    `json:"user_code"`
	Role       string    `json:"role"`
	Tool       string    `json:"tool"`
	Interval   int       `json:"interval"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// Ensure returns the tool's token. If TokenPath holds one it is returned and
// nothing else happens. Otherwise, when a request URL is known, it files a key
// request (or resumes the one kept in the pending file), calls Notify once and
// polls; see the package documentation and docs/CONTRACT.md.
//
// With Wait false the returned error is an *[ErrPending] right after filing or
// resuming; the poll then runs in a goroutine until ctx ends, so ctx must
// outlive the call.
func Ensure(ctx context.Context, o Options) (token string, err error) {
	host, err := o.validate()
	if err != nil {
		return "", err
	}
	unit := second
	tokenPath := filepath.Clean(o.TokenPath)
	pendPath := PendingPath(tokenPath)

	if tok, err := readToken(tokenPath); err != nil || tok != "" {
		return tok, err
	}

	ensureMu.Lock()
	locked := true
	unlock := func() {
		if locked {
			locked = false
			ensureMu.Unlock()
		}
	}
	defer unlock()

	if tok, err := readToken(tokenPath); err != nil || tok != "" {
		return tok, err
	}
	if ep := running[tokenPath]; ep != nil {
		unlock()
		if !o.Wait {
			return "", ep
		}
		select {
		case <-ep.done:
		case <-ctx.Done():
			return "", ctx.Err()
		}
		if ep.err != nil {
			return "", ep.err
		}
		return readToken2(tokenPath)
	}

	client := o.client()
	s := &session{
		client: client, tokenPath: tokenPath, pendPath: pendPath, unit: unit,
	}
	var notice Notice

	pf, resumed, err := loadPending(pendPath)
	if err != nil {
		return "", err
	}
	if resumed {
		s.pollURL, s.secret, s.interval, s.expiresAt = pf.PollURL, pf.PollSecret, pf.Interval, pf.ExpiresAt
		s.nextAt = now().Add(s.wait())
		notice = Notice{ApproveURL: pf.ApproveURL, UserCode: pf.UserCode, Role: pf.Role, Tool: pf.Tool, ExpiresAt: pf.ExpiresAt, Resumed: true}
	} else {
		reqURL, err := o.requestURL(ctx, client)
		if err != nil {
			return "", err
		}
		pf, arrived, err := fileRequest(ctx, client, reqURL, o, host)
		if err != nil {
			return "", err
		}
		pf.ExpiresAt = arrived.Add(time.Duration(clampSeconds(pf.expiresIn)) * unit)
		fileData, err := json.Marshal(pf.pendingFile)
		if err != nil {
			return "", err
		}
		if err := atomicfile.Write(pendPath, fileData); err != nil {
			return "", fmt.Errorf("key request: cannot store the pending request: %w", err)
		}
		s.pollURL, s.secret, s.interval, s.expiresAt = pf.PollURL, pf.PollSecret, pf.Interval, pf.ExpiresAt
		s.nextAt = arrived.Add(s.wait())
		notice = Notice{ApproveURL: pf.ApproveURL, UserCode: pf.UserCode, Role: pf.Role, Tool: pf.Tool, ExpiresAt: pf.ExpiresAt}
	}

	var ep *ErrPending
	if !o.Wait {
		ep = &ErrPending{Notice: notice, done: make(chan struct{})}
		running[tokenPath] = ep
	}
	unlock()
	if o.Notify != nil {
		o.Notify(notice)
	}
	if o.Wait {
		return s.run(ctx)
	}
	go func() {
		_, err := s.run(ctx)
		ep.err = err
		ensureMu.Lock()
		if running[tokenPath] == ep {
			delete(running, tokenPath)
		}
		ensureMu.Unlock()
		close(ep.done)
	}()
	return "", ep
}

func readToken2(path string) (string, error) {
	tok, err := readToken(path)
	if err == nil && tok == "" {
		err = errors.New("key request: the request ended but " + path + " holds no token")
	}
	return tok, err
}

// readToken returns the trimmed content of path; "" when it is missing or empty.
func readToken(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("key request: cannot read the token file: %w", err)
	}
	return strings.TrimSpace(string(b)), nil
}

func (o Options) client() *http.Client {
	var c http.Client
	if o.HTTPClient != nil {
		c = *o.HTTPClient
	}
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &c
}

// validate checks the options before any I/O and returns the host to send.
func (o Options) validate() (host string, err error) {
	if o.TokenPath == "" {
		return "", errors.New("key request: TokenPath is required")
	}
	for _, f := range []struct {
		name, v string
		re      *regexp.Regexp
	}{
		{"Role", o.Role, reRole},
		{"NameHint", o.NameHint, reNameHint},
		{"Tool", o.Tool, reTool},
	} {
		if f.v == "" {
			return "", fmt.Errorf("key request: %s is required", f.name)
		}
		if !f.re.MatchString(f.v) {
			return "", fmt.Errorf("key request: %s breaks the rule %s", f.name, f.re)
		}
	}
	host = o.Host
	if host == "" {
		full, err := os.Hostname()
		if err != nil {
			return "", fmt.Errorf("key request: Host not set and the hostname is unknown: %w", err)
		}
		host, _, _ = strings.Cut(full, ".")
	}
	if !reHost.MatchString(host) {
		return "", fmt.Errorf("key request: Host breaks the rule %s", reHost)
	}
	return host, nil
}

// absoluteHTTP reports whether s is an absolute http or https URL.
func absoluteHTTP(s string) (*url.URL, bool) {
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, false
	}
	return u, true
}

// requestURL is the Override, else the one discovery gives.
func (o Options) requestURL(ctx context.Context, c *http.Client) (string, error) {
	if o.Override != "" {
		if _, ok := absoluteHTTP(o.Override); !ok {
			return "", errors.New("key request: Override is not an absolute http or https URL")
		}
		return o.Override, nil
	}
	if o.ServiceURL == "" {
		return "", ErrNoProvider
	}
	u := strings.TrimRight(o.ServiceURL, "/") + discoveryURI
	cctx, cancel := context.WithTimeout(ctx, RequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, u, nil)
	if err != nil {
		return "", fmt.Errorf("key request: bad ServiceURL: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return "", fmt.Errorf("key request: discovery failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, MaxReply))
	switch resp.StatusCode {
	case http.StatusNotFound:
		return "", ErrNoProvider
	case http.StatusOK:
	default:
		return "", fmt.Errorf("key request: discovery answered status %d", resp.StatusCode)
	}
	var d struct {
		RequestURL string `json:"request_url"`
	}
	if json.Unmarshal(body, &d) != nil {
		return "", errors.New("key request: discovery reply is not valid JSON")
	}
	if _, ok := absoluteHTTP(d.RequestURL); !ok {
		return "", errors.New("key request: discovery gave a request_url that is not an absolute http or https URL")
	}
	return d.RequestURL, nil
}

type created struct {
	pendingFile
	expiresIn int
}

// fileRequest posts the request and validates the 201.
func fileRequest(ctx context.Context, c *http.Client, reqURL string, o Options, host string) (created, time.Time, error) {
	var zero created
	body, err := json.Marshal(map[string]string{"name_hint": o.NameHint, "host": host, "tool": o.Tool, "role": o.Role})
	if err != nil {
		return zero, time.Time{}, err
	}
	cctx, cancel := context.WithTimeout(ctx, RequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodPost, reqURL, bytes.NewReader(body))
	if err != nil {
		return zero, time.Time{}, fmt.Errorf("key request: bad request URL: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.Do(req)
	arrived := now()
	if err != nil {
		return zero, time.Time{}, fmt.Errorf("key request: filing failed: %w", err)
	}
	defer resp.Body.Close()
	reply, _ := io.ReadAll(io.LimitReader(resp.Body, MaxReply))

	if resp.StatusCode != http.StatusCreated {
		pe := &ProviderError{Status: resp.StatusCode}
		var m struct{ Error, Message string }
		if json.Unmarshal(reply, &m) == nil {
			pe.Code = sanitize(m.Error, maxCode)
			pe.Message = sanitize(m.Message, maxMessage)
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			if n, err := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After"))); err == nil && n > 0 {
				pe.RetryAfter = time.Duration(clampSeconds(n)) * time.Second
			}
		}
		return zero, time.Time{}, pe
	}

	var r struct {
		RequestID  string `json:"request_id"`
		PollURL    string `json:"poll_url"`
		PollSecret string `json:"poll_secret"`
		ApproveURL string `json:"approve_url"`
		UserCode   string `json:"user_code"`
		Interval   int    `json:"interval"`
		ExpiresIn  int    `json:"expires_in"`
	}
	if json.Unmarshal(reply, &r) != nil {
		return zero, time.Time{}, errors.New("key request: the 201 reply is not valid JSON of the expected shape")
	}
	for _, f := range []struct{ name, v string }{
		{"request_id", r.RequestID}, {"poll_secret", r.PollSecret}, {"user_code", r.UserCode},
	} {
		if f.v == "" {
			return zero, time.Time{}, fmt.Errorf("key request: the 201 reply lacks %s", f.name)
		}
	}
	if r.Interval < 1 {
		return zero, time.Time{}, errors.New("key request: the 201 reply has an interval below 1")
	}
	if r.ExpiresIn < 1 {
		return zero, time.Time{}, errors.New("key request: the 201 reply has an expires_in below 1")
	}
	pu, ok := absoluteHTTP(r.PollURL)
	if !ok {
		return zero, time.Time{}, errors.New("key request: the 201 reply has a poll_url that is not an absolute http or https URL")
	}
	if _, ok := absoluteHTTP(r.ApproveURL); !ok {
		return zero, time.Time{}, errors.New("key request: the 201 reply has an approve_url that is not an absolute http or https URL")
	}
	if ru, _ := absoluteHTTP(reqURL); ru != nil && ru.Scheme == "https" && pu.Scheme != "https" {
		return zero, time.Time{}, errors.New("key request: poll_url is not https although the request URL is; refusing to send the poll secret in clear")
	}
	return created{
		pendingFile: pendingFile{
			RequestID: r.RequestID, PollURL: r.PollURL, PollSecret: r.PollSecret, ApproveURL: r.ApproveURL,
			UserCode: r.UserCode, Role: o.Role, Tool: o.Tool, Interval: clampSeconds(r.Interval),
		},
		expiresIn: r.ExpiresIn,
	}, arrived, nil
}

// loadPending reads the pending file. resumed is false when there is none or it
// has expired (then it is removed).
func loadPending(path string) (pf pendingFile, resumed bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return pf, false, nil
		}
		return pf, false, fmt.Errorf("key request: cannot open the pending file: %w", err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return pf, false, fmt.Errorf("key request: cannot stat the pending file %s: %w", path, err)
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm()&0o077 != 0 {
		return pf, false, fmt.Errorf("key request: the pending file %s must be a regular file readable only by its owner (mode 0600); fix its mode or remove it", path)
	}
	b, err := io.ReadAll(io.LimitReader(f, MaxReply))
	if err != nil {
		return pf, false, fmt.Errorf("key request: cannot read the pending file %s: %w", path, err)
	}
	bad := fmt.Errorf("key request: the pending file %s cannot be used; remove it", path)
	dec := json.NewDecoder(bytes.NewReader(b))
	if dec.Decode(&pf) != nil || pf.PollSecret == "" || pf.Interval < 1 || pf.ExpiresAt.IsZero() {
		return pendingFile{}, false, bad
	}
	if _, ok := absoluteHTTP(pf.PollURL); !ok {
		return pendingFile{}, false, bad
	}
	pf.Interval = clampSeconds(pf.Interval)
	if !now().Before(pf.ExpiresAt) {
		os.Remove(path)
		return pendingFile{}, false, nil
	}
	return pf, true, nil
}

func clampSeconds(n int) int {
	if n > maxSeconds {
		return maxSeconds
	}
	return n
}

// sanitize keeps printable characters, turns other white space into a space,
// drops the rest, and cuts at max runes.
func sanitize(s string, max int) string {
	var b strings.Builder
	n := 0
	for _, r := range s {
		if n >= max {
			break
		}
		switch {
		case unicode.IsPrint(r):
			b.WriteRune(r)
			n++
		case unicode.IsSpace(r):
			b.WriteByte(' ')
			n++
		}
	}
	return strings.TrimSpace(b.String())
}
