package atomicfile

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func mode(t *testing.T, p string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

func names(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range es {
		out = append(out, e.Name())
	}
	return out
}

func TestWriteModeIsUmaskProof(t *testing.T) {
	base := t.TempDir()
	old := syscall.Umask(0)
	defer syscall.Umask(old)
	p := filepath.Join(base, "a", "b", "token")
	if err := Write(p, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if m := mode(t, p); m != 0o600 {
		t.Fatalf("file mode %o", m)
	}
	for _, d := range []string{filepath.Dir(p), filepath.Dir(filepath.Dir(p))} {
		if m := mode(t, d); m != 0o700 {
			t.Fatalf("dir %s mode %o", d, m)
		}
	}
}

func TestWriteNarrowUmaskStillGivesUsableDirs(t *testing.T) {
	base := t.TempDir()
	old := syscall.Umask(0o377)
	defer syscall.Umask(old)
	p := filepath.Join(base, "d", "e", "token")
	if err := Write(p, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if m := mode(t, filepath.Dir(p)); m != 0o700 {
		t.Fatalf("dir mode %o", m)
	}
	if m := mode(t, p); m != 0o600 {
		t.Fatalf("file mode %o", m)
	}
}

func TestWriteExistingDirModeUnchanged(t *testing.T) {
	d := t.TempDir()
	if err := os.Chmod(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Write(filepath.Join(d, "t"), []byte("x")); err != nil {
		t.Fatal(err)
	}
	if m := mode(t, d); m != 0o755 {
		t.Fatalf("dir mode %o", m)
	}
}

func TestWriteOverwriteLeavesNoTemp(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "t")
	for _, s := range []string{"one", "two-longer"} {
		if err := Write(p, []byte(s)); err != nil {
			t.Fatal(err)
		}
	}
	b, _ := os.ReadFile(p)
	if string(b) != "two-longer" {
		t.Fatalf("content %q", b)
	}
	if n := names(t, d); len(n) != 1 || n[0] != "t" {
		t.Fatalf("dir holds %v", n)
	}
}

func TestWriteFailureKeepsOldAndNoTemp(t *testing.T) {
	const secret = "SECRET-distinctive-9f8e7d"
	t.Run("dir not writable", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root ignores directory modes")
		}
		d := t.TempDir()
		p := filepath.Join(d, "t")
		if err := Write(p, []byte("old")); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(d, 0o500); err != nil {
			t.Fatal(err)
		}
		defer os.Chmod(d, 0o700)
		err := Write(p, []byte(secret))
		if err == nil {
			t.Fatal("want error")
		}
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("error leaks data: %v", err)
		}
		if !strings.Contains(err.Error(), p) {
			t.Fatalf("error does not name the path: %v", err)
		}
		os.Chmod(d, 0o700)
		b, _ := os.ReadFile(p)
		if string(b) != "old" {
			t.Fatalf("content %q", b)
		}
		if n := names(t, d); len(n) != 1 {
			t.Fatalf("dir holds %v", n)
		}
	})
	t.Run("path is a directory", func(t *testing.T) {
		d := t.TempDir()
		p := filepath.Join(d, "t")
		if err := os.Mkdir(p, 0o700); err != nil {
			t.Fatal(err)
		}
		err := Write(p, []byte(secret))
		if err == nil {
			t.Fatal("want error")
		}
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("error leaks data: %v", err)
		}
		if n := names(t, d); len(n) != 1 || n[0] != "t" {
			t.Fatalf("dir holds %v", n)
		}
	})
	t.Run("parent is a file", func(t *testing.T) {
		d := t.TempDir()
		f := filepath.Join(d, "f")
		if err := os.WriteFile(f, []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
		err := Write(filepath.Join(f, "t"), []byte(secret))
		if err == nil || strings.Contains(err.Error(), secret) {
			t.Fatalf("err %v", err)
		}
	})
}
