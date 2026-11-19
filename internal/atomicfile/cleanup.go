package atomicfile

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// RemoveTemps removes the temporary files that an interrupted [Write] to path
// can leave beside it (".<base>.tmp-*"). Only regular files are removed, never
// a directory or a symlink, and only those of this path. A missing directory
// is not an error. The caller must be the only writer of path (it holds the
// path's lock, or is starting up before any write).
func RemoveTemps(path string) error {
	dir, base := filepath.Split(path)
	if dir == "" {
		dir = "."
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	prefix := "." + base + ".tmp-"
	var first error
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), prefix) || !e.Type().IsRegular() {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) && first == nil {
			first = err
		}
	}
	return first
}
