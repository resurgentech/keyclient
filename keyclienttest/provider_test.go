package keyclienttest

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

type reply struct {
	status int
	header http.Header
	body   string
}

func do(t *testing.T, method, url, auth, body string) reply {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return reply{resp.StatusCode, resp.Header, string(b)}
}

const goodFiling = `{"name_hint":"alice@laptop","host":"laptop","tool":"mytool/0.3.0","role":"agent"}`

type created struct {
	RequestID  string `json:"request_id"`
	PollURL    string `json:"poll_url"`
	PollSecret string `json:"poll_secret"`
	ApproveURL string `json:"approve_url"`
	UserCode   string `json:"user_code"`
	Interval   int    `json:"interval"`
	ExpiresIn  int    `json:"expires_in"`
}

func file(t *testing.T, p *Provider) created {
	t.Helper()
	r := do(t, "POST", p.RequestURL(), "", goodFiling)
	if r.status != 201 {
		t.Fatalf("filing: %d %s", r.status, r.body)
	}
	var c created
	if err := json.Unmarshal([]byte(r.body), &c); err != nil {
		t.Fatal(err)
	}
	return c
}

func poll(t *testing.T, c created, secret string) reply {
	t.Helper()
	return do(t, "POST", c.PollURL, "Bearer "+secret, "")
}

func jsonField(t *testing.T, body, field string) any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("body %q: %v", body, err)
	}
	return m[field]
}

func TestDiscovery(t *testing.T) {
	p := NewProvider(t)
	r := do(t, "GET", p.URL()+"/v1/key-request", "", "")
	if r.status != 200 || jsonField(t, r.body, "request_url") != p.RequestURL() {
		t.Fatalf("%d %s", r.status, r.body)
	}
	if !strings.HasSuffix(p.RequestURL(), "/v1/services/example/requests") || !strings.HasPrefix(p.RequestURL(), p.URL()) {
		t.Fatal(p.RequestURL())
	}
	p.SetDiscovery(false)
	if r := do(t, "GET", p.URL()+"/v1/key-request", "", ""); r.status != 404 {
		t.Fatalf("%d", r.status)
	}
	p.SetDiscovery(true)
	if r := do(t, "GET", p.URL()+"/v1/key-request", "", ""); r.status != 200 {
		t.Fatalf("%d", r.status)
	}
}

func TestFilingValidation(t *testing.T) {
	p := NewProvider(t)
	pad := func(n int) string { // a valid JSON body of exactly n bytes
		base := `{"name_hint":"a","host":"h","tool":"t","role":"r"}`
		return base + strings.Repeat(" ", n-len(base))
	}
	cases := []struct {
		name, body string
		status     int
		code       string
	}{
		{"2048 ok", pad(2048), 201, ""},
		{"2049 too large", pad(2049), 413, "too_large"},
		{"not json", `nope`, 400, "bad_request"},
		{"unknown field", `{"name_hint":"a","host":"h","tool":"t","role":"r","extra":"x"}`, 400, "bad_request"},
		{"missing field", `{"name_hint":"a","host":"h","tool":"t"}`, 400, "bad_request"},
		{"null field", `{"name_hint":"a","host":"h","tool":"t","role":null}`, 400, "bad_request"},
		{"host with @", `{"name_hint":"a","host":"h@x","tool":"t","role":"r"}`, 400, "bad_request"},
		{"name_hint empty", `{"name_hint":"","host":"h","tool":"t","role":"r"}`, 400, "bad_request"},
		{"role 33 long", `{"name_hint":"a","host":"h","tool":"t","role":"` + strings.Repeat("r", 33) + `"}`, 400, "bad_request"},
		{"tool with slash and plus", `{"name_hint":"a","host":"h","tool":"x/1.0+b","role":"r"}`, 201, ""},
		{"trailing newline in field", `{"name_hint":"a\n","host":"h","tool":"t","role":"r"}`, 400, "bad_request"},
	}
	want := 0
	for _, c := range cases {
		r := do(t, "POST", p.RequestURL(), "", c.body)
		if r.status != c.status {
			t.Errorf("%s: status %d, want %d (%s)", c.name, r.status, c.status, r.body)
			continue
		}
		if c.status == 201 {
			want++
			continue
		}
		if jsonField(t, r.body, "error") != c.code {
			t.Errorf("%s: body %s", c.name, r.body)
		}
		if r.header.Get("Cache-Control") != "no-store" || r.header.Get("Content-Type") != "application/json" {
			t.Errorf("%s: headers %v", c.name, r.header)
		}
	}
	if got := len(p.Filed()); got != want {
		t.Fatalf("Filed has %d, want %d (only valid filings are recorded)", got, want)
	}
}

