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
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
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
	// ErrBusy is returned with Wait false when another process holds the lock
	// of this token and there is no request of this run's service in the
	// pending file to report: that process is filing one, or is about to end.
	// Nothing was filed or shown. Call Ensure again shortly. It is also the
	// end of the background wait of a Wait false call (see [ErrPending]) when,
	// once the lock is free, the request it reported has ended in that other
	// process without a token here.
	ErrBusy = errors.New("key request: another process is handling a key request for this token; try again shortly")
)

// errUnsupported is returned where a request would be filed or resumed on a
// platform without the file lock. It wraps errors.ErrUnsupported.
var errUnsupported = fmt.Errorf("key request: key requests are not supported on this platform: %w", errors.ErrUnsupported)

// hostname is the source of the default Host; tests replace it.
var hostname = os.Hostname

// geteuid is the user that must own the lock and pending files: the effective
// user, the one whose rights open and create them (T-28); tests replace it.
var geteuid = os.Geteuid

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
	// Host is optional; the default is os.Hostname() up to the first dot. The
	// key's name needs one: when the default is empty or breaks the rule
	// ^[A-Za-z0-9_.-]{1,64}$, Ensure returns an error that names Host.
	Host string
	// Tool is required: tool name and version, for example mytool/0.3.0.
	Tool string
	// HTTPClient may be nil (a new client). Either way redirects are never
	// followed: a 3xx is "any other status".
	HTTPClient *http.Client
	// Notify is called once per Ensure that files or resumes a request, before
	// polling, in the caller's goroutine; except that with Wait false, when a
	// poll of a resumed request is already due, that poll comes first and
	// Notify is called after it only if the request is still open (a call that
	// collects the key, or finds the request ended, shows no notice). It is
	// also called, with a notice
	// marked resumed, when another process holds the lock and the pending file
	// holds its request: then before waiting for that lock (Wait true) or
	// before returning *ErrPending (Wait false); a request shown this way is
	// not shown again when this call resumes it. Nil: nothing. A caller that
	// joins a flow already running in this process is not notified again. A
	// panic in Notify reaches the caller of Ensure; the flow ends with an error
	// for every caller that joined it, the lock is released and the pending
	// file kept.
	//
	// The notice holds a URL from the provider. Never pass Notice.ApproveURL
	// (or Notice.String()) to a shell or a URL opener without the user's
	// consent.
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
	// once the request is filed or resumed, and polls in the background,
	// holding the lock until that poll ends; when a poll of a resumed request
	// is already due, that one poll is made first (before Notify), and if it
	// settles the request its result is returned instead, with no notice. A
	// call with Wait false never
	// waits for a person or for another process: only the time limits of its
	// network calls bound it.
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
//
// ApproveURL comes from the provider. Never pass it (or String()) to a shell or
// a URL opener without the user's consent.
type Notice struct {
	// Service is the host (and port, if any) of the service the key is for:
	// the host of ServiceURL when ServiceURL is set (with or without an
	// Override), and the host of Override only when there is no ServiceURL
	// (then it is the provider's host). It comes from the configuration, not
	// from the provider; a resumed notice takes it from the pending file,
	// which records both.
	Service    string
	ApproveURL string
	UserCode   string
	Role       string // asked for
	Tool       string
	ExpiresAt  time.Time
	Resumed    bool // an earlier request was resumed from the pending file
}

// String is the one message for the user: approve at ApproveURL and check that
// the code UserCode matches, with the service, the role and tool asked for and
// when the request expires. It never holds the poll URL, the poll secret or a
// key.
func (n Notice) String() string {
	what := "this key request"
	if n.Service != "" {
		what = "this key request for " + n.Service
	}
	s := fmt.Sprintf("Approve %s at %s and check that the code %s matches. Asked for: role %s, tool %s. The request expires at %s.",
		what, n.ApproveURL, n.UserCode, n.Role, n.Tool, n.ExpiresAt.UTC().Format(time.RFC3339))
	if n.Resumed {
		s = "Resuming an earlier key request. " + s
	}
	return s
}

