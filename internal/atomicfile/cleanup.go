package atomicfile

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// RemoveTemps removes the temporary files that an interrupted [Write] to path
// can leave beside it: exactly the names Write creates, ".<base>.tmp-" followed
// by the decimal digits os.CreateTemp puts in place of its "*" (so the temporary
// files of a path named "<base>.tmp-x" are not touched). Only regular files are removed, never
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
		if !isTempOf(e.Name(), prefix) || !e.Type().IsRegular() {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) && first == nil {
			first = err
		}
	}
	return first
}

// isTempOf reports whether name is prefix followed by one or more ASCII digits,
// the shape os.CreateTemp gives the random part.
func isTempOf(name, prefix string) bool {
	rest, ok := strings.CutPrefix(name, prefix)
	if !ok || rest == "" {
		return false
	}
	for i := 0; i < len(rest); i++ {
		if rest[i] < '0' || rest[i] > '9' {
			return false
		}
	}
	return true
}