func TestFirstCreatedIsFixtureThenDistinct(t *testing.T) {
	p := NewProvider(t)
	r := do(t, "POST", p.RequestURL(), "", goodFiling)
	if r.status != 201 || r.header.Get("Cache-Control") != "no-store" || r.header.Get("Content-Type") != "application/json" {
		t.Fatalf("%d %v", r.status, r.header)
	}
	var c1 created
	json.Unmarshal([]byte(r.body), &c1)
	if c1.RequestID != "fakeRequestId00000000x" || c1.PollSecret != "fake-poll-secret-0123456789-ABCDEFGHIJKLMNO" {
		t.Fatalf("%+v", c1)
	}
	if c1.PollURL != p.URL()+"/v1/requests/"+c1.RequestID+"/poll" {
		t.Fatal(c1.PollURL)
	}
	if c1.ApproveURL != p.URL()+"/requests?code="+c1.UserCode || c1.UserCode != "BCDF-GHJK" {
		t.Fatal(c1.ApproveURL)
	}
	if strings.Contains(c1.ApproveURL, c1.PollSecret) {
		t.Fatal("approve_url carries the poll secret")
	}
	if c1.Interval != 1 || c1.ExpiresIn != 600 {
		t.Fatalf("%+v", c1)
	}
	if p.PollSecret() != c1.PollSecret {
		t.Fatal("PollSecret")
	}
	p.SetInterval(7)
	p.SetExpiresIn(30)
	c2 := file(t, p)
	if c2.RequestID == c1.RequestID || c2.PollSecret == c1.PollSecret {
		t.Fatalf("not distinct: %+v", c2)
	}
	if len(c2.RequestID) != len(c1.RequestID) || len(c2.PollSecret) != len(c1.PollSecret) {
		t.Fatalf("lengths differ: %+v", c2)
	}
	if c2.Interval != 7 || c2.ExpiresIn != 30 || p.PollSecret() != c2.PollSecret {
		t.Fatalf("%+v", c2)
	}
	f := p.Filed()
	if len(f) != 2 || f[0].NameHint != "alice@laptop" || f[0].Host != "laptop" || f[0].Tool != "mytool/0.3.0" || f[0].Role != "agent" || f[0].Time.IsZero() {
		t.Fatalf("%+v", f)
	}
	if p.Key() != "example-key-0123456789abcdefghijklmnopqrstuvw" {
		t.Fatal(p.Key())
	}
}

func TestScriptOrderAndPollRecords(t *testing.T) {
	p := NewProvider(t)
	c := file(t, p)
	p.Script(Pending(), Approved(), Denied())
	var got []int
	for i := 0; i < 3; i++ {
		r := poll(t, c, c.PollSecret)
		got = append(got, r.status)
		if r.header.Get("Cache-Control") != "no-store" || r.header.Get("Content-Type") != "application/json" {
			t.Fatalf("headers %v", r.header)
		}
		if i == 1 {
			if jsonField(t, r.body, "key") != p.Key() || jsonField(t, r.body, "name") != "alice@laptop" || jsonField(t, r.body, "role") != "agent" {
				t.Fatalf("%s", r.body)
			}
		}
	}
	if got[0] != 202 || got[1] != 200 || got[2] != 403 {
		t.Fatalf("%v", got)
	}
	ps := p.Polls()
	if len(ps) != 3 || ps[0].RequestID != c.RequestID || !ps[0].Authorized || ps[0].Time.IsZero() {
		t.Fatalf("%+v", ps)
	}
}

func TestUnauthorizedPollDoesNotConsumeScript(t *testing.T) {
	p := NewProvider(t)
	c := file(t, p)
	p.Script(Approved())
	r := poll(t, c, "wrong")
	if r.status != 401 || jsonField(t, r.body, "error") != "unauthorized" {
		t.Fatalf("%d %s", r.status, r.body)
	}
	if r := do(t, "POST", c.PollURL, "", ""); r.status != 401 {
		t.Fatalf("no header: %d", r.status)
	}
	if r := do(t, "POST", p.URL()+"/v1/requests/unknown/poll", "Bearer "+c.PollSecret, ""); r.status != 401 {
		t.Fatalf("unknown id: %d", r.status)
	}
	if r := poll(t, c, c.PollSecret); r.status != 200 {
		t.Fatalf("script entry lost: %d", r.status)
	}
	ps := p.Polls()
	if len(ps) != 4 || ps[0].Authorized || ps[1].Authorized || ps[2].Authorized || !ps[3].Authorized {
		t.Fatalf("%+v", ps)
	}
	if ps[2].RequestID != "unknown" {
		t.Fatal(ps[2])
	}
}

