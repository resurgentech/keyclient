package keyclienttest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func get(t *testing.T, l *List, hdr map[string]string) reply {
	t.Helper()
	req, _ := http.NewRequest("GET", l.URL(), nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return reply{resp.StatusCode, resp.Header, sb.String()}
}

func TestListEmptyInitially(t *testing.T) {
	l := NewList(t)
	if !strings.HasSuffix(l.URL(), "/v1/lists/example") {
		t.Fatal(l.URL())
	}
	r := get(t, l, nil)
	if r.status != 200 || r.body != `{"keys":[]}` || r.header.Get("Content-Type") != "application/json" {
		t.Fatalf("%d %q %v", r.status, r.body, r.header)
	}
	sum := sha256.Sum256([]byte(r.body))
	if r.header.Get("ETag") != `"`+hex.EncodeToString(sum[:])[:32]+`"` {
		t.Fatalf("etag %q", r.header.Get("ETag"))
	}
}

func TestListETagAnd304(t *testing.T) {
	l := NewList(t)
	l.Set(Entry{"b", "agent", "h2"}, Entry{"a", "full", "h1"})
	r := get(t, l, nil)
	if r.body != `{"keys":[{"name":"a","role":"full","sha256":"h1"},{"name":"b","role":"agent","sha256":"h2"}]}` {
		t.Fatalf("%s", r.body)
	}
	etag := r.header.Get("ETag")
	n := get(t, l, map[string]string{"If-None-Match": etag})
	if n.status != 304 || n.body != "" || n.header.Get("ETag") != etag {
		t.Fatalf("%d %q %v", n.status, n.body, n.header)
	}
	if o := get(t, l, map[string]string{"If-None-Match": `"other"`}); o.status != 200 {
		t.Fatal(o.status)
	}
	l.Remove("a")
	r2 := get(t, l, map[string]string{"If-None-Match": etag})
	if r2.status != 200 || r2.header.Get("ETag") == etag {
		t.Fatalf("%d %v", r2.status, r2.header)
	}
	// The same entries give the same ETag.
	l.Set(Entry{"b", "agent", "h2"})
	if get(t, l, nil).header.Get("ETag") != r2.header.Get("ETag") {
		t.Fatal("ETag is not deterministic")
	}
}

func TestListFixtureVerbatim(t *testing.T) {
	l := NewList(t)
	l.LoadFixture("full")
	r := get(t, l, nil)
	if r.status != 200 || r.header.Get("ETag") != `"725d544c16cbd3c19288ac55547e4f2b"` {
		t.Fatalf("%d %v", r.status, r.header)
	}
	var v struct{ Keys []Entry }
	if err := json.Unmarshal([]byte(r.body), &v); err != nil || len(v.Keys) != 4 || v.Keys[0].Name != "alice@laptop" ||
		v.Keys[0].SHA256 != "097628f28c9143508f639c6a239bd2d6f4cc362062345c6cf7f8c1132cb12fd1" {
		t.Fatalf("%v %s", err, r.body)
	}
	if n := get(t, l, map[string]string{"If-None-Match": `"725d544c16cbd3c19288ac55547e4f2b"`}); n.status != 304 {
		t.Fatal(n.status)
	}
	l.LoadFixture("empty")
	r = get(t, l, nil)
	if r.header.Get("ETag") != `"9638d11914735d0072002dd4d67c734a"` || r.body != `{"keys":[]}` {
		t.Fatalf("%v %s", r.header, r.body)
	}
	// The fixture key is the one the provider delivers.
	p := NewProvider(t)
	l.LoadFixture("full")
	l.Add("alice@laptop", "agent", p.Key())
	if r := get(t, l, nil); !strings.Contains(r.body, "097628f28c9143508f639c6a239bd2d6f4cc362062345c6cf7f8c1132cb12fd1") {
		t.Fatal(r.body)
	}
}

func TestListAddHashesAndReplaces(t *testing.T) {
	l := NewList(t)
	l.Add("zed", "agent", "key-one")
	l.Add("amy", "full", "key-two")
	l.Add("zed", "full", "key-three")
	var v struct{ Keys []Entry }
	if err := json.Unmarshal([]byte(get(t, l, nil).body), &v); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("key-three"))
	if len(v.Keys) != 2 || v.Keys[0].Name != "amy" || v.Keys[1] != (Entry{"zed", "full", hex.EncodeToString(sum[:])}) {
		t.Fatalf("%+v", v.Keys)
	}
	l.Remove("nobody") // no-op
	l.Remove("amy")
	l.Remove("zed")
	if r := get(t, l, nil); r.body != `{"keys":[]}` {
		t.Fatal(r.body)
	}
}

