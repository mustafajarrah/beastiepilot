package main

import (
	"os"
	"syscall"
	"unsafe"
)

// termSize returns the size, in character cells, of the terminal that f is
// attached to. ok is false if f is not a terminal or the size is unknown.
//
// It asks the kernel directly so that the program needs nothing outside the
// standard library.
func termSize(f *os.File) (cols, rows int, ok bool) {
	// struct winsize from <sys/ioctl.h>.
	var ws struct{ row, col, xpixel, ypixel uint16 }
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), uintptr(syscall.TIOCGWINSZ), uintptr(unsafe.Pointer(&ws)))
	if errno != 0 || ws.col == 0 || ws.row == 0 {
		return 0, 0, false
	}
	return int(ws.col), int(ws.row), true
}
