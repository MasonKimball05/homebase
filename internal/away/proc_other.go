//go:build !windows

package away

import (
	"os"
	"os/exec"
	"syscall"
)

func hide(*exec.Cmd) {}

// stoppable: cancelling the context ends the whole process group.
func stoppable(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}

func linkDir(src, dst string) error { return os.Symlink(src, dst) }

func unlinkDir(p string) {
	if fi, err := os.Lstat(p); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		_ = os.Remove(p)
	}
}

// IdleSeconds isn't available here (the idle reporter is for Windows).
func IdleSeconds() (int64, bool) { return 0, false }
