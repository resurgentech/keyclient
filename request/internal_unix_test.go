//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package request

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// T-28 for the pending file: one not owned by this process's effective user is
// refused with an error that names it.
func TestPendingNotOwnedRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token.pending")
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readPending(path); err != nil {
		t.Fatalf("owned by this user: %v", err)
	}
	t.Cleanup(SetGeteuid(func() int { return os.Geteuid() + 1 }))
	_, exists, err := readPending(path)
	if !exists || err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "owned") {
		t.Errorf("exists=%v err=%v", exists, err)
	}
}
