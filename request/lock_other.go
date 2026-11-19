//go:build !(linux || darwin || freebsd || netbsd || openbsd || dragonfly)

package request

import (
	"context"
	"os"
)

const pendingOpenFlags = os.O_RDONLY

// lockFile is not supported here: without the lock two copies of a tool could
// file two requests or poll one request at once, so no request is filed or
// resumed on this platform. A token already on disk is still returned.
func lockFile(context.Context, string) (func(), error) { return nil, errUnsupported }
