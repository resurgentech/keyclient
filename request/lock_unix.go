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

// lockSupported is true where key requests can be filed: the platform has
// flock.
const lockSupported = true

// pendingOpenFlags open the pending file without following a symlink and
// without blocking on a FIFO.
const pendingOpenFlags = os.O_RDONLY | syscall.O_NOFOLLOW | syscall.O_NONBLOCK

// lockRetry is the pause between two attempts to take a busy lock.
const lockRetry = 25 * time.Millisecond

// fileLock is the lock file of one token (TokenPath + ".lock"), open, and
// whether this process holds the flock on it.
type fileLock struct {
	path string
	f    *os.File
	rc   syscall.RawConn
	held bool
	done bool
}

// openLock opens path (TokenPath + ".lock") without taking the lock, creating
// it with mode 0600 and its directory with mode 0700 when missing, never
// through a symlink. It refuses a lock file that is not a regular file, is not
// owned by this user, or that group or others can write. The file is left in
// place: removing it would let two holders lock two different files. Every
// error names the lock file.
func openLock(path string) (*fileLock, error) {
	if err := mkdirPrivate(filepath.Dir(path)); err != nil {
		return nil, fmt.Errorf("key request: cannot create the directory of the lock file %s: %w", path, err)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o600)
	if err != nil {
		return nil, fmt.Errorf("key request: cannot open the lock file %s: %w", path, err)
	}
	fi, err := f.Stat()
	switch {
	case err != nil || !fi.Mode().IsRegular():
		err = fmt.Errorf("key request: the lock file %s is not a regular file", path)
	case !ownedByMe(fi):
		err = fmt.Errorf("key request: the lock file %s is not owned by this user", path)
	case fi.Mode().Perm()&0o022 != 0:
		err = fmt.Errorf("key request: the lock file %s can be written by group or others; fix its mode (0600) or remove it", path)
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	rc, err := f.SyscallConn()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("key request: cannot lock %s: %w", path, err)
	}
	return &fileLock{path: path, f: f, rc: rc}, nil
}

func (l *fileLock) flock(how int) error {
	var ferr error
	if err := l.rc.Control(func(fd uintptr) { ferr = syscall.Flock(int(fd), how) }); err != nil {
		return err
	}
	return ferr
}

// try takes the lock if it is free. It reports false when another holder has
// it. A file system without locks is an error: it fails closed (T-28).
func (l *fileLock) try() (bool, error) {
	for {
		err := l.flock(syscall.LOCK_EX | syscall.LOCK_NB)
		switch {
		case err == nil:
			l.held = true
			return true, nil
		case errors.Is(err, syscall.EINTR):
			continue
		case errors.Is(err, syscall.EWOULDBLOCK):
			return false, nil
		}
		return false, fmt.Errorf("key request: cannot lock %s: %w", l.path, err)
	}
}

// wait takes the lock, waiting while another holder has it, until ctx ends.
func (l *fileLock) wait(ctx context.Context) error {
	for {
		ok, err := l.try()
		if err != nil || ok {
			return err
		}
		t := time.NewTimer(lockRetry)
		select {
		case <-ctx.Done():
			t.Stop()
			return fmt.Errorf("key request: waiting for the lock %s: %w", l.path, ctx.Err())
		case <-t.C:
		}
	}
}

// release unlocks (if held) and closes the lock file. Later calls do nothing.
func (l *fileLock) release() {
	if l.done {
		return
	}
	l.done = true
	if l.held {
		l.flock(syscall.LOCK_UN)
	}
	l.f.Close()
}

// ownedByMe reports whether fi is owned by this process's user.
func ownedByMe(fi fs.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && uint64(st.Uid) == uint64(getuid())
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
