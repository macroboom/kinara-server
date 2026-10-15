package metrics

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// Point is one reading. Rates are averages over the reading's interval.
type Point struct {
	T int64 `json:"t"` // Unix seconds
	// CPU is the whole computer's processor use, 0 to 100 (ffmpeg included).
	CPU float64 `json:"cpu"`
	// Mem is memory in use, bytes.
	Mem uint64 `json:"mem"`
	// Out and In are traffic served and received, megabits per second.
	Out float64 `json:"out"`
	In  float64 `json:"in"`
	// Direct and Converting count devices playing each way.
	Direct     float64 `json:"direct"`
	Converting float64 `json:"converting"`
	// NVIDIA card: overall, encoder and decoder busy (0 to 100), memory used.
	GPU    float64 `json:"gpu"`
	Enc    float64 `json:"enc"`
	Dec    float64 `json:"dec"`
	GPUMem uint64  `json:"gpuMem"`
	// GPUOn: the card was read for this point. It's only read while
	// someone is looking at the metrics or something is converting.
	GPUOn bool `json:"gpuOn,omitempty"`
}

// Series is a range of readings for the page.
type Series struct {
	// Step is the seconds between points.
	Step   int     `json:"step"`
	Points []Point `json:"points"`
	// What this computer can report.
	HasCPU    bool    `json:"hasCpu"`
	HasMemory bool    `json:"hasMemory"`
	MemTotal  uint64  `json:"memTotal"`
	GPU       *GPU    `json:"gpu,omitempty"`
	Uptime    float64 `json:"uptimeSec"`
}

const (
	fineStep    = 5 * time.Second
	fineKeep    = 720 // one hour
	coarseStep  = time.Minute
	coarseKeep  = 1440 // a day
	gpuInterval = 5 * time.Second
	// gpuWatch is how long the card keeps being read after the metrics
	// were last asked for.
	gpuWatch = 5 * time.Minute
	// gpuFresh is how old a card reading can be and still count for a point
	// (a reading takes about a second).
	gpuFresh = 2*gpuInterval + 2*time.Second
)

// Collector takes a reading every few seconds and keeps the last hour at
// full detail and the last day by the minute, in memory.
type Collector struct {
	// Streams counts devices playing directly and with conversion now.
	Streams func() (direct, converting int)

	bytesOut, bytesIn atomic.Uint64

	mu       sync.Mutex
	fine     []Point
	coarse   []Point
	acc      []Point // this minute's readings
	hasCPU   bool
	hasMem   bool
	memTotal uint64
	gpu      *GPU
	gpuAt    time.Time // when gpu was last read
	viewed   time.Time // when readings were last asked for
	started  time.Time
	smi      string
	// wake starts a card reading now, when the page opens while idle.
	wake chan struct{}
}

// AddOut counts bytes sent to clients.
func (c *Collector) AddOut(n int64) {
	if n > 0 {
		c.bytesOut.Add(uint64(n))
	}
}

// AddIn counts bytes received from clients.
func (c *Collector) AddIn(n int64) {
	if n > 0 {
		c.bytesIn.Add(uint64(n))
	}
}

// Run takes readings until ctx ends.
func (c *Collector) Run(ctx context.Context) {
	c.mu.Lock()
	c.started = time.Now()
	c.smi = findSMI()
	c.wake = make(chan struct{}, 1)
	c.mu.Unlock()
	if c.smi != "" {
		go c.runGPU(ctx)
	}
	prevCPU, cpuOK := readCPU()
	prevOut, prevIn := c.bytesOut.Load(), c.bytesIn.Load()
	prevAt := time.Now()
	tick := time.NewTicker(fineStep)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			p := Point{T: now.Unix()}
			if cur, ok := readCPU(); ok && cpuOK {
				p.CPU = round1(100 * cpuShare(prevCPU, cur))
				prevCPU = cur
			} else {
				prevCPU, cpuOK = cur, ok
			}
			mem, memOK := readMemory()
			p.Mem = mem.Used
			secs := now.Sub(prevAt).Seconds()
			out, in := c.bytesOut.Load(), c.bytesIn.Load()
			if secs > 0 {
				p.Out = round1(float64(out-prevOut) * 8 / secs / 1e6)
				p.In = round1(float64(in-prevIn) * 8 / secs / 1e6)
			}
			prevOut, prevIn, prevAt = out, in, now
			if c.Streams != nil {
				d, cv := c.Streams()
				p.Direct, p.Converting = float64(d), float64(cv)
			}
			c.mu.Lock()
			if c.gpu != nil && now.Sub(c.gpuAt) < gpuFresh {
				p.GPU, p.Enc, p.Dec, p.GPUMem = c.gpu.Util, c.gpu.Enc, c.gpu.Dec, c.gpu.MemUsed
				p.GPUOn = true
			}
			c.hasCPU, c.hasMem = cpuOK, memOK
			if memOK {
				c.memTotal = mem.Total
			}
			c.add(p)
			c.mu.Unlock()
		}
	}
}

