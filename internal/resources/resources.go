// Package resources samples how much CPU and memory the machine is using,
// and how much of that belongs to each app homebase runs.
//
// An app's usage is the sum over its process tree: the process homebase
// started plus everything it launched (a Rails app's helpers, a shell wrapper
// and the real server...). The math lives here and is tested with a fake
// platform; reading the numbers from the OS is in platform_windows.go.
package resources

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"time"
)

// ErrUnsupported means this OS has no implementation (only Windows does;
// homebase's home is a Windows desktop).
var ErrUnsupported = errors.New("resource monitoring isn't supported on this OS")

// AppUsage is one app's share of the machine.
type AppUsage struct {
	CPUPercent  float64 `json:"cpu_percent"`  // of the whole machine, like Task Manager
	MemoryBytes uint64  `json:"memory_bytes"` // working set, summed over the process tree
	Processes   int     `json:"processes"`    // processes in the tree
}

// Point is one history entry for the machine-wide graphs.
type Point struct {
	At         time.Time `json:"at"`
	CPUPercent float64   `json:"cpu_percent"`
	MemPercent float64   `json:"mem_percent"`
}

// Snapshot is the latest reading.
type Snapshot struct {
	At            time.Time           `json:"at"`
	CPUPercent    float64             `json:"cpu_percent"`
	MemUsedBytes  uint64              `json:"mem_used_bytes"`
	MemTotalBytes uint64              `json:"mem_total_bytes"`
	Cores         int                 `json:"cores"` // logical processors
	Apps          map[string]AppUsage `json:"apps"`
	History       []Point             `json:"history"` // oldest first
}

// ---- what a platform provides ----

type cpuTimes struct {
	Idle, Kernel, User time.Duration // since boot; on Windows, Kernel includes Idle
}

type memInfo struct {
	Total, Available uint64
}

type procLink struct {
	PID, PPID uint32
}

type procStats struct {
	Created time.Time     // distinguishes a reused PID from the original
	CPU     time.Duration // user + kernel time, since the process started
	Memory  uint64        // working set
}

type platform interface {
	system() (cpuTimes, memInfo, error)
	processes() ([]procLink, error)
	stats(pid uint32) (procStats, bool) // false if it has exited or can't be opened
}

// ---- sampler ----

// HistoryLength is how many readings the graphs keep: 5 minutes at 3 s.
const HistoryLength = 100

// Sampler reads the machine every interval. roots says which process each
// app was started as (app name -> PID), from the supervisor's statuses.
type Sampler struct {
	plat  platform
	roots func() map[string]uint32
	every time.Duration
	cores int
	now   func() time.Time

	mu      sync.Mutex
	prevSys cpuTimes
	prevAt  time.Time
	prevCPU map[procKey]time.Duration
	latest  Snapshot
	have    bool
	history []Point
}

type procKey struct {
	pid     uint32
	created time.Time
}

func NewSampler(roots func() map[string]uint32, every time.Duration) *Sampler {
	return newSampler(newPlatform(), roots, every)
}

func newSampler(p platform, roots func() map[string]uint32, every time.Duration) *Sampler {
	return &Sampler{plat: p, roots: roots, every: every, cores: runtime.NumCPU(), now: time.Now}
}

// Run samples until ctx is done. On an unsupported OS it returns at once.
func (s *Sampler) Run(ctx context.Context) {
	if _, _, err := s.plat.system(); errors.Is(err, ErrUnsupported) {
		return
	}
	t := time.NewTicker(s.every)
	defer t.Stop()
	for {
		s.sample()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Latest returns the newest snapshot, or false before the second reading
// (CPU % needs two readings to compare).
func (s *Sampler) Latest() (Snapshot, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.have {
		return Snapshot{}, false
	}
	snap := s.latest
	snap.History = append([]Point(nil), s.history...)
	return snap, true
}

func (s *Sampler) sample() {
	sys, mem, err := s.plat.system()
	if err != nil {
		return
	}
	links, err := s.plat.processes()
	if err != nil {
		return
	}
	at := s.now()

	s.mu.Lock()
	defer s.mu.Unlock()

	first := s.prevAt.IsZero()
	wall := at.Sub(s.prevAt)
	snap := Snapshot{
		At: at, Cores: s.cores, MemTotalBytes: mem.Total, Apps: map[string]AppUsage{},
	}
	if mem.Total >= mem.Available {
		snap.MemUsedBytes = mem.Total - mem.Available
	}
	if !first {
		snap.CPUPercent = systemCPU(s.prevSys, sys)
	}

	children := map[uint32][]uint32{}
	for _, l := range links {
		if l.PID != l.PPID {
			children[l.PPID] = append(children[l.PPID], l.PID)
		}
	}
	cpuNow := map[procKey]time.Duration{}
	for name, root := range s.roots() {
		if root == 0 {
			continue
		}
		tree := collectTree(root, children, s.plat.stats)
		var u AppUsage
		var delta time.Duration
		for _, p := range tree {
			k := procKey{p.pid, p.stats.Created}
			cpuNow[k] = p.stats.CPU
			u.MemoryBytes += p.stats.Memory
			u.Processes++
			if prev, ok := s.prevCPU[k]; ok {
				delta += max(p.stats.CPU-prev, 0)
			} else if !first && p.stats.Created.After(s.prevAt) {
				delta += p.stats.CPU // born since the last reading: all of it is new
			}
		}
		if !first && wall > 0 {
			u.CPUPercent = clampPercent(float64(delta) / (float64(wall) * float64(s.cores)) * 100)
		}
		snap.Apps[name] = u
	}

	s.prevSys, s.prevAt, s.prevCPU = sys, at, cpuNow
	if first {
		return // nothing to compare against yet
	}
	s.latest, s.have = snap, true
	memPct := 0.0
	if mem.Total > 0 {
		memPct = float64(snap.MemUsedBytes) / float64(mem.Total) * 100
	}
	s.history = append(s.history, Point{At: at, CPUPercent: snap.CPUPercent, MemPercent: memPct})
	if len(s.history) > HistoryLength {
		s.history = s.history[len(s.history)-HistoryLength:]
	}
}

// systemCPU is the share of time the CPUs weren't idle between two readings.
// GetSystemTimes counts idle time inside kernel time, so busy = total - idle.
func systemCPU(prev, cur cpuTimes) float64 {
	total := (cur.Kernel - prev.Kernel) + (cur.User - prev.User)
	idle := cur.Idle - prev.Idle
	if total <= 0 {
		return 0
	}
	return clampPercent(float64(total-idle) / float64(total) * 100)
}

type treeProc struct {
	pid   uint32
	stats procStats
}

// collectTree walks from root to every descendant. A child must have been
// created after its parent: Windows reuses PIDs, and an old process whose
// parent PID now belongs to someone else would otherwise be counted.
func collectTree(root uint32, children map[uint32][]uint32, stats func(uint32) (procStats, bool)) []treeProc {
	rs, ok := stats(root)
	if !ok {
		return nil
	}
	out := []treeProc{{root, rs}}
	seen := map[uint32]bool{root: true}
	for i := 0; i < len(out); i++ {
		parent := out[i]
		for _, c := range children[parent.pid] {
			if seen[c] {
				continue
			}
			cs, ok := stats(c)
			if !ok || cs.Created.Before(parent.stats.Created) {
				continue
			}
			seen[c] = true
			out = append(out, treeProc{c, cs})
		}
	}
	return out
}

func clampPercent(p float64) float64 {
	return min(max(p, 0), 100)
}
