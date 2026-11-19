//go:build unix

package atomicfile

import (
	"os"
	"path/filepath"
	"testing"
)

// A directory that another process creates between the check and the mkdir
// already existed for this call: its mode is left alone.
func TestWriteDoesNotChmodADirectoryCreatedByOthers(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "conf")
	beforeMkdir = func(d string) {
		if d == dir {
			if err := os.Mkdir(d, 0o755); err != nil {
				t.Fatal(err)
			}
			os.Chmod(d, 0o755)
		}
	}
	t.Cleanup(func() { beforeMkdir = nil })
	if err := Write(filepath.Join(dir, "token"), []byte("x")); err != nil {
		t.Fatal(err)
	}
	if got := mode(t, dir); got != 0o755 {
		t.Fatalf("directory created by someone else was chmod-ed to %04o", got)
	}
}

// A directory swapped for a symlink between the mkdir and the chmod is never
// chmod-ed through the link.
func TestWriteNeverChmodsThroughASymlink(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "target")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	os.Chmod(target, 0o755)
	dir := filepath.Join(base, "conf")
	afterMkdir = func(d string) {
		if d == dir {
			os.Remove(d)
			if err := os.Symlink(target, d); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Cleanup(func() { afterMkdir = nil })
	err := Write(filepath.Join(dir, "token"), []byte("x"))
	if got := mode(t, target); got != 0o755 {
		t.Fatalf("symlink target chmod-ed to %04o (Write err %v)", got, err)
	}
	if err == nil {
		t.Fatal("Write succeeded through a directory replaced by a symlink")
	}
}
