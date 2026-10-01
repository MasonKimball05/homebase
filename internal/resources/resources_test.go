package resources

import (
	"math"
	"testing"
	"time"
)

// fake is a platform whose numbers the test sets between readings.
type fake struct {
	sys   cpuTimes
	mem   memInfo
	links []procLink
	procs map[uint32]procStats
}

func (f *fake) system() (cpuTimes, memInfo, error) { return f.sys, f.mem, nil }
func (f *fake) processes() ([]procLink, error)     { return f.links, nil }
func (f *fake) stats(pid uint32) (procStats, bool) {
	s, ok := f.procs[pid]
	return s, ok
}

var t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// newTestSampler: 4 cores, a clock the test advances, and one app "web" whose
// root is PID 100.
func newTestSampler(f *fake) (*Sampler, *time.Time) {
	now := t0
	s := newSampler(f, func() map[string]uint32 { return map[string]uint32{"web": 100} }, time.Second)
	s.cores = 4
	s.now = func() time.Time { return now }
	return s, &now
}

func near(a, b float64) bool { return math.Abs(a-b) < 0.01 }

func TestNoSnapshotUntilTwoReadings(t *testing.T) {
	f := &fake{mem: memInfo{Total: 100, Available: 40}, procs: map[uint32]procStats{}}
	s, _ := newTestSampler(f)
	s.sample()
	if _, ok := s.Latest(); ok {
		t.Fatal("a snapshot after one reading: CPU % needs two")
	}
}

func TestSystemCPUAndMemory(t *testing.T) {
	f := &fake{mem: memInfo{Total: 16 << 30, Available: 4 << 30}, procs: map[uint32]procStats{}}
	s, now := newTestSampler(f)
	s.sample()
	// 10 s of CPU time passed (kernel includes idle), 7.5 s of it idle: 25% busy.
	f.sys = cpuTimes{Idle: 7500 * time.Millisecond, Kernel: 8 * time.Second, User: 2 * time.Second}
	*now = now.Add(time.Second)
	s.sample()
	snap, ok := s.Latest()
	if !ok || !near(snap.CPUPercent, 25) {
		t.Fatalf("cpu = %v, ok = %v; want 25", snap.CPUPercent, ok)
	}
	if snap.MemUsedBytes != 12<<30 || snap.MemTotalBytes != 16<<30 {
		t.Fatalf("mem used %d of %d", snap.MemUsedBytes, snap.MemTotalBytes)
	}
	if len(snap.History) != 1 || !near(snap.History[0].MemPercent, 75) {
		t.Fatalf("history %+v", snap.History)
	}
}

func TestAppUsageSumsTheProcessTree(t *testing.T) {
	f := &fake{
		mem:   memInfo{Total: 1, Available: 1},
		links: []procLink{{100, 1}, {101, 100}, {102, 101}, {200, 1}},
		procs: map[uint32]procStats{
			100: {Created: t0.Add(-time.Hour), CPU: 10 * time.Second, Memory: 100 << 20},
			101: {Created: t0.Add(-time.Minute), CPU: 5 * time.Second, Memory: 50 << 20},
			102: {Created: t0.Add(-time.Second), CPU: 1 * time.Second, Memory: 10 << 20},
			200: {Created: t0.Add(-time.Hour), CPU: 99 * time.Second, Memory: 999 << 20}, // not ours
		},
	}
	s, now := newTestSampler(f)
	s.sample()
	// Over 2 s of wall time on 4 cores (8 core-seconds), the tree used 2 s: 25%.
	f.procs[100] = procStats{Created: t0.Add(-time.Hour), CPU: 11 * time.Second, Memory: 100 << 20}
	f.procs[101] = procStats{Created: t0.Add(-time.Minute), CPU: 6 * time.Second, Memory: 50 << 20}
	*now = now.Add(2 * time.Second)
	s.sample()

	snap, _ := s.Latest()
	web := snap.Apps["web"]
	if web.Processes != 3 || web.MemoryBytes != 160<<20 || !near(web.CPUPercent, 25) {
		t.Fatalf("web = %+v; want 3 processes, 160 MiB, 25%%", web)
	}
}

