//go:build android
// +build android

package common

func Platformfd(fd uintptr) int {
	return int(fd)
}
