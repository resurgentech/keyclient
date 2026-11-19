//go:build linux

package request_test

import (
	"fmt"
	"os"
	"syscall"
	"testing"
	"unsafe"

	"github.com/resurgentech/keyclient/request"
)

// A pseudo-terminal is a terminal: both its master and its slave side.
func TestIsTerminalPTY(t *testing.T) {
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skip("no /dev/ptmx:", err)
	}
	defer m.Close()
	var unlock int32
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, m.Fd(), syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock))); errno != 0 {
		t.Skip("unlockpt:", errno)
	}
	var n uint32
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, m.Fd(), syscall.TIOCGPTN, uintptr(unsafe.Pointer(&n))); errno != 0 {
		t.Skip("ptsname:", errno)
	}
	s, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skip("open the slave:", err)
	}
	defer s.Close()
	if !request.IsTerminal(m) {
		t.Error("the pty master is not reported a terminal")
	}
	if !request.IsTerminal(s) {
		t.Error("the pty slave is not reported a terminal")
	}
}
