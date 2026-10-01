//go:build windows

package resources

import (
	"os"
	"testing"
	"time"
)

// These call the real Windows APIs, so they only run on Windows. They check
// the numbers are plausible, which catches a wrong struct layout or call.

func TestRealSystemReading(t *testing.T) {
	p := newPlatform()
	sys, mem, err := p.system()
	if err != nil {
		t.Fatal(err)
	}
	if mem.Total < 1<<30 || mem.Available == 0 || mem.Available > mem.Total {
		t.Fatalf("implausible memory: %+v", mem)
	}
	if sys.Kernel <= 0 || sys.User <= 0 || sys.Idle <= 0 || sys.Idle > sys.Kernel {
		t.Fatalf("implausible CPU times: %+v", sys)
	}
	t.Logf("memory: %.1f of %.1f GB used", float64(mem.Total-mem.Available)/(1<<30), float64(mem.Total)/(1<<30))
}

func TestRealProcessesIncludeThisOne(t *testing.T) {
	p := newPlatform()
	links, err := p.processes()
	if err != nil {
		t.Fatal(err)
	}
	me := uint32(os.Getpid())
	found := false
	for _, l := range links {
		if l.PID == me {
			found = true
			if l.PPID != uint32(os.Getppid()) {
				t.Errorf("parent of %d = %d, want %d", me, l.PPID, os.Getppid())
			}
		}
	}
	if !found || len(links) < 20 {
		t.Fatalf("found self: %v, %d processes", found, len(links))
	}
	st, ok := p.stats(me)
	if !ok || st.Memory < 1<<20 || st.Created.After(time.Now()) || time.Since(st.Created) > time.Hour {
		t.Fatalf("implausible stats for this process: %+v ok=%v", st, ok)
	}
	t.Logf("%d processes; this one: %.1f MB, started %s ago", len(links), float64(st.Memory)/(1<<20), time.Since(st.Created).Round(time.Millisecond))
}

func TestRealSamplerMeasuresBusyWork(t *testing.T) {
	me := uint32(os.Getpid())
	s := NewSampler(func() map[string]uint32 { return map[string]uint32{"me": me} }, time.Second)
	s.sample()
	deadline := time.Now().Add(1500 * time.Millisecond)
	for x := 0; time.Now().Before(deadline); x++ { // keep one core busy
		_ = x * x
	}
	s.sample()
	snap, ok := s.Latest()
	if !ok {
		t.Fatal("no snapshot after two readings")
	}
	me1 := snap.Apps["me"]
	// One busy core of N shows as about 100/N percent.
	want := 100 / float64(snap.Cores)
	if me1.CPUPercent < want*0.5 || me1.CPUPercent > want*1.6 {
		t.Fatalf("CPU %.1f%%, want about %.1f%% (one of %d cores)", me1.CPUPercent, want, snap.Cores)
	}
	t.Logf("system CPU %.1f%%, this process %.1f%% (one busy core of %d), %.1f MB", snap.CPUPercent, me1.CPUPercent, snap.Cores, float64(me1.MemoryBytes)/(1<<20))
}
