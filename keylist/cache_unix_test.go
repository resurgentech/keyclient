//go:build unix

package keylist_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/resurgentech/keyclient/keyclienttest"
	"github.com/resurgentech/keyclient/keylist"
)

// readOnlyDir makes dir read-only (cache writes fail) until the test ends.
func readOnlyDir(t *testing.T, dir string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
}

// TestFailedCacheWriteNeverRevivesRevokedKey (probe K1b): cache writes fail, a
// key is revoked, and a restart with the provider down must not accept it.
func TestFailedCacheWriteNeverRevivesRevokedKey(t *testing.T) {
	e := newEnv(t)
	e.add("alice@laptop", "agent", "key-alice")
	e.add("bob@desktop", "agent", "key-bob")
	s := e.source(e.opts())
	if err := s.Pull(context.Background()); err != nil { // the cache now holds both
		t.Fatal(err)
	}
	readOnlyDir(t, filepath.Dir(e.cache()))
	e.l.Remove("bob@desktop")
	err := s.Pull(context.Background())
	if !errors.Is(err, keylist.ErrCacheWrite) {
		t.Fatalf("Pull = %v, want ErrCacheWrite", err)
	}
	mustMiss(t, s, "key-bob")
	if st := s.Status(); !st.CacheFailing {
		t.Fatalf("Status = %+v", st)
	}
	if !strings.Contains(e.log.String(), "level=ERROR") {
		t.Fatalf("not logged at error level:\n%s", e.log.String())
	}

	e.l.FailWith(keyclienttest.Status(500, "")) // provider down at restart
	s2 := e.source(e.opts())
	e.begin(s2)
	mustMiss(t, s2, "key-bob")
	mustMiss(t, s2, "key-alice") // the older list is not loaded at all
}

// TestStaleCacheNotLoadedOverFresherList (probe K1): Pull before Start with a
// failing cache write, then Start with a stale cache on disk and a provider
// that answers 304 forever: the revoked key is not accepted.
func TestStaleCacheNotLoadedOverFresherList(t *testing.T) {
	e := newEnv(t)
	e.add("alice@laptop", "agent", "key-alice")
	e.secret("key-revoked")
	if err := os.MkdirAll(filepath.Dir(e.cache()), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, e.cache(), oneKeyList("revoked@host", "key-revoked"))
	readOnlyDir(t, filepath.Dir(e.cache()))

	o := e.opts()
	o.Refresh = 20 * time.Millisecond
	s := e.source(o)
	if err := s.Pull(context.Background()); !errors.Is(err, keylist.ErrCacheWrite) {
		t.Fatalf("Pull = %v, want ErrCacheWrite", err)
	}
	e.begin(s) // every timer pull is a 304 from here on
	mustMiss(t, s, "key-revoked")
	time.Sleep(200 * time.Millisecond)
	mustMiss(t, s, "key-revoked")
	mustFind(t, s, "key-alice", "alice@laptop", "agent")
}

// TestStartDoesNotLoadCacheOverPulledList is the first fix on its own: even
// with a cache that is there and readable, Start does not load it over a list
// already pulled in this process.
func TestStartDoesNotLoadCacheOverPulledList(t *testing.T) {
	e := newEnv(t)
	e.skipInvariants = true // the test itself replaces the cache with a stale body
	e.add("alice@laptop", "agent", "key-alice")
	e.secret("key-revoked")
	s := e.source(e.opts())
	if err := s.Pull(context.Background()); err != nil {
		t.Fatal(err)
	}
	writeFile(t, e.cache(), oneKeyList("revoked@host", "key-revoked")) // a stale cache appears
	e.begin(s)                                                         // Start loads the cache synchronously, or not at all
	mustMiss(t, s, "key-revoked")
	mustFind(t, s, "key-alice", "alice@laptop", "agent")
}

func oneKeyList(name, key string) string {
	return `{"keys":[{"name":"` + name + `","role":"agent","sha256":"` + hashOf(key) + `"}]}`
}

func TestCacheFileTrust(t *testing.T) {
	setup := map[string]func(t *testing.T, path string){
		"fifo": func(t *testing.T, path string) {
			if err := syscall.Mkfifo(path, 0o600); err != nil {
				t.Skip("no mkfifo:", err)
			}
		},
		"group and other writable": func(t *testing.T, path string) {
			writeFile(t, path, oneKeyList("evil@host", "key-evil"))
			if err := os.Chmod(path, 0o664); err != nil {
				t.Fatal(err)
			}
		},
		"other writable": func(t *testing.T, path string) {
			writeFile(t, path, oneKeyList("evil@host", "key-evil"))
			if err := os.Chmod(path, 0o602); err != nil {
				t.Fatal(err)
			}
		},
		"directory": func(t *testing.T, path string) {
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, mk := range setup {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.l.FailWith(keyclienttest.Status(500, ""))
			e.secret("key-evil")
			if err := os.MkdirAll(filepath.Dir(e.cache()), 0o700); err != nil {
				t.Fatal(err)
			}
			mk(t, e.cache())
			s := e.source(e.opts())
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			done := make(chan struct{})
			go func() {
				defer close(done)
				s.Start(ctx)
			}()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("Start blocked on the cache")
			}
			t.Cleanup(func() { cancel(); s.Pull(ctx) })
			mustMiss(t, s, "key-evil")
			if !strings.Contains(e.log.String(), "cache ignored") {
				t.Fatalf("not logged:\n%s", e.log.String())
			}
		})
	}
	t.Run("0640 and 0600 are loaded", func(t *testing.T) {
		for _, mode := range []os.FileMode{0o600, 0o640, 0o400} {
			e := newEnv(t)
			e.l.FailWith(keyclienttest.Status(500, ""))
			if err := os.MkdirAll(filepath.Dir(e.cache()), 0o700); err != nil {
				t.Fatal(err)
			}
			writeFile(t, e.cache(), oneKeyList("ok@host", "key-ok"))
			os.Chmod(e.cache(), mode)
			s := e.source(e.opts())
			e.begin(s)
			mustFind(t, s, "key-ok", "ok@host", "agent")
		}
	})
}