// ErrPending is returned by [Ensure] with Wait false after it filed or resumed
// a request, or when it joins a flow already running in this process. The
// poll goes on in the background and holds the lock until it ends.
//
// It is also returned, at once, when another process holds the lock and the
// pending file holds a current request: its Notice is built from that file
// (Resumed is true), and a background goroutine waits for the lock (bounded by
// the context), then returns the token another holder stored, or resumes and
// polls that same request. If that request has ended without a token here, Err
// is [ErrBusy].
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
	Interval   int       `json:"interval"`     // the current interval I (L-22)
	NextPollAt time.Time `json:"next_poll_at"` // when the next poll is due (L-22); absent in older files: now
	ExpiresAt  time.Time `json:"expires_at"`
	ServiceURL string    `json:"service_url"` // the ServiceURL of the run that filed it, trailing / trimmed
	Override   string    `json:"override"`    // the Override of that run
	RequestURL string    `json:"request_url"` // where it was filed
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
		full, err := hostname()
		if err != nil {
			return "", fmt.Errorf("key request: Host is not set and the hostname is unknown (%v); set Host", err)
		}
		host, _, _ = strings.Cut(full, ".")
		if !reHost.MatchString(host) {
			return "", fmt.Errorf("key request: Host is not set and the short hostname %q is empty or breaks the rule %s; set Host", sanitize(host, 64), reHost)
		}
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
	if holdsSecret(requestID, secret) || holdsSecret(approveURL, secret) || holdsSecret(userCode, secret) {
		return "request_id, approve_url or user_code holds the poll secret"
	}
	return ""
}

// holdsSecret reports whether s holds secret as it is, or once URL-unescaped
// as a path or as a query (where + is a space). Unescaping is lenient (a broken
// escape is kept as it is, so it cannot hide the rest) and repeated until
// nothing changes (so escaping twice hides nothing either).
func holdsSecret(s, secret string) bool {
	for _, plus := range []bool{false, true} {
		for v := s; ; {
			if strings.Contains(v, secret) {
				return true
			}
			u := unescape(v, plus)
			if u == v {
				break
			}
			v = u
		}
	}
	return false
}

// unescape decodes each valid %XX of s, and with plus each + as a space;
// everything else is kept.
func unescape(s string, plus bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '%' && i+2 < len(s) && isHex(s[i+1]) && isHex(s[i+2]):
			b.WriteByte(unhex(s[i+1])<<4 | unhex(s[i+2]))
			i += 2
		case c == '+' && plus:
			b.WriteByte(' ')
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

func isHex(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}

func unhex(c byte) byte {
	switch {
	case c <= '9':
		return c - '0'
	case c <= 'F':
		return c - 'A' + 10
	}
	return c - 'a' + 10
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
	if !ownedByMe(fi) {
		return nil, true, fmt.Errorf("key request: the pending file %s is not owned by this user; remove it", path)
	}
	b, over, err := readReply(f)
	if err != nil || over {
		return nil, true, fmt.Errorf("key request: the pending file %s cannot be used; remove it (unreadable, or larger than 64 KiB)", path)
	}
	return b, true, nil
}

// The states of the pending file P that parsePending tells apart (STATES.md,
// machine 2); unusable is an error.
const (
	pendingAbsent  = iota
	pendingCurrent // filed for this run's service URL and override, not expired
	pendingStale   // foreign (another service URL or override) or expired
)

// parsePending reads the pending file and tells its state. It changes nothing.
// serviceURL is trimmed of a trailing /. A current file has its interval and
// expiry clamped.
func parsePending(path, serviceURL, override string, pol policy, unit time.Duration) (pf pendingFile, state int, err error) {
	b, exists, err := readPending(path)
	if err != nil || !exists {
		return pf, pendingAbsent, err
	}
	bad := func(why string) error {
		return fmt.Errorf("key request: the pending file %s cannot be used; remove it (%s)", path, why)
	}
	if json.Unmarshal(b, &pf) != nil {
		return pendingFile{}, pendingAbsent, bad("not valid JSON of the expected shape")
	}
	if pf.ServiceURL != serviceURL || pf.Override != override || !now().Before(pf.ExpiresAt) {
		return pendingFile{}, pendingStale, nil
	}
	if !reRole.MatchString(pf.Role) || !reTool.MatchString(pf.Tool) || pf.Interval < 1 {
		return pendingFile{}, pendingAbsent, bad("role, tool or interval breaks its rule")
	}
	_, rpol, err := pol.check("request_url", pf.RequestURL)
	if err != nil {
		return pendingFile{}, pendingAbsent, bad(strings.TrimPrefix(err.Error(), "key request: "))
	}
	if why := checkRequest(rpol, pf.RequestID, pf.PollURL, pf.PollSecret, pf.ApproveURL, pf.UserCode); why != "" {
		return pendingFile{}, pendingAbsent, bad(why)
	}
	pf.Interval = min(pf.Interval, maxSeconds)
	if limit := now().Add(maxExpiresIn * unit); pf.ExpiresAt.After(limit) {
		pf.ExpiresAt = limit
	}
	return pf, pendingCurrent, nil
}

// loadPending reads the pending file under the lock. resumed is false when
// there is none, or when it is foreign or expired: then it is removed (T-12).
func loadPending(path, serviceURL, override string, pol policy, unit time.Duration) (pf pendingFile, resumed bool, err error) {
	pf, state, err := parsePending(path, serviceURL, override, pol, unit)
	if state == pendingStale {
		os.Remove(path)
	}
	return pf, state == pendingCurrent, err
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
