package keyclienttest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"
	"time"
)

// Entry is one entry of the key list.
type Entry struct {
	Name   string `json:"name"`
	Role   string `json:"role"`
	SHA256 string `json:"sha256"`
}

// ListRequest is one pull the list received.
type ListRequest struct {
	Time          time.Time
	IfNoneMatch   string
	Authorization string // the raw header value, so tests can assert what was sent
}

// List is a fake key-list endpoint. Create it with [NewList].
type List struct {
	srv *httptest.Server
	t   testing.TB

	mu       sync.Mutex
	entries  []Entry // sorted by name
	etag     string  // set by LoadFixture; empty means derive from the body
	fail     *Outcome
	token    string
	block    chan struct{}
	requests []ListRequest
}

type listBody struct {
	Keys []Entry `json:"keys"`
}

// NewList starts a fake list endpoint, closed in t.Cleanup, at [List.URL] (path
// /v1/lists/example), initially an empty list. GET answers 200 with
// {"keys":[...]} sorted by name, content type application/json and an ETag; an
// If-None-Match equal to the current ETag is answered 304 with the ETag and no
// body.
//
// The body is serialised deterministically, and an empty list is {"keys":[]}.
// After [List.Set], [List.Add] or [List.Remove] the ETag is a quote, the first
// 32 lowercase hex characters of the SHA-256 of the exact body served, and a
// quote. After [List.LoadFixture] it is the fixture's ETag, verbatim.
func NewList(t testing.TB) *List {
	t.Helper()
	l := &List{t: t}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/lists/example", l.handle)
	// Set before the server starts, as in NewProvider.
	l.srv = httptest.NewUnstartedServer(mux)
	l.srv.Start()
	// Cleanups run last in, first out: a Block is released before the server
	// closes, which would otherwise wait for the held pulls.
	t.Cleanup(l.srv.Close)
	return l
}

// URL is the URL of the list.
func (l *List) URL() string { return l.srv.URL + "/v1/lists/example" }

// Set replaces the list.
func (l *List) Set(entries ...Entry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = sortedCopy(entries)
	l.etag = ""
}

// Add adds an entry, or replaces the one of that name, with the lowercase hex
// SHA-256 of key.
func (l *List) Add(name, role, key string) {
	sum := sha256.Sum256([]byte(key))
	e := Entry{Name: name, Role: role, SHA256: hex.EncodeToString(sum[:])}
	l.mu.Lock()
	defer l.mu.Unlock()
	next := make([]Entry, 0, len(l.entries)+1)
	for _, o := range l.entries {
		if o.Name != name {
			next = append(next, o)
		}
	}
	l.entries = sortedCopy(append(next, e))
	l.etag = ""
}

// Remove removes the entry of that name, if any.
func (l *List) Remove(name string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	next := make([]Entry, 0, len(l.entries))
	for _, o := range l.entries {
		if o.Name != name {
			next = append(next, o)
		}
	}
	l.entries = next
	l.etag = ""
}

// LoadFixture replaces the list with testdata/keylist/<name>.json ("full" or
// "empty"): its entries and its ETag header, verbatim. The entries are served
// re-encoded, in the order of the fixture, which is sorted.
func (l *List) LoadFixture(name string) {
	ex := loadExample("keylist/" + name + ".json")
	var v listBody
	if err := json.Unmarshal(ex.Value, &v); err != nil {
		panic("keyclienttest: bad fixture keylist/" + name + ".json: " + err.Error())
	}
	etag := ex.Headers["ETag"]
	if etag == "" {
		panic("keyclienttest: fixture keylist/" + name + ".json has no ETag")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = v.Keys
	if l.entries == nil {
		l.entries = []Entry{}
	}
	l.etag = etag
}

// FailWith makes every later pull answer o (for example Status(500, ""),
// Status(404, `{"error":"not_found","message":"x"}`) or Status(200, "not
// json")) until [List.Recover]. It takes precedence over [List.RequireToken].
func (l *List) FailWith(o Outcome) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.fail = &o
}

// Recover goes back to serving the list.
func (l *List) Recover() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.fail = nil
}

// RequireToken makes pulls without exactly "Authorization: Bearer <token>"
// answer 401 {"error":"unauthorized",...}. An empty token turns the check off,
// which is the default.
func (l *List) RequireToken(token string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.token = token
}

// Block holds the pulls that arrive after it, until the returned release is
// called or the client gives up. release is idempotent, and t.Cleanup (of the
// test that made the list) calls it. Pulls are recorded when they arrive, so
// [List.Pulls] counts the held ones.
func (l *List) Block() (release func()) {
	ch := make(chan struct{})
	l.mu.Lock()
	l.block = ch
	l.mu.Unlock()
	var once sync.Once
	release = func() {
		once.Do(func() {
			l.mu.Lock()
			if l.block == ch {
				l.block = nil
			}
			l.mu.Unlock()
			close(ch)
		})
	}
	l.t.Cleanup(release)
	return release
}

// Pulls is the number of pulls received, with every status.
func (l *List) Pulls() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.requests)
}

// Requests returns every pull, in order.
func (l *List) Requests() []ListRequest {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]ListRequest(nil), l.requests...)
}

func sortedCopy(in []Entry) []Entry {
	out := make([]Entry, len(in))
	copy(out, in)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (l *List) handle(w http.ResponseWriter, r *http.Request) {
	l.mu.Lock()
	l.requests = append(l.requests, ListRequest{
		Time:          time.Now(),
		IfNoneMatch:   r.Header.Get("If-None-Match"),
		Authorization: r.Header.Get("Authorization"),
	})
	block := l.block
	l.mu.Unlock()

	if block != nil {
		select {
		case <-block:
		case <-r.Context().Done():
			return
		}
	}

	l.mu.Lock()
	if l.fail != nil {
		rd := render(*l.fail, nil, 1, nil)
		l.mu.Unlock()
		rd.write(w, r)
		return
	}
	if l.token != "" && r.Header.Get("Authorization") != "Bearer "+l.token {
		l.mu.Unlock()
		b, _ := json.Marshal(map[string]string{"error": "unauthorized", "message": "unauthorized"})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write(b)
		return
	}
	keys := l.entries
	if keys == nil {
		keys = []Entry{}
	}
	body, err := json.Marshal(listBody{Keys: keys})
	if err != nil {
		l.mu.Unlock()
		panic(err)
	}
	etag := l.etag
	if etag == "" {
		sum := sha256.Sum256(body)
		etag = `"` + hex.EncodeToString(sum[:])[:32] + `"`
	}
	l.mu.Unlock()

	w.Header().Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(body)
}
