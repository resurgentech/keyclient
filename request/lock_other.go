//go:build !(linux || darwin || freebsd || netbsd || openbsd || dragonfly)

package request

import (
	"context"
	"io/fs"
	"os"
)

// lockSupported is false here: without the lock two copies of a tool could file
// two requests or poll one request at once, so no request is filed or resumed
// on this platform. A token already on disk is still returned.
const lockSupported = false

const pendingOpenFlags = os.O_RDONLY

type fileLock struct{}

func openLock(string) (*fileLock, error)     { return nil, errUnsupported }
func (*fileLock) try() (bool, error)         { return false, errUnsupported }
func (*fileLock) wait(context.Context) error { return errUnsupported }
func (*fileLock) release()                   {}
func ownedByMe(fs.FileInfo) bool             { return true }
