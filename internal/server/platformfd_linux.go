//go:build linux
// +build linux

package server

func platformfd(fd uintptr) int {
	return int(fd)
}
