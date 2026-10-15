package metrics

import (
	"bytes"
	"io"
	"log/slog"
	"testing"
	"time"
)

func TestParseGPU(t *testing.T) {
	g, ok := parseQuery("NVIDIA GeForce RTX 4070 Ti, 37, 2120, 12282, 51, 63.20, 2\n")
	if !ok || g.Name != "NVIDIA GeForce RTX 4070 Ti" || g.Util != 37 || g.MemUsed != 2120<<20 || g.MemTotal != 12282<<20 || g.TempC != 51 || g.PowerW != 63.2 || g.Sessions != 2 {
		t.Fatalf("%+v %v", g, ok)
	}
	if g, ok := parseQuery("Quadro P400, [N/A], 100, 2000, 40, [N/A]"); !ok || g.Util != 0 || g.PowerW != 0 {
		t.Fatalf("N/A fields: %+v", g)
	}
	enc, dec := parseDmon([]byte("# gpu    sm   mem   enc   dec   jpg   ofa\n# Idx     %     %     %     %     %     %\n    0     3     2    41     8     0     0\n"))
	if enc != 41 || dec != 8 {
		t.Fatalf("dmon %v %v", enc, dec)
	}
	enc, dec = parseDmon([]byte("# gpu   sm  mem  enc  dec\n# Idx    %    %    %    %\n    0   10    5   20   30\n"))
	if enc != 20 || dec != 30 {
		t.Fatalf("older dmon %v %v", enc, dec)
	}
}

func TestCPUShare(t *testing.T) {
	if s := cpuShare(cpuTimes{idle: 100, total: 200}, cpuTimes{idle: 150, total: 300}); s != 0.5 {
		t.Fatalf("share %v", s)
	}
	if s := cpuShare(cpuTimes{total: 5}, cpuTimes{total: 5}); s != 0 {
		t.Fatalf("no time passed %v", s)
	}
}

func TestMinuteRollup(t *testing.T) {
	c := &Collector{}
	base := int64(1_800_000_000) / 60 * 60
	for i := int64(0); i < 12; i++ {
		c.add(Point{T: base + i*5, CPU: float64(i), Converting: float64(i % 3), Out: 10})
	}
	c.add(Point{T: base + 60, CPU: 50})
	if len(c.coarse) != 1 {
		t.Fatalf("coarse %d", len(c.coarse))
	}
	m := c.coarse[0]
	if m.T != base || m.CPU != 5.5 || m.Converting != 2 || m.Out != 10 {
		t.Fatalf("minute %+v", m)
	}
	s := c.Range("24h")
	if s.Step != 60 || len(s.Points) != 2 {
		t.Fatalf("day range %+v", s)
	}
	if h := c.Range("1h"); h.Step != 5 || len(h.Points) != 13 {
		t.Fatalf("hour range %d", len(h.Points))
	}
	for i := 0; i < fineKeep+50; i++ {
		c.add(Point{T: base + 120 + int64(i)*5})
	}
	if len(c.fine) != fineKeep {
		t.Fatalf("hour kept %d", len(c.fine))
	}
}

func TestHTTPStats(t *testing.T) {
	var h HTTPStats
	for i := 0; i < 100; i++ {
		h.Record("GET /api/v1/home", 200, time.Duration(i%10+1)*time.Millisecond, 1000)
	}
	h.Record("GET /api/v1/home", 500, 3*time.Second, 10)
	h.Record("GET /api/v1/transcode/{id}", 200, time.Hour, 5e9)
	rs := h.Routes()
	if len(rs) != 2 || rs[0].Route != "GET /api/v1/home" || rs[0].Count != 101 || rs[0].ServerErr != 1 {
		t.Fatalf("%+v", rs)
	}
	if rs[0].P50Ms != 10 || rs[0].P95Ms != 10 {
		t.Fatalf("percentiles %+v", rs[0])
	}
	if !rs[1].Streaming || rs[1].P95Ms != 0 || rs[1].Bytes != 5e9 {
		t.Fatalf("streaming route %+v", rs[1])
	}
}

func TestLogRing(t *testing.T) {
	var buf bytes.Buffer
	ring := &LogRing{Keep: 2}
	log := slog.New(ring.Handler(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError})))
	log.Info("fine")
	log.With("file", "a.mkv").Warn("transcode failed", "err", "boom")
	log.WithGroup("scan").Error("bad", "path", "/x")
	log.Warn("third")
	es := ring.Entries()
	if len(es) != 2 || es[0].Message != "third" || es[1].Message != "bad" || es[1].Attrs != "scan.path=/x" {
		t.Fatalf("%+v", es)
	}
	// The wrapped handler still only writes what its own level allows.
	if bytes.Contains(buf.Bytes(), []byte("transcode failed")) || !bytes.Contains(buf.Bytes(), []byte("bad")) {
		t.Fatalf("inner handler got %q", buf.String())
	}
	_ = io.Discard
}

func TestDiskUsage(t *testing.T) {
	d, ok := DiskUsage(t.TempDir())
	if !ok || d.Total == 0 || d.Free > d.Total {
		t.Fatalf("%+v %v", d, ok)
	}
}

func TestGPUOnlyWhenWanted(t *testing.T) {
	c := &Collector{}
	now := time.Now()
	if c.gpuWanted(now) {
		t.Fatal("idle server should not read the card")
	}
	c.Range("1h")
	if !c.gpuWanted(now) {
		t.Fatal("asking for readings should start card readings")
	}
	if c.gpuWanted(now.Add(gpuWatch + time.Second)) {
		t.Fatal("readings should stop a while after the page closes")
	}
	c.mu.Lock()
	c.add(Point{T: now.Unix(), Converting: 1})
	c.mu.Unlock()
	if !c.gpuWanted(now.Add(gpuWatch + time.Second)) {
		t.Fatal("a converting stream should keep the card read")
	}
}

func TestMinuteAverageSkipsUnreadCard(t *testing.T) {
	a := average([]Point{{T: 60, GPU: 40, GPUOn: true}, {T: 65}, {T: 70, GPU: 20, GPUOn: true}})
	if !a.GPUOn || a.GPU != 30 {
		t.Fatalf("got %+v, want GPU 30 over the two readings", a)
	}
	if b := average([]Point{{T: 60}, {T: 65}}); b.GPUOn {
		t.Fatal("a minute with no card readings has no card value")
	}
}
