package request

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/resurgentech/keyclient/internal/atomicfile"
)

const (
	// RequestTimeout bounds each HTTP call (discovery, filing, one poll).
	RequestTimeout = 30 * time.Second
	// MaxReply is the most bytes of a reply that are read. A reply with a
	// longer body is not used: discovery and a 201 are refused with an error
	// (nothing is filed, shown or stored); a filing error gives a
	// [ProviderError] without code or message; a poll reply is read as a body
	// the tool cannot read (a 202 keeps the current interval, a 429 adds 2, a
	// 410 is final, a 401 has no message), except that a 200 is an error that
	// stores nothing and keeps the request.
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

// errUnsupported is returned where a request would be filed or resumed on a
// platform without the file lock.
var errUnsupported = errors.New("key request: key requests are not supported on this platform")

// second is the unit of intervals and of expires_in; tests shrink it.
var second = time.Second

// now is the clock.
var now = time.Now

// requestTimeout is the bound of each HTTP call; tests shrink it.
var requestTimeout = RequestTimeout

const (
	maxSeconds    = 1 << 24 // clamp for interval, so a Duration cannot overflow
	maxExpiresIn  = 86400   // the client limit of the contract: 24 hours
	maxMessage    = 200
	maxCode       = 64
	maxApproveURL = 2048
	maxPollSecret = 1024
	pendingExt    = ".pending"
	lockExt       = ".lock"
	discoveryURI  = "/v1/key-request"
)

var (
	reNameHint  = regexp.MustCompile(`^[A-Za-z0-9_.@-]{1,64}$`)
	reHost      = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
	reTool      = regexp.MustCompile(`^[A-Za-z0-9_.@/+-]{1,64}$`)
	reRole      = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,32}$`)
	reName      = reNameHint
	reRequestID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	reUserCode  = regexp.MustCompile(`^[A-Za-z0-9-]{1,32}$`)
	reKey       = regexp.MustCompile(`^[!-~]{16,512}$`)
)

// ProviderError is a filing error reply (4xx or 5xx, usually with
// {"error","message"}).
type ProviderError struct {
	Status     int
	Code       string        // for logs; a tool does not branch on it. Sanitised, at most 64 runes
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
	// "/v1/key-request" (a trailing / is trimmed). It must be an absolute
	// https URL, or plain http to a loopback address (see AllowInsecureHTTP).
	ServiceURL string
	// Override is the request URL from the tool's flag or env var; when set,
	// discovery is not called. The same scheme rules as ServiceURL apply.
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
	// polling. Nil: nothing. A caller that joins a flow already running in
	// this process is not notified again.
	Notify func(Notice)
	// AllowInsecureHTTP allows plain http to any host for ServiceURL, Override
	// and the URLs the provider gives (request_url, poll_url, approve_url). By
	// default plain http is refused, before anything is sent, unless the host
	// is a loopback address: "localhost" (by name), 127.0.0.0/8 or ::1.
	//
	// It never allows a downgrade: once one step of the flow is https (the
	// ServiceURL, the Override, or the request_url discovery gives), every
	// later URL of the flow must be https too, whatever this says.
	//
	// The discovery answer is trusted exactly as much as the service. Whoever
	// can answer for the service can send the tool to a request URL of their
	// choosing, relay the tool's request to the real provider, and receive the
	// key while the person sees the genuine approval page and a matching code.
	// Reaching the service over https is what protects the flow; setting this
	// gives that up, so set it only for a network path you trust.
	AllowInsecureHTTP bool
	// Wait true polls until the request settles. False returns *ErrPending
	// once the request is filed or resumed, and polls in the background.
	Wait bool
}

// Result is what [Ensure] returns: the token, and the name and role the
// provider granted with it.
//
// Name and Role are set only when this flow received the key in an approved
// reply, and each only if it matches its pattern in the contract
// (^[A-Za-z0-9_.@-]{1,64}$ and ^[A-Za-z0-9_.-]{1,32}$) and does not hold the
// key; the key is kept either way. When the token was already on disk, or was
// collected by another holder of the lock (another process, or something that
// wrote the token file while the request was polled), Name and Role are "". A
// caller that joined a flow running in this process gets the Result of that
// flow.
type Result struct {
	Token string
	Name  string
	Role  string
}

// Notice is what the user is told when a request is filed or resumed. Every
// field comes from validated values: the 201 reply (or the pending file) and
// the options.
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
// a request, or when it joins a flow already running in this process. The
// poll goes on in the background.
type ErrPending struct {
	Notice Notice

	f *flight
}

func (e *ErrPending) Error() string { return "key request pending: " + e.Notice.String() }

// Ready reports that the background poll has ended with the token at
// TokenPath.
func (e *ErrPending) Ready() bool {
	if e.f == nil {
		return false
	}
	select {
	case <-e.f.done:
		return e.f.err == nil
	default:
		return false
	}
}

// Done is closed when the background poll ends, key stored or not.
func (e *ErrPending) Done() <-chan struct{} {
	if e.f == nil {
		return nil
	}
	return e.f.done
}

// Err is nil until Done is closed; then nil if the token is at TokenPath, else
// why (ErrDenied, ErrExpired, the context's error, ...).
func (e *ErrPending) Err() error {
	if e.f == nil {
		return nil
	}
	select {
	case <-e.f.done:
		return e.f.err
	default:
		return nil
	}
}

// PendingPath returns TokenPath + ".pending".
func PendingPath(tokenPath string) string { return tokenPath + pendingExt }

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
	ServiceURL string    `json:"service_url"` // the ServiceURL of the run that filed it, trailing / trimmed
	Override   string    `json:"override"`    // the Override of that run
	RequestURL string    `json:"request_url"` // where it was filed
}

// Ensure returns the tool's token. If TokenPath holds one it is returned and
// nothing else happens. Otherwise, when a request URL is known, it files a key
// request (or resumes the one kept in the pending file), calls Notify once and
// polls; see the package documentation and docs/CONTRACT.md.
//
// With Wait false the returned error is an *[ErrPending] right after filing or
// resuming; the poll then runs in a goroutine until ctx ends, so ctx must
// outlive the call. Waiting for the lock that another process holds is bounded
// by ctx too, with Wait false as with Wait true.
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
			return o.own(ctx, f, tokenPath, host, pol)
		}
		res, err, again := f.join(ctx, o.Wait)
		if !again {
			return res, err
		}
	}
}

// own runs the flow of f: it takes the lock, then resumes or files, notifies
// and polls.
func (o Options) own(ctx context.Context, f *flight, tokenPath, host string, pol policy) (res Result, err error) {
	handedOff := false
	defer func() {
		if !handedOff {
			f.finish(res, err) // a no-op if already finished; also covers a panic in Notify
		}
	}()
	unlock, err := lockFile(ctx, tokenPath+lockExt)
	if err != nil {
		return Result{}, err
	}
	locked := true
	release := func() {
		if locked {
			locked = false
			unlock()
		}
	}
	defer release()

	if tok, err := readToken(tokenPath); err != nil || tok != "" {
		return Result{Token: tok}, err
	}
	s, notice, err := o.start(ctx, tokenPath, host, pol)
	if err != nil {
		return Result{}, err
	}
	ep := f.pending(notice)
	if o.Notify != nil {
		o.Notify(notice)
	}
	if o.Wait {
		res, err := s.run(ctx)
		f.finish(res, err)
		return res, err
	}
	// Wait false: the lock is released here; the background poll goes on
	// under the in-process table only.
	release()
	handedOff = true
	go func() {
		res, err := s.run(ctx)
		f.finish(res, err)
	}()
	return Result{}, ep
}

// start resumes the pending request, or files a new one and stores it. The
// caller holds the lock.
func (o Options) start(ctx context.Context, tokenPath, host string, pol policy) (*session, Notice, error) {
	unit := second
	client := o.client()
	pendPath := PendingPath(tokenPath)
	s := &session{client: client, tokenPath: tokenPath, pendPath: pendPath, unit: unit}
	serviceURL := strings.TrimRight(o.ServiceURL, "/")

	pf, resumed, err := loadPending(pendPath, serviceURL, o.Override, pol, unit)
	if err != nil {
		return nil, Notice{}, err
	}
	var arrived time.Time
	if resumed {
		arrived = now()
	} else {
		reqURL, rpol := o.Override, pol
		if reqURL == "" {
			if reqURL, rpol, err = o.discover(ctx, client, pol); err != nil {
				return nil, Notice{}, err
			}
		}
		var c created
		c, arrived, err = fileRequest(ctx, client, reqURL, rpol, o, host)
		if err != nil {
			return nil, Notice{}, err
		}
		pf = c.pendingFile
		pf.ExpiresAt = arrived.Add(time.Duration(min(c.expiresIn, maxExpiresIn)) * unit)
		pf.ServiceURL, pf.Override, pf.RequestURL = serviceURL, o.Override, reqURL
		data, err := json.Marshal(pf)
		if err != nil {
			return nil, Notice{}, err
		}
		if err := atomicfile.Write(pendPath, data); err != nil {
			return nil, Notice{}, fmt.Errorf("key request: cannot store the pending request: %w", err)
		}
	}
	s.requestID, s.pollURL, s.secret, s.interval, s.expiresAt = pf.RequestID, pf.PollURL, pf.PollSecret, pf.Interval, pf.ExpiresAt
	s.nextAt = arrived.Add(s.wait())
	return s, Notice{ApproveURL: pf.ApproveURL, UserCode: pf.UserCode, Role: pf.Role, Tool: pf.Tool, ExpiresAt: pf.ExpiresAt, Resumed: resumed}, nil
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
// The URLs are checked in Ensure after the token file is read: a token on disk
// is returned whatever they are.
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

// policy is the scheme rule for the next URL of a flow.
type policy struct {
	insecure bool // Options.AllowInsecureHTTP
	https    bool // a step of the flow was https: no plain http from here on
}

// check checks one URL of the flow and returns it parsed, with the policy for
// the URLs after it. The error names the URL, never its value.
func (p policy) check(name, raw string) (*url.URL, policy, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, p, fmt.Errorf("key request: %s is not an absolute http or https URL", name)
	}
	if u.Scheme == "https" {
		p.https = true
		return u, p, nil
	}
	if p.https {
		return nil, p, fmt.Errorf("key request: %s is not https although an earlier step of the flow is; refusing to downgrade", name)
	}
	if !p.insecure && !isLoopback(u.Hostname()) {
		return nil, p, fmt.Errorf("key request: %s uses plain http to a host that is not a loopback address; use https, or set AllowInsecureHTTP for a network path you trust", name)
	}
	return u, p, nil
}

// isLoopback reports whether host is "localhost" or a loopback IP address.
func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	a, err := netip.ParseAddr(host)
	return err == nil && a.Unmap().IsLoopback()
}

// readReply reads a body of at most MaxReply bytes; over is true when there was
// more (then b is nil).
func readReply(r io.Reader) (b []byte, over bool, err error) {
	b, err = io.ReadAll(io.LimitReader(r, MaxReply+1))
	if len(b) > MaxReply {
		return nil, true, err
	}
	return b, false, err
}

// discover asks the service for the request URL. pol is the policy after the
// ServiceURL; the one returned is the policy after the request URL.
func (o Options) discover(ctx context.Context, c *http.Client, pol policy) (string, policy, error) {
	u := strings.TrimRight(o.ServiceURL, "/") + discoveryURI
	cctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, u, nil)
	if err != nil {
		return "", pol, fmt.Errorf("key request: bad ServiceURL: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return "", pol, fmt.Errorf("key request: discovery failed: %w", err)
	}
	defer resp.Body.Close()
	body, over, _ := readReply(resp.Body)
	switch resp.StatusCode {
	case http.StatusNotFound:
		return "", pol, ErrNoProvider
	case http.StatusOK:
	default:
		return "", pol, fmt.Errorf("key request: discovery answered status %d", resp.StatusCode)
	}
	if over {
		return "", pol, errors.New("key request: the discovery reply is larger than 64 KiB; refused")
	}
	var d struct {
		RequestURL string `json:"request_url"`
	}
	if json.Unmarshal(body, &d) != nil {
		return "", pol, errors.New("key request: discovery reply is not valid JSON")
	}
	_, pol, err = pol.check("the request_url from discovery", d.RequestURL)
	if err != nil {
		return "", pol, err
	}
	return d.RequestURL, pol, nil
}

type created struct {
	pendingFile
	expiresIn int
}

// fileRequest posts the request and validates the 201. pol is the policy after
// the request URL.
func fileRequest(ctx context.Context, c *http.Client, reqURL string, pol policy, o Options, host string) (created, time.Time, error) {
	var zero created
	body, err := json.Marshal(map[string]string{"name_hint": o.NameHint, "host": host, "tool": o.Tool, "role": o.Role})
	if err != nil {
		return zero, time.Time{}, err
	}
	cctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodPost, reqURL, bytes.NewReader(body))
	if err != nil {
		return zero, time.Time{}, errors.New("key request: bad request URL")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.Do(req)
	arrived := now()
	if err != nil {
		return zero, time.Time{}, fmt.Errorf("key request: filing failed: %w", err)
	}
	defer resp.Body.Close()
	reply, over, rerr := readReply(resp.Body)

	if resp.StatusCode != http.StatusCreated {
		pe := &ProviderError{Status: resp.StatusCode}
		var m struct{ Error, Message string }
		if !over && json.Unmarshal(reply, &m) == nil {
			pe.Code = sanitize(m.Error, maxCode)
			pe.Message = sanitize(m.Message, maxMessage)
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			if n, err := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After"))); err == nil && n > 0 {
				pe.RetryAfter = time.Duration(min(n, maxSeconds)) * time.Second
			}
		}
		return zero, time.Time{}, pe
	}
	if over {
		return zero, time.Time{}, errors.New("key request: the 201 reply is larger than 64 KiB; refused")
	}
	if rerr != nil {
		return zero, time.Time{}, errors.New("key request: the 201 reply could not be read")
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
		return zero, time.Time{}, errors.New("key request: refused the 201 reply: it is not valid JSON of the expected shape")
	}
	if r.Interval < 1 {
		return zero, time.Time{}, errors.New("key request: refused the 201 reply: interval is below 1")
	}
	if r.ExpiresIn < 1 {
		return zero, time.Time{}, errors.New("key request: refused the 201 reply: expires_in is below 1")
	}
	if why := checkRequest(pol, r.RequestID, r.PollURL, r.PollSecret, r.ApproveURL, r.UserCode); why != "" {
		return zero, time.Time{}, errors.New("key request: refused the 201 reply: " + why)
	}
	return created{
		pendingFile: pendingFile{
			RequestID: r.RequestID, PollURL: r.PollURL, PollSecret: r.PollSecret, ApproveURL: r.ApproveURL,
			UserCode: r.UserCode, Role: o.Role, Tool: o.Tool, Interval: min(r.Interval, maxSeconds),
		},
		expiresIn: r.ExpiresIn,
	}, arrived, nil
}

// checkRequest checks the values of a request that came from the provider, in
// a 201 or in the pending file. It returns why they are refused, naming fields
// only, or "".
func checkRequest(pol policy, requestID, pollURL, secret, approveURL, userCode string) string {
	if !reRequestID.MatchString(requestID) {
		return "request_id is missing or breaks its rule"
	}
	if !reUserCode.MatchString(userCode) {
		return "user_code is missing or breaks its rule"
	}
	if len(secret) > maxPollSecret || !printableASCII(secret) {
		return "poll_secret is missing, longer than 1024 bytes, or not printable ASCII"
	}
	if len(approveURL) > maxApproveURL || !printableASCII(approveURL) {
		return "approve_url is missing, longer than 2048 bytes, or not printable ASCII without spaces"
	}
	au, _, err := pol.check("approve_url", approveURL)
	if err != nil {
		return strings.TrimPrefix(err.Error(), "key request: ")
	}
	if au.User != nil {
		return "approve_url carries user information"
	}
	if _, _, err := pol.check("poll_url", pollURL); err != nil {
		return strings.TrimPrefix(err.Error(), "key request: ")
	}
	if strings.Contains(requestID, secret) || strings.Contains(approveURL, secret) || strings.Contains(userCode, secret) {
		return "request_id, approve_url or user_code holds the poll secret"
	}
	return ""
}

// printableASCII reports whether s is not empty and every byte is 0x21 to 0x7e.
func printableASCII(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// readPending reads the pending file without following a symlink and without
// blocking on a FIFO. exists is false when there is none.
func readPending(path string) (b []byte, exists bool, err error) {
	f, err := os.OpenFile(path, pendingOpenFlags, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, true, fmt.Errorf("key request: cannot open the pending file %s: %w", path, err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, true, fmt.Errorf("key request: cannot stat the pending file %s: %w", path, err)
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm()&0o077 != 0 {
		return nil, true, fmt.Errorf("key request: the pending file %s must be a regular file readable only by its owner (mode 0600); fix its mode or remove it", path)
	}
	b, over, err := readReply(f)
	if err != nil || over {
		return nil, true, fmt.Errorf("key request: the pending file %s cannot be used; remove it (unreadable, or larger than 64 KiB)", path)
	}
	return b, true, nil
}

// loadPending reads the pending file. resumed is false when there is none, or
// when it is for another ServiceURL or Override, or has expired: then it is
// removed (the caller holds the lock). serviceURL is trimmed of a trailing /.
func loadPending(path, serviceURL, override string, pol policy, unit time.Duration) (pf pendingFile, resumed bool, err error) {
	b, exists, err := readPending(path)
	if err != nil || !exists {
		return pf, false, err
	}
	bad := func(why string) error {
		return fmt.Errorf("key request: the pending file %s cannot be used; remove it (%s)", path, why)
	}
	if json.Unmarshal(b, &pf) != nil {
		return pendingFile{}, false, bad("not valid JSON of the expected shape")
	}
	if pf.ServiceURL != serviceURL || pf.Override != override || !now().Before(pf.ExpiresAt) {
		os.Remove(path)
		return pendingFile{}, false, nil
	}
	if !reRole.MatchString(pf.Role) || !reTool.MatchString(pf.Tool) || pf.Interval < 1 {
		return pendingFile{}, false, bad("role, tool or interval breaks its rule")
	}
	_, rpol, err := pol.check("request_url", pf.RequestURL)
	if err != nil {
		return pendingFile{}, false, bad(strings.TrimPrefix(err.Error(), "key request: "))
	}
	if why := checkRequest(rpol, pf.RequestID, pf.PollURL, pf.PollSecret, pf.ApproveURL, pf.UserCode); why != "" {
		return pendingFile{}, false, bad(why)
	}
	pf.Interval = min(pf.Interval, maxSeconds)
	if limit := now().Add(maxExpiresIn * unit); pf.ExpiresAt.After(limit) {
		pf.ExpiresAt = limit
	}
	return pf, true, nil
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

// shown is a provider's text as the tool shows it: sanitised first, then the
// secret redacted, then cut at max runes, so that neither characters dropped
// by sanitising nor the cut can keep the secret from the redaction.
func shown(s, secret string, max int) string {
	s = redact(sanitize(s, utf8.RuneCountInString(s)), secret)
	if utf8.RuneCountInString(s) > max {
		s = string([]rune(s)[:max])
	}
	return strings.TrimSpace(s)
}

// redact removes the secret from s.
func redact(s, secret string) string {
	if secret == "" {
		return s
	}
	return strings.ReplaceAll(s, secret, "[redacted]")
}
