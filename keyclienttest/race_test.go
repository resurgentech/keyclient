package keyclienttest

import (
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
)

// A request sent from another goroutine (or another process) right after
// NewProvider returns must not race with the provider's own set-up: the
// server's address is in place before the server can serve. Run under -race.
func TestNewProviderServesAnotherGoroutineAtOnce(t *testing.T) {
	for i := 0; i < 20; i++ {
		p := NewProvider(t)
		url := p.RequestURL()
		var wg sync.WaitGroup
		errs := make(chan string, 1)
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Post(url, "application/json", strings.NewReader(goodFiling))
			if err != nil {
				errs <- err.Error()
				return
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusCreated {
				errs <- resp.Status
			}
		}()
		wg.Wait()
		close(errs)
		for e := range errs {
			t.Fatalf("filing from another goroutine: %s", e)
		}
	}
}

const raceChildEnv = "KEYCLIENTTEST_RACE_CHILD_URL"

// TestRaceChild is the second process of TestNewProviderServesAnotherProcessAtOnce.
func TestRaceChild(t *testing.T) {
	url := os.Getenv(raceChildEnv)
	if url == "" {
		t.Skip("child only")
	}
	resp, err := http.Post(url, "application/json", strings.NewReader(goodFiling))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatal(resp.Status)
	}
}

// A real second process files right after NewProvider returns, as a tool under
// test in another process does. The race detector watches the provider's side.
func TestNewProviderServesAnotherProcessAtOnce(t *testing.T) {
	if os.Getenv(raceChildEnv) != "" {
		t.Skip("parent only")
	}
	for i := 0; i < 2; i++ {
		p := NewProvider(t)
		cmd := exec.Command(os.Args[0], "-test.run", "^TestRaceChild$", "-test.count=1")
		cmd.Env = append(os.Environ(), raceChildEnv+"="+p.RequestURL())
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("child: %v\n%s", err, out)
		}
		if len(p.Filed()) != 1 {
			t.Fatalf("filed %d, want 1", len(p.Filed()))
		}
	}
}

// The same for the list: a pull from another goroutine right after NewList.
func TestNewListServesAnotherGoroutineAtOnce(t *testing.T) {
	for i := 0; i < 20; i++ {
		l := NewList(t)
		url := l.URL()
		var wg sync.WaitGroup
		var status int
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Get(url)
			if err != nil {
				return
			}
			resp.Body.Close()
			status = resp.StatusCode
		}()
		wg.Wait()
		if status != http.StatusOK {
			t.Fatalf("pull from another goroutine: status %d", status)
		}
	}
}
