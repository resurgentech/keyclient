//go:build !unix

package atomicfile

import (
	"errors"
	"os"
)

// chmodDirNoFollow changes the mode of dir unless it is a symlink. Without
// O_NOFOLLOW the check and the change are two steps.
func chmodDirNoFollow(dir string, mode os.FileMode) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		return errors.New("not a directory")
	}
	return os.Chmod(dir, mode)
}
