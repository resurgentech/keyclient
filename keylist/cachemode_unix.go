//go:build unix

package keylist

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
)

// cacheWritableByOthers reports whether group or others can write the cache.
func cacheWritableByOthers(m fs.FileMode) bool { return m&0o022 != 0 }

// cacheForeignOwner reports whether another user owns the cache file.
func cacheForeignOwner(fi fs.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) != os.Getuid()
}

// emptyFile truncates the regular file at path without following a symlink at
// path. It opens without blocking and without O_TRUNC, so that nothing but a
// regular file is ever opened for writing, and truncates what it opened.
func emptyFile(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return errors.New("not a regular file")
	}
	return f.Truncate(0)
}
