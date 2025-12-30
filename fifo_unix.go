//go:build unix

// This file only builds on Linux, macOS, BSD, Android, etc.
// for all archs

package main

import "syscall"

// mkfifo - creates named pipe (Unix only)
func mkfifo(path string, mode uint32) error {
	return syscall.Mkfifo(path, mode)
}