func TestScriptedUnauthorizedWithRightSecret(t *testing.T) {
	p := NewProvider(t)
	c := file(t, p)
	p.Script(Unauthorized(), Pending())
	if r := poll(t, c, c.PollSecret); r.status != 401 {
		t.Fatal(r.status)
	}
	if r := poll(t, c, c.PollSecret); r.status != 202 {
		t.Fatal(r.status)
	}
}

func TestExhaustedScriptDefaults(t *testing.T) {
	p := NewProvider(t)
	c := file(t, p)
	for i := 0; i < 2; i++ {
		r := poll(t, c, c.PollSecret)
		if r.status != 202 || jsonField(t, r.body, "status") != "pending" || jsonField(t, r.body, "interval") != float64(1) {
			t.Fatalf("%d %s", r.status, r.body)
		}
	}
	p.Script(Approved())
	if r := poll(t, c, c.PollSecret); r.status != 200 {
		t.Fatal(r.status)
	}
	for i := 0; i < 2; i++ {
		r := poll(t, c, c.PollSecret)
		if r.status != 410 || jsonField(t, r.body, "status") != "consumed" {
			t.Fatalf("%d %s", r.status, r.body)
		}
	}
	// A script entry wins over the "already approved" default.
	p.Script(Pending())
	if r := poll(t, c, c.PollSecret); r.status != 202 {
		t.Fatal(r.status)
	}
	// Another request is not affected.
	c2 := file(t, p)
	if r := poll(t, c2, c2.PollSecret); r.status != 202 {
		t.Fatal(r.status)
	}
}

func TestSlowDownRaisesInterval(t *testing.T) {
	p := NewProvider(t)
	c := file(t, p)
	p.Script(SlowDown(), Pending(), SlowDown())
	r := poll(t, c, c.PollSecret)
	if r.status != 429 || jsonField(t, r.body, "status") != "slow_down" || jsonField(t, r.body, "interval") != float64(3) {
		t.Fatalf("%d %s", r.status, r.body)
	}
	r = poll(t, c, c.PollSecret)
	if r.status != 202 || jsonField(t, r.body, "interval") != float64(3) {
		t.Fatalf("%d %s", r.status, r.body)
	}
	r = poll(t, c, c.PollSecret)
	if jsonField(t, r.body, "interval") != float64(5) {
		t.Fatalf("%s", r.body)
	}
	r = poll(t, c, c.PollSecret) // default pending carries the raised interval
	if jsonField(t, r.body, "interval") != float64(5) {
		t.Fatalf("%s", r.body)
	}
}

// P-6: the interval never exceeds ten times the interval the 201 gave.
func TestP6_SlowDownCappedAtTenTimesFirstInterval(t *testing.T) {
	p := NewProvider(t)
	p.SetInterval(2)
	c := file(t, p)
	var outs []Outcome
	for i := 0; i < 15; i++ {
		outs = append(outs, SlowDown())
	}
	p.Script(outs...)
	var last float64
	for i := 0; i < 15; i++ {
		r := poll(t, c, c.PollSecret)
		last = jsonField(t, r.body, "interval").(float64)
		if last > 20 {
			t.Fatalf("poll %d: interval %v, over ten times the first (2)", i, last)
		}
	}
	if last != 20 {
		t.Fatalf("interval settled at %v, want 20", last)
	}
}

// P-11: an approved reply is served on any authorised poll, early or not (the
// fake never paces), and a scenario with an early collecting poll says so.
func TestP11_ApprovedDeliveredOnEarlyPoll(t *testing.T) {
	p := NewProvider(t)
	c := file(t, p)
	p.Script(Scenario("approved-early-poll")...)
	var statuses []int
	for i := 0; i < 3; i++ {
		statuses = append(statuses, poll(t, c, c.PollSecret).status)
	}
	if statuses[0] != 202 || statuses[1] != 200 || statuses[2] != 410 {
		t.Fatalf("statuses %v, want 202 200 410", statuses)
	}
}

func TestFinalOutcomes(t *testing.T) {
	p := NewProvider(t)
	c := file(t, p)
	p.Script(Denied(), Expired(), Consumed(), Status(503, ""), Status(200, "not json"), Status(418, `{"x":1}`))
	want := []struct {
		status int
		body   string
	}{
		{403, `{"status":"denied"}`},
		{410, `{"status":"expired"}`},
		{410, `{"status":"consumed"}`},
		{503, ""},
		{200, "not json"},
		{418, `{"x":1}`},
	}
	for i, w := range want {
		r := poll(t, c, c.PollSecret)
		if r.status != w.status || r.body != w.body {
			t.Errorf("%d: %d %q", i, r.status, r.body)
		}
	}
}

