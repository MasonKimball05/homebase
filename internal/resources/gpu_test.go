package resources

import (
	"testing"
	"time"
)

func TestParseSMI(t *testing.T) {
	out := "NVIDIA GeForce RTX 5070, 87, 10342, 12227, 71, 214.53, 250.00\r\n" +
		"Some Old Card, 3, 512, 4096, 40, [N/A], [N/A]\r\n" +
		"garbage line\r\n"
	gpus := parseSMI(out)
	if len(gpus) != 2 {
		t.Fatalf("got %d GPUs, want 2: %+v", len(gpus), gpus)
	}
	g := gpus[0]
	if g.Name != "NVIDIA GeForce RTX 5070" || g.UtilPercent != 87 || g.TemperatureC != 71 || g.PowerWatts != 214.53 {
		t.Fatalf("first GPU: %+v", g)
	}
	if g.MemUsedBytes != 10342<<20 || g.MemTotalBytes != 12227<<20 || !near(g.VRAMPercent(), 84.58) {
		t.Fatalf("memory %d / %d (%.2f%%)", g.MemUsedBytes, g.MemTotalBytes, g.VRAMPercent())
	}
	if gpus[1].PowerWatts != 0 || gpus[1].UtilPercent != 3 {
		t.Fatalf("[N/A] fields should be zero: %+v", gpus[1])
	}
}

func TestSamplerRecordsGPU(t *testing.T) {
	f := &fake{mem: memInfo{Total: 100, Available: 50}, procs: map[uint32]procStats{}}
	s, now := newTestSampler(f)
	calls := 0
	s.gpu = func() ([]GPU, error) {
		calls++
		return []GPU{{Name: "RTX", UtilPercent: 90, MemUsedBytes: 11, MemTotalBytes: 12}}, nil
	}
	s.sample()
	*now = now.Add(time.Second)
	s.sample()
	snap, ok := s.Latest()
	if !ok || len(snap.GPUs) != 1 || snap.GPUs[0].UtilPercent != 90 {
		t.Fatalf("snapshot GPUs: %+v", snap.GPUs)
	}
	last := snap.History[len(snap.History)-1]
	if last.GPUPercent == nil || *last.GPUPercent != 90 || last.VRAMPercent == nil || !near(*last.VRAMPercent, 91.67) {
		t.Fatalf("history point: %+v", last)
	}
	if calls != 2 {
		t.Fatalf("nvidia-smi read %d times, want once per sample", calls)
	}
}

func TestNoGPUStopsAsking(t *testing.T) {
	f := &fake{mem: memInfo{Total: 100, Available: 50}, procs: map[uint32]procStats{}}
	s, now := newTestSampler(f)
	calls := 0
	s.gpu = func() ([]GPU, error) { calls++; return nil, errNoGPU }
	for i := 0; i < 3; i++ {
		s.sample()
		*now = now.Add(time.Second)
	}
	if calls != 1 {
		t.Fatalf("asked %d times after learning there's no GPU", calls)
	}
	snap, _ := s.Latest()
	if snap.GPUs != nil || snap.History[len(snap.History)-1].GPUPercent != nil {
		t.Fatalf("GPU fields without a GPU: %+v", snap)
	}
}
