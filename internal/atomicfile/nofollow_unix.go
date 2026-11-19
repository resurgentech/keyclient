//go:build unix

package atomicfile

import (
	"errors"
	"os"
	"syscall"
)

// chmodDirNoFollow opens dir without following a symlink and changes the mode
// of what it opened, so a directory swapped for a link is never chmod-ed
// through the link.
func chmodDirNoFollow(dir string, mode os.FileMode) error {
	f, err := os.OpenFile(dir, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_DIRECTORY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return errors.New("not a directory")
	}
	return f.Chmod(mode)
}
