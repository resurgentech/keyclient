//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package request

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// pendingOpenFlags open the pending file without following a symlink and
// without blocking on a FIFO.
const pendingOpenFlags = os.O_RDONLY | syscall.O_NOFOLLOW | syscall.O_NONBLOCK

// lockRetry is the pause between two attempts to take a busy lock.
const lockRetry = 25 * time.Millisecond

// lockFile takes an exclusive flock on path (TokenPath + ".lock"), creating it
// with mode 0600 and its directory with mode 0700 when missing, never through
// a symlink. It waits while another holder has it, until ctx ends. The file is
// left in place: removing it would let two holders lock two different files.
func lockFile(ctx context.Context, path string) (unlock func(), err error) {
	if err := mkdirPrivate(filepath.Dir(path)); err != nil {
		return nil, fmt.Errorf("key request: cannot create the directory of %s: %w", path, err)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o600)
	if err != nil {
		return nil, fmt.Errorf("key request: cannot open the lock file %s: %w", path, err)
	}
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("key request: the lock file %s is not a regular file", path)
	}
	rc, err := f.SyscallConn()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("key request: cannot lock %s: %w", path, err)
	}
	flock := func(how int) error {
		var ferr error
		if err := rc.Control(func(fd uintptr) { ferr = syscall.Flock(int(fd), how) }); err != nil {
			return err
		}
		return ferr
	}
	for {
		err := flock(syscall.LOCK_EX | syscall.LOCK_NB)
		if err == nil {
			return func() {
				flock(syscall.LOCK_UN)
				f.Close()
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			f.Close()
			return nil, fmt.Errorf("key request: cannot lock %s: %w", path, err)
		}
		t := time.NewTimer(lockRetry)
		select {
		case <-ctx.Done():
			t.Stop()
			f.Close()
			return nil, fmt.Errorf("key request: waiting for the lock %s: %w", path, ctx.Err())
		case <-t.C:
		}
	}
}

// mkdirPrivate creates dir and any missing parents with mode 0700, as the
// token file's writer does: each level it creates is chmod-ed to 0700 through
// a descriptor opened without following a symlink, so the umask cannot change
// the mode. Existing directories are left alone.
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
	for i := len(missing) - 1; i >= 0; i-- {
		d := missing[i]
		if err := os.Mkdir(d, 0o700); err != nil {
			if errors.Is(err, fs.ErrExist) {
				continue // someone else made it: not ours to chmod
			}
			return err
		}
		f, err := os.OpenFile(d, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_DIRECTORY, 0)
		if err != nil {
			return err
		}
		err = f.Chmod(0o700)
		f.Close()
		if err != nil {
			return err
		}
	}
	return nil
}
