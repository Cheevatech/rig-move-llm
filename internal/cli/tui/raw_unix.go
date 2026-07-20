//go:build darwin || linux

package tui

import (
	"os"
	"syscall"
	"unsafe"
)

// rawSupported reports that this build can put the terminal into raw mode (byte-at-
// a-time key reads for arrow/space navigation). Set false on platforms we do not
// implement termios for (see raw_other.go), which makes Select fall back to a
// numbered line prompt.
const rawSupported = true

// termState holds a terminal's original attributes so raw mode can be reversed.
type termState struct {
	fd  int
	old syscall.Termios
}

// makeRaw disables canonical mode and echo on the terminal so keystrokes (including
// arrow-key escape sequences) arrive immediately and are not printed. It leaves ISIG
// on, so Ctrl-C still interrupts normally. restore() must be called to undo it.
func makeRaw(f *os.File) (*termState, error) {
	fd := int(f.Fd())
	var old syscall.Termios
	if err := ioctlTermios(fd, ioctlReadTermios, &old); err != nil {
		return nil, err
	}
	raw := old
	raw.Lflag &^= syscall.ECHO | syscall.ICANON
	raw.Cc[syscall.VMIN] = 1
	raw.Cc[syscall.VTIME] = 0
	if err := ioctlTermios(fd, ioctlWriteTermios, &raw); err != nil {
		return nil, err
	}
	return &termState{fd: fd, old: old}, nil
}

func (s *termState) restore() {
	if s != nil {
		_ = ioctlTermios(s.fd, ioctlWriteTermios, &s.old)
	}
}

func ioctlTermios(fd int, req uintptr, t *syscall.Termios) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), req, uintptr(unsafe.Pointer(t)))
	if errno != 0 {
		return errno
	}
	return nil
}
