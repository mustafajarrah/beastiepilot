//go:build darwin || dragonfly || freebsd || netbsd || openbsd

package main

import "syscall"

// The ioctl requests that read and change the modes of a terminal. The last
// one also discards the input that has not been read yet.
const (
	ioctlGetTermios      = syscall.TIOCGETA
	ioctlSetTermios      = syscall.TIOCSETA
	ioctlSetTermiosFlush = syscall.TIOCSETAF
)
