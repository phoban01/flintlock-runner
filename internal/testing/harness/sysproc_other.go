//go:build !linux

package harness

import "syscall"

// runnerSysProcAttr puts the Runner in its own process group, so that the
// harness can signal it and anything it started together. Outside Linux
// there is no parent-death signal; Shutdown and the test cleanup are what
// stop the Runner.
func runnerSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}
