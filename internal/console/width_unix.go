//go:build darwin || linux || freebsd || netbsd || openbsd || dragonfly

package console

import (
	"os"
	"syscall"
	"unsafe"
)

// queryTerminalWidth asks the kernel for the window size. This project keeps a
// deliberately small go.mod, so it is a direct TIOCGWINSZ ioctl rather than a
// terminal library; any failure returns 0 and the caller falls back to a safe
// default width.
func queryTerminalWidth(file *os.File) int {
	var size struct{ rows, columns, xPixels, yPixels uint16 }
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, file.Fd(), uintptr(syscall.TIOCGWINSZ), uintptr(unsafe.Pointer(&size)))
	if errno != 0 {
		return 0
	}
	return int(size.columns)
}
