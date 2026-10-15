package resources

// GPU readings, for the Local AI models: a 30B model lives in the GPU's
// memory (VRAM), and when it doesn't fit, Ollama puts part of it in system
// RAM and answers get much slower. NVIDIA's driver ships nvidia-smi, which
// prints one CSV line per GPU; that's the whole interface here. No NVIDIA
// GPU (or no driver) just means no GPU readings.

import (
	"context"
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// GPU is one graphics card's reading.
type GPU struct {
	Name           string  `json:"name"`
	UtilPercent    float64 `json:"util_percent"`
	MemUsedBytes   uint64  `json:"mem_used_bytes"`
	MemTotalBytes  uint64  `json:"mem_total_bytes"`
	TemperatureC   float64 `json:"temperature_c"`
	PowerWatts     float64 `json:"power_watts,omitempty"`
	PowerLimitWatt float64 `json:"power_limit_watts,omitempty"`
}

const smiQuery = "name,utilization.gpu,memory.used,memory.total,temperature.gpu,power.draw,power.limit"

// errNoGPU means nvidia-smi isn't there: stop asking.
var errNoGPU = errors.New("no NVIDIA GPU (nvidia-smi not found)")

// readGPUs runs nvidia-smi once (hidden: no console window flashes on Windows).
func readGPUs() ([]GPU, error) {
	path, err := exec.LookPath("nvidia-smi")
	if err != nil {
		return nil, errNoGPU
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--query-gpu="+smiQuery, "--format=csv,noheader,nounits")
	hideWindow(cmd)
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	return parseSMI(string(out)), nil
}

// parseSMI reads nvidia-smi's CSV (MiB for memory, °C, W). Fields it can't
// read ("[N/A]" on some cards) are left at zero; malformed lines are skipped.
func parseSMI(out string) []GPU {
	var gpus []GPU
	for _, line := range strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n") {
		f := strings.Split(line, ",")
		if len(f) < 5 {
			continue
		}
		for i := range f {
			f[i] = strings.TrimSpace(f[i])
		}
		num := func(i int) float64 {
			if i >= len(f) {
				return 0
			}
			v, err := strconv.ParseFloat(f[i], 64)
			if err != nil {
				return 0
			}
			return v
		}
		const mib = 1 << 20
		g := GPU{
			Name:           f[0],
			UtilPercent:    clampPercent(num(1)),
			MemUsedBytes:   uint64(num(2) * mib),
			MemTotalBytes:  uint64(num(3) * mib),
			TemperatureC:   num(4),
			PowerWatts:     num(5),
			PowerLimitWatt: num(6),
		}
		if g.Name == "" || g.MemTotalBytes == 0 {
			continue
		}
		gpus = append(gpus, g)
	}
	return gpus
}

// VRAMPercent is how full the card's memory is.
func (g GPU) VRAMPercent() float64 {
	if g.MemTotalBytes == 0 {
		return 0
	}
	return clampPercent(float64(g.MemUsedBytes) / float64(g.MemTotalBytes) * 100)
}
