package keyclienttest

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sync"
	"testing"
	"time"
)

// Provider is a fake key provider that also plays the service's discovery
// endpoint. Create it with [NewProvider].
type Provider struct {
	srv *httptest.Server

	mu        sync.Mutex
	discovery bool
	interval  int
	expiresIn int
	script    []Outcome
	fileWith  *Outcome
	filed     []Filed
	polls     []Poll
	requests  map[string]*reqState
	count     int // 201s handed out
	lastSec   string

	created createdFixture
	key     string
}

// Filed is one filing the provider received with a valid body.
type Filed struct {
	NameHint, Host, Tool, Role string
	Time                       time.Time
}

// Poll is one poll the provider received.
type Poll struct {
	Time       time.Time // when the poll arrived
	RequestID  string
	Authorized bool // the Bearer matched that request's poll secret
}

type createdFixture struct {
	RequestID  string `json:"request_id"`
	PollURL    string `json:"poll_url"`
	PollSecret string `json:"poll_secret"`
	ApproveURL string `json:"approve_url"`
	UserCode   string `json:"user_code"`
	Interval   int    `json:"interval"`
	ExpiresIn  int    `json:"expires_in"`
}

var (
	reNameHint = regexp.MustCompile(`^[A-Za-z0-9_.@-]{1,64}$`)
	reHost     = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
	reTool     = regexp.MustCompile(`^[A-Za-z0-9_.@/+-]{1,64}$`)
	reRole     = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,32}$`)
)

const maxFilingBody = 2048

// NewProvider starts a fake provider, closed in t.Cleanup. Its URL serves:
//
//	GET  /v1/key-request                discovery (the service's endpoint)
//	POST /v1/services/example/requests  filing
//	POST /v1/requests/{id}/poll         polling
//
// Every response to filing and polling carries Cache-Control: no-store, and
// Content-Type application/json when it has a body.
//
// Filing: a body over 2048 bytes is answered 413 too_large; a body that is not
// JSON, has an unknown or a missing field, or a field that breaks its pattern
// in docs/CONTRACT.md is answered 400 bad_request. Otherwise the reply is the
// one set by [Provider.FileWith], by default a 201 made from created.json: the
// first filing gets the fixture's request_id and poll_secret exactly, each
// later one distinct values of the same lengths; poll_url and approve_url are
// on this server; interval and expires_in come from [Provider.SetInterval] and
// [Provider.SetExpiresIn].
//
// Polling: a poll whose id is unknown, or whose "Authorization: Bearer" is not
// exactly that request's secret, is answered with poll-unauthorized.json and
// does not consume a script entry; it is recorded with Authorized false.
// Otherwise the next entry of the script answers. The script wins while it has
// entries. Only when it is exhausted does the provider decide: Consumed if an
// approved reply has already been served for that request, else Pending.
func NewProvider(t testing.TB) *Provider {
	t.Helper()
	p := &Provider{
		discovery: true,
		interval:  1,
		requests:  map[string]*reqState{},
	}
	ex := loadExample("keyrequest/created.json")
	if err := json.Unmarshal(ex.Value, &p.created); err != nil {
		panic("keyclienttest: bad fixture keyrequest/created.json: " + err.Error())
	}
	p.expiresIn = p.created.ExpiresIn
	p.key = approvedKey()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/key-request", p.handleDiscovery)
	mux.HandleFunc("POST /v1/services/example/requests", p.handleFile)
	mux.HandleFunc("POST /v1/requests/{id}/poll", p.handlePoll)
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	return p
}

func approvedKey() string {
	var v struct{ Key string }
	ex := loadExample("keyrequest/poll-approved.json")
	if err := json.Unmarshal(ex.Value, &v); err != nil {
		panic("keyclienttest: bad fixture keyrequest/poll-approved.json: " + err.Error())
	}
	return v.Key
}

// URL is the base URL of the provider; a tool's ServiceURL.
func (p *Provider) URL() string { return p.srv.URL }

// RequestURL is URL() + "/v1/services/example/requests".
func (p *Provider) RequestURL() string { return p.srv.URL + "/v1/services/example/requests" }

// SetDiscovery turns the discovery answer on (200 with the request URL, the
// default) or off (404).
func (p *Provider) SetDiscovery(on bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.discovery = on
}

// SetInterval sets the interval in each later 201 reply, and so the first
// current interval of each later request. The default is 1 second: the
// fixture's 3 would make tests slow.
func (p *Provider) SetInterval(seconds int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.interval = seconds
}

// SetExpiresIn sets expires_in in each later 201 reply. The default is 600,
// from created.json.
func (p *Provider) SetExpiresIn(seconds int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.expiresIn = seconds
}

// Script replaces the poll script: the replies to the next polls, in order.
func (p *Provider) Script(outcomes ...Outcome) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.script = append([]Outcome(nil), outcomes...)
}

// FileWith sets the reply to every later filing with a valid body, instead of
// the 201 (for example Status(429, ...)). FileWith(Created()) restores the
// default.
func (p *Provider) FileWith(o Outcome) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if o.kind == kindCreated && o.delay == 0 && len(o.header) == 0 {
		p.fileWith = nil
		return
	}
	p.fileWith = &o
}

// Filed returns every filing received with a valid body, in order.
func (p *Provider) Filed() []Filed {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]Filed(nil), p.filed...)
}

// Polls returns every poll received, in order.
func (p *Provider) Polls() []Poll {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]Poll(nil), p.polls...)
}

// PollSecret is the poll secret handed out by the latest 201. A test asserts
// that it never appears in what the tool prints or stores.
func (p *Provider) PollSecret() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastSec
}

// Key is the key the default [Approved] delivers: the fixture key.
func (p *Provider) Key() string { return p.key }

func (p *Provider) baseHeader() http.Header {
	return http.Header{"Cache-Control": {"no-store"}}
}

func (p *Provider) errReply(w http.ResponseWriter, status int, code, msg string) {
	b, _ := json.Marshal(map[string]string{"error": code, "message": msg})
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(b)
}

func (p *Provider) handleDiscovery(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	on := p.discovery
	p.mu.Unlock()
	if !on {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	b, _ := json.Marshal(map[string]string{"request_url": p.RequestURL()})
	w.Header().Set("Content-Type", "application/json")
	w.Write(b)
}

func (p *Provider) handleFile(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxFilingBody+1))
	if err != nil {
		p.errReply(w, http.StatusBadRequest, "bad_request", "unreadable body")
		return
	}
	if len(body) > maxFilingBody {
		p.errReply(w, http.StatusRequestEntityTooLarge, "too_large", "body over 2048 bytes")
		return
	}
	var in struct {
		NameHint *string `json:"name_hint"`
		Host     *string `json:"host"`
		Tool     *string `json:"tool"`
		Role     *string `json:"role"`
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil || dec.More() {
		p.errReply(w, http.StatusBadRequest, "bad_request", "body is not the expected JSON object")
		return
	}
	for _, f := range []struct {
		name string
		v    *string
		re   *regexp.Regexp
	}{
		{"name_hint", in.NameHint, reNameHint},
		{"host", in.Host, reHost},
		{"tool", in.Tool, reTool},
		{"role", in.Role, reRole},
	} {
		if f.v == nil {
			p.errReply(w, http.StatusBadRequest, "bad_request", "missing field "+f.name)
			return
		}
		if !f.re.MatchString(*f.v) {
			p.errReply(w, http.StatusBadRequest, "bad_request", "field "+f.name+" breaks its rule")
			return
		}
	}

	p.mu.Lock()
	p.filed = append(p.filed, Filed{NameHint: *in.NameHint, Host: *in.Host, Tool: *in.Tool, Role: *in.Role, Time: time.Now()})
	var rd rendered
	if p.fileWith != nil {
		rd = render(*p.fileWith, nil, p.interval, p.baseHeader())
	} else {
		rd = p.createLocked()
	}
	p.mu.Unlock()
	rd.write(w, r)
}

// createLocked makes a request and its 201 reply.
func (p *Provider) createLocked() rendered {
	p.count++
	c := p.created
	if p.count > 1 {
		n := fmt.Sprintf("%04d", p.count)
		c.RequestID = c.RequestID[:len(c.RequestID)-5] + n + c.RequestID[len(c.RequestID)-1:]
		c.PollSecret = c.PollSecret[:len(c.PollSecret)-4] + n
	}
	c.PollURL = p.srv.URL + "/v1/requests/" + c.RequestID + "/poll"
	c.ApproveURL = p.srv.URL + "/requests?code=" + c.UserCode
	c.Interval = p.interval
	c.ExpiresIn = p.expiresIn
	p.requests[c.RequestID] = &reqState{secret: c.PollSecret, interval: p.interval, first: p.interval}
	p.lastSec = c.PollSecret
	b, err := json.Marshal(c)
	if err != nil {
		panic(err)
	}
	h := p.baseHeader()
	h.Set("Content-Type", "application/json")
	return rendered{status: http.StatusCreated, header: h, body: b}
}

func (p *Provider) handlePoll(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	auth := r.Header.Get("Authorization")
	p.mu.Lock()
	st := p.requests[id]
	ok := st != nil && subtle.ConstantTimeCompare([]byte(auth), []byte("Bearer "+st.secret)) == 1
	p.polls = append(p.polls, Poll{Time: time.Now(), RequestID: id, Authorized: ok})
	var o Outcome
	switch {
	case !ok:
		o = Unauthorized()
		st = nil
	case len(p.script) > 0:
		o = p.script[0]
		p.script = p.script[1:]
	case st.approved:
		o = Consumed()
	default:
		o = Pending()
	}
	rd := render(o, st, p.interval, p.baseHeader())
	p.mu.Unlock()
	rd.write(w, r)
}
