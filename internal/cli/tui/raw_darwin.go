//go:build darwin

package tui

import "syscall"

// termios ioctl request numbers for macOS/BSD.
const (
	ioctlReadTermios  uintptr = syscall.TIOCGETA
	ioctlWriteTermios uintptr = syscall.TIOCSETA
)
