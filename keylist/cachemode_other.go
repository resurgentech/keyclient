//go:build !unix

package keylist

import (
	"errors"
	"io/fs"
	"os"
)

// cacheWritableByOthers is false where the file mode says nothing about who can
// write (permission bits are not meaningful there).
func cacheWritableByOthers(fs.FileMode) bool { return false }

// cacheForeignOwner is false where the owner of a file is not examined.
func cacheForeignOwner(fs.FileInfo) bool { return false }

// emptyFile truncates the regular file at path, unless path is a symlink or
// not a regular file. Without O_NOFOLLOW the check and the truncation are two
// steps.
func emptyFile(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return errors.New("not a regular file")
	}
	return os.Truncate(path, 0)
}
