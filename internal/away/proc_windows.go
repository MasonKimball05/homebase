//go:build windows

package away

import (
	"os/exec"
	"strconv"
	"syscall"
	"unsafe"
)

const createNoWindow = 0x08000000

func hide(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNoWindow}
}

// stoppable: cancelling the context ends the whole tree (npx → node → …), not just the first process.
func stoppable(cmd *exec.Cmd) {
	hide(cmd)
	cmd.Cancel = func() error {
		return exec.Command("taskkill", "/PID", strconv.Itoa(cmd.Process.Pid), "/T", "/F").Run()
	}
}

// linkDir: a directory junction (no admin rights needed, unlike a symlink).
func linkDir(src, dst string) error {
	c := exec.Command("cmd", "/c", "mklink", "/J", dst, src)
	hide(c)
	return c.Run()
}

// unlinkDir removes a junction without touching what it points at.
func unlinkDir(p string) {
	if isJunction(p) {
		_ = syscall.RemoveDirectory(syscall.StringToUTF16Ptr(p))
	}
}

func isJunction(p string) bool {
	attrs, err := syscall.GetFileAttributes(syscall.StringToUTF16Ptr(p))
	return err == nil && attrs&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0
}

var (
	user32          = syscall.NewLazyDLL("user32.dll")
	kernel32        = syscall.NewLazyDLL("kernel32.dll")
	procLastInput   = user32.NewProc("GetLastInputInfo")
	procTickCount64 = kernel32.NewProc("GetTickCount64")
	procFreeConsole = kernel32.NewProc("FreeConsole")
)

// DetachConsole lets go of the console window Windows opens for the idle
// reporter at logon. Closing that window killed the reporter (twice, 10-18
// and 10-19), and with it "when idle" mode; without a console there's
// nothing to close.
func DetachConsole() { procFreeConsole.Call() }

type lastInputInfo struct {
	size uint32
	time uint32
}

// IdleSeconds: how long since this session's last keyboard or mouse input.
func IdleSeconds() (int64, bool) {
	info := lastInputInfo{size: uint32(unsafe.Sizeof(lastInputInfo{}))}
	if ok, _, _ := procLastInput.Call(uintptr(unsafe.Pointer(&info))); ok == 0 {
		return 0, false
	}
	now, _, _ := procTickCount64.Call()
	// GetLastInputInfo's tick is 32-bit; compare in the same width.
	return int64(uint32(now)-info.time) / 1000, true
}
