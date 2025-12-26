//go:build windows

// This file only builds on Windows, all archs

package main

import "errors"

// mkfifo - stub for Windows (not supported yet)
// TODO: Port this stub to use CreateNamedPipe() from NtAPI?
func mkfifo(path string, mode uint32) error {
	return errors.New("FIFO not supported yet on Windows")
}
