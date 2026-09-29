//go:build !windows

package supervisor

import (
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// On Unix, start each app in its own process group so a stop reaches any
// children it spawned (e.g. a shell wrapper and the real server).
func prepare(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// afterStart is a no-op on Unix. A normal shutdown (Ctrl+C, SIGTERM) stops
// every app, but a SIGKILLed homebase leaves them running; its next start
// will then report them as "external" rather than launching duplicates.
func afterStart(*exec.Cmd) error { return nil }

// terminate asks the group to exit (SIGTERM), then forces it (SIGKILL)
// if it hasn't gone by the deadline. done is closed when the process exits.
func terminate(pid int, timeout time.Duration, done <-chan struct{}) {
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	select {
	case <-done:
	case <-time.After(timeout):
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}
}

// memoryBytes reports resident memory via ps.
func memoryBytes(pid int) (int64, bool) {
	out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0, false
	}
	kb, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0, false
	}
	return kb * 1024, true
}
