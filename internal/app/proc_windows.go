//go:build windows

package app

import "os"

// processAlive reports whether a process with this pid exists. On Windows
// FindProcess fails for a pid that is not running.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	_ = p.Release()
	return true
}