func TestApprovedWithIsUnvalidated(t *testing.T) {
	p := NewProvider(t)
	c := file(t, p)
	p.Script(ApprovedWith("n", "r", "short key"))
	r := poll(t, c, c.PollSecret)
	if r.status != 200 || jsonField(t, r.body, "key") != "short key" || jsonField(t, r.body, "name") != "n" {
		t.Fatalf("%s", r.body)
	}
}

func TestFileWith(t *testing.T) {
	p := NewProvider(t)
	p.FileWith(Status(429, `{"error":"rate_limited","message":"slow"}`).WithHeader("Retry-After", "30"))
	r := do(t, "POST", p.RequestURL(), "", goodFiling)
	if r.status != 429 || r.header.Get("Retry-After") != "30" || r.header.Get("Cache-Control") != "no-store" ||
		r.header.Get("Content-Type") != "application/json" || jsonField(t, r.body, "error") != "rate_limited" {
		t.Fatalf("%d %v %s", r.status, r.header, r.body)
	}
	// A bad body is still a 400, before FileWith.
	if r := do(t, "POST", p.RequestURL(), "", "x"); r.status != 400 {
		t.Fatal(r.status)
	}
	p.FileWith(Created())
	c := file(t, p)
	if c.RequestID != "fakeRequestId00000000x" {
		t.Fatalf("the failed filing made a request: %+v", c)
	}
	if len(p.Filed()) != 2 {
		t.Fatalf("%d filings recorded", len(p.Filed()))
	}
}

func TestScenarioApproved(t *testing.T) {
	p := NewProvider(t)
	c := file(t, p)
	p.Script(Scenario("approved")...)
	type step struct {
		status   int
		field    string
		want     any
		wantBody string
	}
	steps := []step{
		{202, "interval", float64(1), ""},
		{429, "interval", float64(3), ""},
		{202, "interval", float64(5), ""}, // the fixture's override, applied as written
		{200, "key", p.Key(), ""},
		{410, "status", "consumed", ""},
	}
	for i, s := range steps {
		r := poll(t, c, c.PollSecret)
		if r.status != s.status || jsonField(t, r.body, s.field) != s.want {
			t.Fatalf("step %d: %d %s", i, r.status, r.body)
		}
	}
	if len(p.Polls()) != 5 {
		t.Fatal(len(p.Polls()))
	}
}

func TestScenarioOverrideKeepsIntervalConsistent(t *testing.T) {
	p := NewProvider(t)
	c := file(t, p)
	p.Script(Scenario("approved")[:3]...)
	for i := 0; i < 3; i++ {
		poll(t, c, c.PollSecret)
	}
	// After the override, the current interval is 5.
	r := poll(t, c, c.PollSecret)
	if jsonField(t, r.body, "interval") != float64(5) {
		t.Fatalf("%s", r.body)
	}
}

func TestScenarios(t *testing.T) {
	want := map[string][]int{
		"approved":     {202, 429, 202, 200, 410},
		"denied":       {202, 403},
		"expired":      {202, 410},
		"unauthorized": {401},
		"lost-reply":   {410},
	}
	for name, codes := range want {
		p := NewProvider(t)
		c := file(t, p)
		p.Script(Scenario(name)...)
		for i, code := range codes {
			if r := poll(t, c, c.PollSecret); r.status != code {
				t.Errorf("%s step %d: %d, want %d", name, i, r.status, code)
			}
		}
	}
	defer func() {
		if recover() == nil {
			t.Fatal("unknown scenario did not panic")
		}
	}()
	Scenario("nope")
}

func TestAfterDelaysAndHonoursCancellation(t *testing.T) {
	p := NewProvider(t)
	c := file(t, p)
	p.Script(Pending().After(150*time.Millisecond), Pending().After(time.Minute), Approved())
	start := time.Now()
	if r := poll(t, c, c.PollSecret); r.status != 202 || time.Since(start) < 150*time.Millisecond {
		t.Fatalf("%d after %v", r.status, time.Since(start))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", c.PollURL, nil)
	req.Header.Set("Authorization", "Bearer "+c.PollSecret)
	start = time.Now()
	if resp, err := http.DefaultClient.Do(req); err == nil {
		resp.Body.Close()
		t.Fatal("expected the client to give up")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("slow")
	}
	// The server handler returned, so the next poll is served at once.
	if r := poll(t, c, c.PollSecret); r.status != 200 {
		t.Fatal(r.status)
	}
}

func TestProviderConcurrent(t *testing.T) {
	p := NewProvider(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := file(t, p)
			for j := 0; j < 5; j++ {
				poll(t, c, c.PollSecret)
				p.Polls()
				p.Filed()
				p.PollSecret()
			}
		}()
	}
	wg.Wait()
	if len(p.Filed()) != 8 || len(p.Polls()) != 40 {
		t.Fatal(len(p.Filed()), len(p.Polls()))
	}
}