func TestReusedPIDIsNotCountedAsAChild(t *testing.T) {
	// PID 300 says its parent is 100, but it's older than 100: its real parent
	// died and Windows gave the PID to our app.
	f := &fake{
		mem:   memInfo{Total: 1, Available: 1},
		links: []procLink{{100, 1}, {300, 100}},
		procs: map[uint32]procStats{
			100: {Created: t0.Add(-time.Minute), Memory: 10},
			300: {Created: t0.Add(-time.Hour), Memory: 1000},
		},
	}
	s, now := newTestSampler(f)
	s.sample()
	*now = now.Add(time.Second)
	s.sample()
	snap, _ := s.Latest()
	if got := snap.Apps["web"]; got.Processes != 1 || got.MemoryBytes != 10 {
		t.Fatalf("web = %+v; the older 'child' must be ignored", got)
	}
}

func TestProcessBornBetweenReadingsCountsAllItsCPU(t *testing.T) {
	f := &fake{
		mem:   memInfo{Total: 1, Available: 1},
		links: []procLink{{100, 1}},
		procs: map[uint32]procStats{100: {Created: t0.Add(-time.Hour), CPU: time.Second}},
	}
	s, now := newTestSampler(f)
	s.sample()
	// A child appears after the first reading and has used 4 s already.
	f.links = append(f.links, procLink{101, 100})
	f.procs[101] = procStats{Created: t0.Add(500 * time.Millisecond), CPU: 4 * time.Second}
	*now = now.Add(4 * time.Second) // 16 core-seconds available
	s.sample()
	snap, _ := s.Latest()
	if got := snap.Apps["web"]; !near(got.CPUPercent, 25) {
		t.Fatalf("cpu = %v; want 25 (4 s of 16)", got.CPUPercent)
	}
}

func TestStoppedAppReportsNothing(t *testing.T) {
	f := &fake{mem: memInfo{Total: 1, Available: 1}, procs: map[uint32]procStats{}}
	s, now := newTestSampler(f)
	s.roots = func() map[string]uint32 { return map[string]uint32{"web": 0, "gone": 555} }
	s.sample()
	*now = now.Add(time.Second)
	s.sample()
	snap, _ := s.Latest()
	if u, ok := snap.Apps["web"]; ok {
		t.Fatalf("a stopped app (PID 0) got usage %+v", u)
	}
	if u := snap.Apps["gone"]; u.Processes != 0 || u.MemoryBytes != 0 {
		t.Fatalf("an exited process got usage %+v", u)
	}
}

func TestHistoryIsCapped(t *testing.T) {
	f := &fake{mem: memInfo{Total: 1, Available: 1}, procs: map[uint32]procStats{}}
	s, now := newTestSampler(f)
	for range HistoryLength + 25 {
		s.sample()
		*now = now.Add(time.Second)
	}
	snap, _ := s.Latest()
	if len(snap.History) != HistoryLength {
		t.Fatalf("history length %d, want %d", len(snap.History), HistoryLength)
	}
	if !snap.History[len(snap.History)-1].At.After(snap.History[0].At) {
		t.Fatal("history should run oldest first")
	}
}

func TestUnsupportedPlatformReturnsAtOnce(t *testing.T) {
	s := newSampler(unsupportedForTest{}, func() map[string]uint32 { return nil }, time.Hour)
	done := make(chan struct{})
	go func() { s.Run(t.Context()); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run should return immediately when unsupported")
	}
}

type unsupportedForTest struct{}

func (unsupportedForTest) system() (cpuTimes, memInfo, error) {
	return cpuTimes{}, memInfo{}, ErrUnsupported
}
func (unsupportedForTest) processes() ([]procLink, error) { return nil, ErrUnsupported }
func (unsupportedForTest) stats(uint32) (procStats, bool) { return procStats{}, false }
