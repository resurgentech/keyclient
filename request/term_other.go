//go:build !(linux || darwin || freebsd || netbsd || openbsd || dragonfly || windows)

package request

func isTerminal(uintptr) bool { return false }
