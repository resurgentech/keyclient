package keyclienttest

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"time"

	"github.com/resurgentech/keyclient"
)

type kind int

const (
	kindRaw kind = iota // a status with a fixed body
	kindCreated
	kindPending
	kindSlowDown
	kindApproved
)

// Outcome is one scripted reply. The zero value is not useful; use the
// constructors.
type Outcome struct {
	kind     kind
	status   int
	body     string
	header   http.Header
	delay    time.Duration
	override map[string]any

	name, role, key string // kindApproved
}

// example is a fixture: an OpenAPI Example Object (testdata/README.md).
type example struct {
	Status  int               `json:"x-status"`
	Headers map[string]string `json:"x-headers"`
	Value   json.RawMessage   `json:"value"`
}

func readFixture(path string) []byte {
	b, err := fs.ReadFile(keyclient.Fixtures(), path)
	if err != nil {
		panic("keyclienttest: missing fixture " + path + ": " + err.Error())
	}
	return b
}

func loadExample(path string) example {
	var ex example
	if err := json.Unmarshal(readFixture(path), &ex); err != nil {
		panic("keyclienttest: bad fixture " + path + ": " + err.Error())
	}
	return ex
}

// fixtureOutcome is a raw outcome with the status and body of a keyrequest fixture.
func fixtureOutcome(name string) Outcome {
	ex := loadExample("keyrequest/" + name + ".json")
	var buf bytes.Buffer
	if err := json.Compact(&buf, ex.Value); err != nil {
		panic("keyclienttest: bad fixture keyrequest/" + name + ".json: " + err.Error())
	}
	return Outcome{kind: kindRaw, status: ex.Status, body: buf.String()}
}

// Created is the default 201 for [Provider.FileWith].
func Created() Outcome { return Outcome{kind: kindCreated, status: http.StatusCreated} }

// Pending is a 202 with the body of poll-pending.json; its interval is the
// request's current interval.
func Pending() Outcome { return Outcome{kind: kindPending, status: http.StatusAccepted} }

// SlowDown is a 429 with the body of poll-slow-down.json. The request's
// interval becomes the current one plus 2 and the body carries the new value
// (with the default interval of 1 it says 3).
func SlowDown() Outcome { return Outcome{kind: kindSlowDown, status: http.StatusTooManyRequests} }

// Approved is the 200 of poll-approved.json: name alice@laptop, role agent and
// the fixture key (see [Provider.Key]).
func Approved() Outcome {
	var v struct{ Name, Role, Key string }
	ex := loadExample("keyrequest/poll-approved.json")
	var raw map[string]string
	if err := json.Unmarshal(ex.Value, &raw); err != nil {
		panic("keyclienttest: bad fixture keyrequest/poll-approved.json: " + err.Error())
	}
	v.Name, v.Role, v.Key = raw["name"], raw["role"], raw["key"]
	return ApprovedWith(v.Name, v.Role, v.Key)
}

// ApprovedWith is an approved 200 with these values, unvalidated, so a test
// can send a bad key.
func ApprovedWith(name, role, key string) Outcome {
	return Outcome{kind: kindApproved, status: http.StatusOK, name: name, role: role, key: key}
}

// Denied is the 403 of poll-denied.json.
func Denied() Outcome { return fixtureOutcome("poll-denied") }

// Expired is the 410 of poll-expired.json.
func Expired() Outcome { return fixtureOutcome("poll-expired") }

// Consumed is the 410 of poll-consumed.json.
func Consumed() Outcome { return fixtureOutcome("poll-consumed") }

// Unauthorized is the 401 of poll-unauthorized.json, scripted: it is sent even
// when the poll presents the right secret.
func Unauthorized() Outcome { return fixtureOutcome("poll-unauthorized") }

// Status is any status with this raw body ("" for none). Use it for 5xx, odd
// statuses, unreadable bodies and errors from filing. A body is sent with
// Content-Type application/json unless [Outcome.WithHeader] sets another.
func Status(code int, body string) Outcome {
	return Outcome{kind: kindRaw, status: code, body: body}
}

// WithHeader returns o with a header added to its reply, for example
// Retry-After on a 429 from filing. It replaces the default value of that
// header.
func (o Outcome) WithHeader(key, value string) Outcome {
	h := make(http.Header, len(o.header)+1)
	for k, v := range o.header {
		h[k] = v
	}
	h.Set(key, value)
	o.header = h
	return o
}

