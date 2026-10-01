//go:build windows

package resources

import (
	"syscall"
	"time"
	"unsafe"
)

var (
	kernel32                 = syscall.NewLazyDLL("kernel32.dll")
	procGetSystemTimes       = kernel32.NewProc("GetSystemTimes")
	procGlobalMemoryStatusEx = kernel32.NewProc("GlobalMemoryStatusEx")
	procK32GetProcessMemInfo = kernel32.NewProc("K32GetProcessMemoryInfo")
)

const (
	processQueryLimitedInformation = 0x1000
	processVMRead                  = 0x0010
)

// Mirrors MEMORYSTATUSEX.
type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

// Mirrors PROCESS_MEMORY_COUNTERS.
type processMemoryCounters struct {
	Cb                         uint32
	PageFaultCount             uint32
	PeakWorkingSetSize         uintptr
	WorkingSetSize             uintptr
	QuotaPeakPagedPoolUsage    uintptr
	QuotaPagedPoolUsage        uintptr
	QuotaPeakNonPagedPoolUsage uintptr
	QuotaNonPagedPoolUsage     uintptr
	PagefileUsage              uintptr
	PeakPagefileUsage          uintptr
}

type windowsPlatform struct{}

func newPlatform() platform { return windowsPlatform{} }

// A FILETIME used as a duration counts 100-nanosecond ticks.
func ticks(ft syscall.Filetime) time.Duration {
	return time.Duration(uint64(ft.HighDateTime)<<32|uint64(ft.LowDateTime)) * 100
}

func (windowsPlatform) system() (cpuTimes, memInfo, error) {
	var idle, kernel, user syscall.Filetime
	if ok, _, err := procGetSystemTimes.Call(uintptr(unsafe.Pointer(&idle)), uintptr(unsafe.Pointer(&kernel)), uintptr(unsafe.Pointer(&user))); ok == 0 {
		return cpuTimes{}, memInfo{}, err
	}
	ms := memoryStatusEx{Length: uint32(unsafe.Sizeof(memoryStatusEx{}))}
	if ok, _, err := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&ms))); ok == 0 {
		return cpuTimes{}, memInfo{}, err
	}
	return cpuTimes{Idle: ticks(idle), Kernel: ticks(kernel), User: ticks(user)},
		memInfo{Total: ms.TotalPhys, Available: ms.AvailPhys}, nil
}

// processes lists every process with its parent, from a Toolhelp snapshot.
func (windowsPlatform) processes() ([]procLink, error) {
	snap, err := syscall.CreateToolhelp32Snapshot(syscall.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer syscall.CloseHandle(snap)

	var out []procLink
	entry := syscall.ProcessEntry32{Size: uint32(unsafe.Sizeof(syscall.ProcessEntry32{}))}
	for err = syscall.Process32First(snap, &entry); err == nil; err = syscall.Process32Next(snap, &entry) {
		out = append(out, procLink{PID: entry.ProcessID, PPID: entry.ParentProcessID})
	}
	return out, nil // the loop ends with ERROR_NO_MORE_FILES
}

func (windowsPlatform) stats(pid uint32) (procStats, bool) {
	h, err := syscall.OpenProcess(processQueryLimitedInformation|processVMRead, false, pid)
	if err != nil {
		return procStats{}, false
	}
	defer syscall.CloseHandle(h)

	var created, exited, kernel, user syscall.Filetime
	if err := syscall.GetProcessTimes(h, &created, &exited, &kernel, &user); err != nil {
		return procStats{}, false
	}
	pmc := processMemoryCounters{Cb: uint32(unsafe.Sizeof(processMemoryCounters{}))}
	if ok, _, _ := procK32GetProcessMemInfo.Call(uintptr(h), uintptr(unsafe.Pointer(&pmc)), uintptr(pmc.Cb)); ok == 0 {
		return procStats{}, false
	}
	return procStats{
		Created: time.Unix(0, created.Nanoseconds()),
		CPU:     ticks(kernel) + ticks(user),
		Memory:  uint64(pmc.WorkingSetSize),
	}, true
}
