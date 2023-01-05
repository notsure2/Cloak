//go:build windows
// +build windows

package server

import "syscall"

func platformfd(fd uintptr) syscall.Handle {
	return syscall.Handle(fd)
}