// runGPU reads the card every few seconds, but only while it's wanted:
// nvidia-smi is a separate program, and starting it twice every 5 seconds
// around the clock is work for a chart nobody is looking at. The first
// reading always happens, so the page knows the card is there.
func (c *Collector) runGPU(ctx context.Context) {
	tick := time.NewTicker(gpuInterval)
	defer tick.Stop()
	misses := 0
	first := true
	for {
		if first || c.gpuWanted(time.Now()) {
			first = false
			g, ok := readGPU(ctx, c.smi)
			c.mu.Lock()
			if ok {
				c.gpu, c.gpuAt, misses = &g, time.Now(), 0
			} else if misses++; misses >= 3 {
				c.gpu = nil
			}
			c.mu.Unlock()
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-c.wake:
		}
	}
}

// gpuWanted: someone asked for the readings lately, or a stream is
// converting (where the card's load matters, and the hour chart should
// have it when someone looks).
func (c *Collector) gpuWanted(now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.viewed.IsZero() && now.Sub(c.viewed) < gpuWatch {
		return true
	}
	return len(c.fine) > 0 && c.fine[len(c.fine)-1].Converting > 0
}

// add stores a reading (mu held): in the hour list, and folded into the
// day list once a minute is complete.
func (c *Collector) add(p Point) {
	c.fine = appendKeep(c.fine, p, fineKeep)
	if len(c.acc) > 0 && p.T/60 != c.acc[0].T/60 {
		c.coarse = appendKeep(c.coarse, average(c.acc), coarseKeep)
		c.acc = c.acc[:0]
	}
	c.acc = append(c.acc, p)
}

func appendKeep(s []Point, p Point, keep int) []Point {
	s = append(s, p)
	if len(s) > keep {
		s = append(s[:0], s[len(s)-keep:]...)
	}
	return s
}

// average folds a minute of readings into one at the minute's start.
// Traffic and load average; stream counts take the peak, so a short
// stream still shows.
func average(ps []Point) Point {
	out := Point{T: ps[0].T / 60 * 60}
	n := float64(len(ps))
	var mem, gmem, gn float64
	for _, p := range ps {
		if p.GPUOn {
			gn++
		}
	}
	for _, p := range ps {
		out.CPU += p.CPU / n
		mem += float64(p.Mem) / n
		out.Out += p.Out / n
		out.In += p.In / n
		// The card only over the readings that have it.
		if p.GPUOn {
			out.GPU += p.GPU / gn
			out.Enc += p.Enc / gn
			out.Dec += p.Dec / gn
			gmem += float64(p.GPUMem) / gn
		}
		out.Direct = max(out.Direct, p.Direct)
		out.Converting = max(out.Converting, p.Converting)
	}
	out.Mem, out.GPUMem = uint64(mem), uint64(gmem)
	out.GPUOn = gn > 0
	out.CPU, out.Out, out.In = round1(out.CPU), round1(out.Out), round1(out.In)
	out.GPU, out.Enc, out.Dec = round1(out.GPU), round1(out.Enc), round1(out.Dec)
	return out
}

func round1(v float64) float64 { return float64(int64(v*10+0.5)) / 10 }

// Range returns the last hour ("1h", every 5 s) or day ("24h", by the
// minute). Asking counts as looking: the graphics card is read for the
// next few minutes.
func (c *Collector) Range(r string) Series {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if c.viewed.IsZero() || now.Sub(c.viewed) >= gpuWatch {
		// Idle until now: take a card reading straight away.
		select {
		case c.wake <- struct{}{}:
		default:
		}
	}
	c.viewed = now
	s := Series{HasCPU: c.hasCPU, HasMemory: c.hasMem, MemTotal: c.memTotal}
	if !c.started.IsZero() {
		s.Uptime = time.Since(c.started).Seconds()
	}
	if c.gpu != nil {
		g := *c.gpu
		s.GPU = &g
	}
	if r == "24h" {
		s.Step = int(coarseStep.Seconds())
		s.Points = append([]Point{}, c.coarse...)
		if len(c.acc) > 0 {
			s.Points = append(s.Points, average(c.acc))
		}
	} else {
		s.Step = int(fineStep.Seconds())
		s.Points = append([]Point{}, c.fine...)
	}
	return s
}