func TestListRequireToken(t *testing.T) {
	l := NewList(t)
	l.RequireToken("s3cret")
	for _, h := range []map[string]string{nil, {"Authorization": "Bearer wrong"}, {"Authorization": "s3cret"}, {"Authorization": "bearer s3cret"}} {
		r := get(t, l, h)
		if r.status != 401 || jsonField(t, r.body, "error") != "unauthorized" {
			t.Fatalf("%v: %d %s", h, r.status, r.body)
		}
	}
	if r := get(t, l, map[string]string{"Authorization": "Bearer s3cret"}); r.status != 200 {
		t.Fatal(r.status)
	}
	l.RequireToken("")
	if r := get(t, l, nil); r.status != 200 {
		t.Fatal(r.status)
	}
}

func TestListFailWithAndRecover(t *testing.T) {
	l := NewList(t)
	l.Add("a", "agent", "k")
	for _, o := range []Outcome{
		Status(500, ""),
		Status(404, `{"error":"not_found","message":"x"}`),
		Status(200, "not json"),
	} {
		l.FailWith(o)
		r := get(t, l, nil)
		if r.status != o.status || r.body != o.body {
			t.Fatalf("%d %q", r.status, r.body)
		}
	}
	l.RequireToken("t") // FailWith wins
	if r := get(t, l, nil); r.status != 200 || r.body != "not json" {
		t.Fatal(r.status)
	}
	l.Recover()
	if r := get(t, l, map[string]string{"Authorization": "Bearer t"}); r.status != 200 || !strings.Contains(r.body, `"a"`) {
		t.Fatalf("%d %s", r.status, r.body)
	}
}

func TestListBlockAndRelease(t *testing.T) {
	l := NewList(t)
	l.Add("a", "agent", "k")
	release := l.Block()
	var wg sync.WaitGroup
	results := make(chan int, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- get(t, l, nil).status
		}()
	}
	deadline := time.Now().Add(5 * time.Second)
	for l.Pulls() < 4 {
		if time.Now().After(deadline) {
			t.Fatal("pulls did not arrive")
		}
		time.Sleep(5 * time.Millisecond)
	}
	select {
	case <-results:
		t.Fatal("a pull was answered while blocked")
	case <-time.After(50 * time.Millisecond):
	}
	release()
	release() // idempotent
	wg.Wait()
	close(results)
	for s := range results {
		if s != 200 {
			t.Fatal(s)
		}
	}
	if r := get(t, l, nil); r.status != 200 { // not blocked any more
		t.Fatal(r.status)
	}
}

func TestListBlockClientGivesUp(t *testing.T) {
	l := NewList(t)
	l.Block() // released by t.Cleanup
	c := &http.Client{Timeout: 100 * time.Millisecond}
	if _, err := c.Get(l.URL()); err == nil {
		t.Fatal("expected a timeout")
	}
	if l.Pulls() != 1 {
		t.Fatal(l.Pulls())
	}
}

func TestListPullsAndRequests(t *testing.T) {
	l := NewList(t)
	l.RequireToken("t")
	get(t, l, nil)
	get(t, l, map[string]string{"Authorization": "Bearer t", "If-None-Match": `"e"`})
	if l.Pulls() != 2 {
		t.Fatal(l.Pulls())
	}
	rs := l.Requests()
	if len(rs) != 2 || rs[0].Authorization != "" || rs[0].IfNoneMatch != "" ||
		rs[1].Authorization != "Bearer t" || rs[1].IfNoneMatch != `"e"` || rs[0].Time.IsZero() {
		t.Fatalf("%+v", rs)
	}
}

func TestListConcurrentMutationAndPull(t *testing.T) {
	l := NewList(t)
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); l.Add("n", "agent", "k"); l.Remove("n"); l.Pulls() }()
		go func() { defer wg.Done(); get(t, l, nil) }()
	}
	wg.Wait()
}
