package harness

import "syscall"

// runnerSysProcAttr puts the Runner in its own process group, so that the
// harness can signal it and anything it started together, and asks the
// kernel to kill it if the thread that started it dies, which is what keeps
// a crashed or killed test binary from leaving a Runner behind.
func runnerSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
}
