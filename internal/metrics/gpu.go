package metrics

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// GPU is an NVIDIA card's state, from nvidia-smi.
type GPU struct {
	Name string `json:"name"`
	// Percent busy: the card overall, its video encoder (NVENC) and its
	// video decoder (NVDEC).
	Util float64 `json:"util"`
	Enc  float64 `json:"enc"`
	Dec  float64 `json:"dec"`
	// Memory in bytes.
	MemUsed  uint64  `json:"memUsed"`
	MemTotal uint64  `json:"memTotal"`
	TempC    float64 `json:"tempC"`
	PowerW   float64 `json:"powerW"`
	// Sessions is how many encodes NVENC is running.
	Sessions int `json:"sessions"`
}

// findSMI finds nvidia-smi: on PATH, or where the Windows driver puts it.
func findSMI() string {
	if p, err := exec.LookPath("nvidia-smi"); err == nil {
		return p
	}
	if runtime.GOOS == "windows" {
		for _, p := range []string{
			filepath.Join(os.Getenv("SystemRoot"), "System32", "nvidia-smi.exe"),
			`C:\Program Files\NVIDIA Corporation\NVSMI\nvidia-smi.exe`,
		} {
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}
	return ""
}

// readGPU asks nvidia-smi for the first card's state. ok is false without
// an NVIDIA card (or driver).
func readGPU(ctx context.Context, smi string) (GPU, bool) {
	if smi == "" {
		return GPU{}, false
	}
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	q := exec.CommandContext(ctx, smi, "--query-gpu=name,utilization.gpu,memory.used,memory.total,temperature.gpu,power.draw,encoder.stats.sessionCount",
		"--format=csv,noheader,nounits", "-i", "0")
	hideWindow(q)
	out, err := q.Output()
	if err != nil {
		return GPU{}, false
	}
	g, ok := parseQuery(string(out))
	if !ok {
		return GPU{}, false
	}
	// Encoder and decoder load come from one sample of dmon (about a second).
	d := exec.CommandContext(ctx, smi, "dmon", "-c", "1", "-s", "u", "-i", "0")
	hideWindow(d)
	if out, err := d.Output(); err == nil {
		g.Enc, g.Dec = parseDmon(out)
	}
	return g, true
}

func num(s string) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0 // "[N/A]" on some cards
	}
	return v
}

// parseQuery reads "NVIDIA GeForce RTX 4070 Ti, 37, 2120, 12282, 51, 63.20, 2".
func parseQuery(line string) (GPU, bool) {
	f := strings.Split(strings.TrimSpace(strings.SplitN(line, "\n", 2)[0]), ",")
	if len(f) < 6 {
		return GPU{}, false
	}
	g := GPU{
		Name:     strings.TrimSpace(f[0]),
		Util:     num(f[1]),
		MemUsed:  uint64(num(f[2])) << 20,
		MemTotal: uint64(num(f[3])) << 20,
		TempC:    num(f[4]),
		PowerW:   num(f[5]),
	}
	if len(f) > 6 {
		g.Sessions = int(num(f[6]))
	}
	return g, g.Name != ""
}

// parseDmon reads the enc and dec columns from nvidia-smi dmon, whose
// columns differ between drivers, so it goes by the header's names:
//
//	# gpu    sm   mem   enc   dec   jpg   ofa
//	# Idx     %     %     %     %     %     %
//	    0     3     2    41     8     0     0
func parseDmon(out []byte) (enc, dec float64) {
	var names []string
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "#") {
			f := strings.Fields(strings.TrimPrefix(line, "#"))
			if len(f) > 0 && f[0] == "gpu" {
				names = f
			}
			continue
		}
		f := strings.Fields(line)
		if names == nil || len(f) != len(names) {
			continue
		}
		for i, n := range names {
			switch n {
			case "enc":
				enc = num(f[i])
			case "dec":
				dec = num(f[i])
			}
		}
		return enc, dec
	}
	return 0, 0
}
