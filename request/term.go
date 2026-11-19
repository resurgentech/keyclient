package request

import "os"

// IsTerminal reports whether f is a terminal: the TCGETS (Linux) or TIOCGETA
// (the BSDs and macOS) ioctl succeeds on it, or on Windows GetConsoleMode does.
// A regular file, a pipe and /dev/null are not terminals. Elsewhere it reports
// false. A calling tool uses it to choose Wait true, or printing the notice and
// exiting.
func IsTerminal(f *os.File) bool {
	if f == nil {
		return false
	}
	rc, err := f.SyscallConn()
	if err != nil {
		return false
	}
	ok := false
	if rc.Control(func(fd uintptr) { ok = isTerminal(fd) }) != nil {
		return false
	}
	return ok
}
