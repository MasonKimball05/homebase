//go:build windows

package supervisor

import (
	"encoding/csv"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

const createNoWindow = 0x08000000

// On Windows, don't pop up a console window for each app.
func prepare(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
}

// terminate ends the app and everything it started. Windows has no SIGTERM
// equivalent for background console apps, so this is a forced stop of the
// whole process tree (taskkill /T), as Task Manager's "End task" would do.
// SQLite and sentinel's state file are both written crash-safely, so this is fine.
func terminate(pid int, timeout time.Duration, done <-chan struct{}) {
	kill := exec.Command("taskkill", "/PID", strconv.Itoa(pid), "/T", "/F")
	kill.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	_ = kill.Run()
	select {
	case <-done:
	case <-time.After(timeout):
	}
}

// memoryBytes reads the working set from tasklist's CSV output, e.g.
// "JobTracker.exe","2532","Console","1","98,432 K"
func memoryBytes(pid int) (int64, bool) {
	cmd := exec.Command("tasklist", "/FI", "PID eq "+strconv.Itoa(pid), "/FO", "CSV", "/NH")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	out, err := cmd.Output()
	if err != nil {
		return 0, false
	}
	rec, err := csv.NewReader(strings.NewReader(string(out))).Read()
	if err != nil || len(rec) < 5 {
		return 0, false // "INFO: No tasks are running..." isn't CSV
	}
	digits := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, rec[4])
	kb, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return 0, false
	}
	return kb * 1024, true
}

// ---- Job Object: tie every app's lifetime to homebase's ----
//
// If homebase is killed (Task Scheduler "End", a crash, Task Manager), its
// apps would normally keep running as orphans. Putting each app in a Job
// Object with KILL_ON_JOB_CLOSE makes Windows end them automatically the
// moment homebase's handle to the job closes, which happens however it exits.

var (
	kernel32                     = syscall.NewLazyDLL("kernel32.dll")
	procCreateJobObjectW         = kernel32.NewProc("CreateJobObjectW")
	procSetInformationJobObject  = kernel32.NewProc("SetInformationJobObject")
	procAssignProcessToJobObject = kernel32.NewProc("AssignProcessToJobObject")

	jobOnce   sync.Once
	jobHandle syscall.Handle
	jobErr    error
)

const (
	jobObjectExtendedLimitInformationClass = 9
	jobObjectLimitKillOnJobClose           = 0x2000
	processSetQuota                        = 0x0100
	processTerminate                       = 0x0001
)

// Mirrors JOBOBJECT_EXTENDED_LIMIT_INFORMATION (64-bit layout).
type jobBasicLimits struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

type jobExtendedLimits struct {
	Basic                 jobBasicLimits
	IoInfo                [6]uint64
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

func killOnExitJob() (syscall.Handle, error) {
	jobOnce.Do(func() {
		h, _, err := procCreateJobObjectW.Call(0, 0)
		if h == 0 {
			jobErr = err
			return
		}
		info := jobExtendedLimits{Basic: jobBasicLimits{LimitFlags: jobObjectLimitKillOnJobClose}}
		ok, _, err := procSetInformationJobObject.Call(h, jobObjectExtendedLimitInformationClass,
			uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info))
		if ok == 0 {
			jobErr = err
			return
		}
		// Deliberately never closed: it must live exactly as long as homebase.
		jobHandle = syscall.Handle(h)
	})
	return jobHandle, jobErr
}

// afterStart adds the new app to homebase's job. Anything the app launches
// later joins the job automatically.
func afterStart(cmd *exec.Cmd) error {
	job, err := killOnExitJob()
	if err != nil {
		return err
	}
	proc, err := syscall.OpenProcess(processSetQuota|processTerminate, false, uint32(cmd.Process.Pid))
	if err != nil {
		return err
	}
	defer syscall.CloseHandle(proc)
	if ok, _, err := procAssignProcessToJobObject.Call(uintptr(job), uintptr(proc)); ok == 0 {
		return err
	}
	return nil
}
