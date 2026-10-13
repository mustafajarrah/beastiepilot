//go:build linux

package main

import "syscall"

// The ioctl requests that read and change the modes of a terminal. The last
// one also discards the input that has not been read yet: it is TCSETSF,
// which package syscall does not name and which follows TCSETS and TCSETSW on
// every architecture.
const (
	ioctlGetTermios      = syscall.TCGETS
	ioctlSetTermios      = syscall.TCSETS
	ioctlSetTermiosFlush = syscall.TCSETS + 2
)
