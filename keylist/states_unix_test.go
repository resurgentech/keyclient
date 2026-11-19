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

	"github.com/resurgentech/keyclient/keyclienttest"
	"github.com/resurgentech/keyclient/keylist"
)

// TestK7_EmptyingNeverFollowsASymlink: with the cache directory read-only and a
// symlink at the cache path, the fallback must not empty the link's target
// (K-6, K-7). The failure is logged as an error.
func TestK7_EmptyingNeverFollowsASymlink(t *testing.T) {
	e := newEnv(t)
	e.add("alice@laptop", "agent", "key-alice")
	target := filepath.Join(e.dir, "elsewhere.txt")
	writeFile(t, target, "precious")
	dir := filepath.Dir(e.cache())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, e.cache()); err != nil {
		t.Fatal(err)
	}
	readOnlyDir(t, dir)
	s := e.source(e.opts())
	if err := s.Pull(context.Background()); !errors.Is(err, keylist.ErrCacheWrite) {
		t.Fatalf("Pull = %v, want ErrCacheWrite", err)
	}
	if b, err := os.ReadFile(target); err != nil || string(b) != "precious" {
		t.Fatalf("the link's target now holds %q (err %v)", b, err)
	}
	if out := e.log.String(); !strings.Contains(out, "level=ERROR") || !strings.Contains(out, "cannot be removed or emptied") {
		t.Fatalf("K-7 not logged as an error:\n%s", out)
	}
	mustFind(t, s, "key-alice", "alice@laptop", "agent")
}

// TestK7_NonRegularCacheIsNotEmptied: a FIFO at the cache path (read-only
// directory) is neither opened for writing nor emptied; the pull returns.
func TestK7_NonRegularCacheIsNotEmptied(t *testing.T) {
	e := newEnv(t)
	e.add("alice@laptop", "agent", "key-alice")
	dir := filepath.Dir(e.cache())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(e.cache(), 0o600); err != nil {
		t.Skip("no FIFOs here:", err)
	}
	readOnlyDir(t, dir)
	s := e.source(e.opts())
	if err := s.Pull(context.Background()); !errors.Is(err, keylist.ErrCacheWrite) {
		t.Fatalf("Pull = %v, want ErrCacheWrite", err)
	}
	if !strings.Contains(e.log.String(), "cannot be removed or emptied") {
		t.Fatalf("K-7 not logged:\n%s", e.log.String())
	}
}

// TestCacheOwnedByAnotherUserIsIgnored: a cache that another user owns is not
// loaded, even when its mode is fine. /etc/passwd stands in for such a file: it
// belongs to root and is not writable by others.
func TestCacheOwnedByAnotherUserIsIgnored(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root owns /etc/passwd")
	}
	if fi, err := os.Stat("/etc/passwd"); err != nil || fi.Mode().Perm()&0o022 != 0 {
		t.Skip("no suitable foreign file")
	}
	e := newEnv(t)
	if err := os.MkdirAll(filepath.Dir(e.cache()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", e.cache()); err != nil {
		t.Fatal(err)
	}
	e.l.FailWith(keyclienttest.Status(500, ""))
	s := e.source(e.opts())
	e.begin(s)
	if out := e.log.String(); !strings.Contains(out, "not owned by the current user") {
		t.Fatalf("the foreign owner was not the reason:\n%s", out)
	}
	if st := s.Status(); st.Origin != keylist.OriginNone {
		t.Fatalf("Origin = %v", st.Origin)
	}
}
