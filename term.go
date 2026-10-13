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

// rawInput switches the terminal f to handing over each key as it is pressed,
// without echoing it and without turning Ctrl-C into a signal, which is what
// a line editor needs. Output is left as it was. With fresh set, what was
// typed before this moment is discarded first.
//
// It returns the function that puts the terminal back, and that also discards
// whatever was typed or pasted and not read: what follows the line just
// entered must not go on to answer a question nobody has seen yet.
func rawInput(f *os.File, fresh bool) (restore func(), err error) {
	var old syscall.Termios
	if err := termios(f, ioctlGetTermios, &old); err != nil {
		return nil, err
	}
	raw := old
	raw.Iflag &^= syscall.IXON
	raw.Lflag &^= syscall.ECHO | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	raw.Cc[syscall.VMIN], raw.Cc[syscall.VTIME] = 1, 0
	request := uintptr(ioctlSetTermios)
	if fresh {
		request = ioctlSetTermiosFlush
	}
	if err := termios(f, request, &raw); err != nil {
		return nil, err
	}
	return func() { termios(f, ioctlSetTermiosFlush, &old) }, nil
}

// discardInput throws away what was typed on the terminal f and not read
// yet, so that only what is typed from now on counts.
func discardInput(f *os.File) {
	var now syscall.Termios
	if termios(f, ioctlGetTermios, &now) == nil {
		termios(f, ioctlSetTermiosFlush, &now)
	}
}

// termios reads or changes the modes of the terminal f.
func termios(f *os.File, request uintptr, t *syscall.Termios) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), request, uintptr(unsafe.Pointer(t))); errno != 0 {
		return errno
	}
	return nil
}