// After returns o with a delay: the server waits d before answering, and gives
// up without answering if the request's context ends first.
func (o Outcome) After(d time.Duration) Outcome {
	o.delay = d
	return o
}

// Scenario returns the poll outcomes of the named scenario of
// testdata/keyrequest/scenarios.json ("approved", "denied", "expired",
// "unauthorized", "lost-reply"). Each step is mapped to the constructor of its
// fixture, and the step's "override" is applied to the body of the reply. It
// panics on an unknown name.
//
// The "approved" scenario has a pending step that overrides interval with 5.
// It is applied as written, and as the contract says the current interval is
// that of the latest response that carried one, the provider's interval for
// the request becomes 5 too. The wait_seconds of the fixture are not applied.
func Scenario(name string) []Outcome {
	var f struct {
		Scenarios []struct {
			Name  string `json:"name"`
			Steps []struct {
				Fixture  string         `json:"fixture"`
				Override map[string]any `json:"override"`
			} `json:"steps"`
		} `json:"scenarios"`
	}
	if err := json.Unmarshal(readFixture("keyrequest/scenarios.json"), &f); err != nil {
		panic("keyclienttest: bad fixture keyrequest/scenarios.json: " + err.Error())
	}
	for _, s := range f.Scenarios {
		if s.Name != name {
			continue
		}
		var out []Outcome
		for _, st := range s.Steps {
			var o Outcome
			switch st.Fixture {
			case "poll-pending":
				o = Pending()
			case "poll-slow-down":
				o = SlowDown()
			case "poll-approved":
				o = Approved()
			case "poll-denied":
				o = Denied()
			case "poll-expired":
				o = Expired()
			case "poll-consumed":
				o = Consumed()
			case "poll-unauthorized":
				o = Unauthorized()
			default:
				panic("keyclienttest: scenario " + name + " names unknown fixture " + st.Fixture)
			}
			o.override = st.Override
			out = append(out, o)
		}
		return out
	}
	panic("keyclienttest: unknown scenario " + name)
}

// reqState is what the provider remembers of one filed request.
type reqState struct {
	secret   string
	interval int
	approved bool // an approved reply has been served
}

type rendered struct {
	status int
	header http.Header
	body   []byte
	delay  time.Duration
}

// render builds the reply for o. st may be nil (replies of the list, or of a
// provider before a request exists); then the interval is def. The caller holds
// the lock that guards st.
func render(o Outcome, st *reqState, def int, base http.Header) rendered {
	if st == nil {
		st = &reqState{interval: def}
	}
	var body any
	status := o.status
	switch o.kind {
	case kindPending:
		body = map[string]any{"status": "pending", "interval": st.interval}
	case kindSlowDown:
		st.interval += 2
		body = map[string]any{"status": "slow_down", "interval": st.interval}
	case kindApproved:
		body = map[string]any{"status": "approved", "name": o.name, "role": o.role, "key": o.key}
		st.approved = true
	case kindCreated:
		status = http.StatusCreated
		body = json.RawMessage(loadExample("keyrequest/created.json").Value)
	}
	var b []byte
	if body != nil {
		var err error
		if b, err = json.Marshal(body); err != nil {
			panic(err)
		}
	} else if o.body != "" {
		b = []byte(o.body)
	}
	if len(o.override) > 0 {
		m := map[string]any{}
		if err := json.Unmarshal(b, &m); err != nil {
			panic("keyclienttest: override on a body that is not an object")
		}
		for k, v := range o.override {
			m[k] = v
		}
		if iv, ok := m["interval"].(float64); ok && (o.kind == kindPending || o.kind == kindSlowDown) {
			st.interval = int(iv)
		}
		b, _ = json.Marshal(m)
	}
	h := http.Header{}
	for k, v := range base {
		h[k] = v
	}
	if len(b) > 0 {
		h.Set("Content-Type", "application/json")
	}
	for k, v := range o.header {
		h[k] = v
	}
	return rendered{status: status, header: h, body: b, delay: o.delay}
}

// write sends r, after its delay. It reports false if the request's context
// ended during the delay (nothing is written then).
func (r rendered) write(w http.ResponseWriter, req *http.Request) bool {
	if r.delay > 0 {
		if !sleep(req.Context(), r.delay) {
			return false
		}
	}
	for k, v := range r.header {
		w.Header()[k] = v
	}
	w.WriteHeader(r.status)
	if len(r.body) > 0 && r.status != http.StatusNotModified && r.status != http.StatusNoContent {
		w.Write(r.body)
	}
	return true
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
