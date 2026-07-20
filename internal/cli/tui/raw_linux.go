//go:build linux

package tui

import "syscall"

// termios ioctl request numbers for Linux.
const (
	ioctlReadTermios  uintptr = syscall.TCGETS
	ioctlWriteTermios uintptr = syscall.TCSETS
)
