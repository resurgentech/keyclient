// Package atomicfile writes a file so that a reader, or a crash, sees the old
// content or the new content and never a partial file. It is used for the
// files that hold secrets, so every file it makes is private to the user.
package atomicfile

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Write writes data to path atomically: a temporary file in the same directory,
// created with mode 0600, written, synced, chmod 0600 (umask-proof), closed,
// renamed over path; then the directory is synced (best effort). A missing
// directory is created with mode 0700 (every created level; the created ones
// are chmod-ed to 0700 explicitly so the umask cannot widen or narrow them); an
// existing directory's mode is left alone. On any failure the temporary file is
// removed and path is unchanged. Errors name the path, never the data.
func Write(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := mkdirPrivate(dir); err != nil {
		return fmt.Errorf("atomicfile: write %s: %w", path, err)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("atomicfile: write %s: %w", path, err)
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			tmp.Close()
			os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("atomicfile: write %s: %w", path, err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("atomicfile: write %s: %w", path, err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("atomicfile: write %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("atomicfile: write %s: %w", path, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("atomicfile: write %s: %w", path, err)
	}
	ok = true
	if d, err := os.Open(dir); err == nil {
		d.Sync() // best effort
		d.Close()
	}
	return nil
}

// mkdirPrivate creates dir and any missing parents with mode 0700. Only the
// levels this call created are chmod-ed.
func mkdirPrivate(dir string) error {
	var missing []string
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Stat(d); err == nil {
			break
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		missing = append(missing, d)
		if filepath.Dir(d) == d {
			break
		}
	}
	if len(missing) == 0 {
		return nil
	}
	// Top down, chmod right after each mkdir, so that a narrow umask cannot
	// stop the next level from being created.
	for i := len(missing) - 1; i >= 0; i-- {
		d := missing[i]
		if beforeMkdir != nil {
			beforeMkdir(d)
		}
		if err := os.Mkdir(d, 0o700); err != nil {
			if errors.Is(err, fs.ErrExist) {
				continue // someone else made it: not ours to chmod
			}
			return err
		}
		if afterMkdir != nil {
			afterMkdir(d)
		}
		if err := chmodDirNoFollow(d, 0o700); err != nil {
			return err
		}
	}
	return nil
}

// Test hooks around each mkdir; nil outside tests.
var beforeMkdir, afterMkdir func(dir string)
