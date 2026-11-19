//go:build unix

package atomicfile

import (
	"os"
	"path/filepath"
	"testing"
)

// T-25, K-13: a crash during Write can leave a temporary file; RemoveTemps
// removes those of one path and nothing else.
func TestRemoveTempsRemovesOnlyThisPathsTemps(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	mine := filepath.Join(dir, ".token.tmp-123")
	other := filepath.Join(dir, ".token2.tmp-456")
	plain := filepath.Join(dir, "token.tmp-789")
	for _, p := range []string{path, mine, other, plain} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A directory and a link with a matching name are not removed.
	if err := os.Mkdir(filepath.Join(dir, ".token.tmp-dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, filepath.Join(dir, ".token.tmp-link")); err != nil {
		t.Fatal(err)
	}
	if err := RemoveTemps(path); err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]bool{
		path: true, mine: false, other: true, plain: true,
		filepath.Join(dir, ".token.tmp-dir"): true, filepath.Join(dir, ".token.tmp-link"): true,
	} {
		_, err := os.Lstat(p)
		if got := err == nil; got != want {
			t.Errorf("%s exists = %v, want %v", filepath.Base(p), got, want)
		}
	}
	// The names Write really makes are removed; the temporary files of a token
	// whose name merely starts like this one's are not (T-25, K-13).
	if err := Write(path, []byte("y")); err != nil {
		t.Fatal(err)
	}
	neighbour := filepath.Join(dir, "token.tmp-x")
	if err := Write(neighbour, []byte("z")); err != nil {
		t.Fatal(err)
	}
	created, err := os.CreateTemp(dir, ".token.tmp-*")
	if err != nil {
		t.Fatal(err)
	}
	created.Close()
	ncreated, err := os.CreateTemp(dir, ".token.tmp-x.tmp-*")
	if err != nil {
		t.Fatal(err)
	}
	ncreated.Close()
	if err := RemoveTemps(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(created.Name()); err == nil {
		t.Errorf("%s (a temporary file of token) remains", filepath.Base(created.Name()))
	}
	if _, err := os.Lstat(ncreated.Name()); err != nil {
		t.Errorf("%s (a temporary file of token.tmp-x) was removed", filepath.Base(ncreated.Name()))
	}
	// A missing directory is not an error.
	if err := RemoveTemps(filepath.Join(dir, "missing", "token")); err != nil {
		t.Errorf("missing directory: %v", err)
	}
}
