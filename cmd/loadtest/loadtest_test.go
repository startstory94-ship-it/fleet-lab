package main

import (
	"testing"
	"time"
)

func TestFrameHeader(t *testing.T) {
	data := []byte(`{"type":"delta","seq":42,"time":"2026-11-03T09:00:01.35+09:00","robots":[{"name":"x","time":"no"}]}`)
	typ, ft, ok := frameHeader(data)
	want := time.Date(2026, 11, 3, 0, 0, 1, 350_000_000, time.UTC)
	if typ != "delta" || !ok || !ft.Equal(want) {
		t.Fatalf("frameHeader = %q %v %v, want delta %v", typ, ft, ok, want)
	}
	if typ, _, ok := frameHeader([]byte(`{"type":"heartbeat","seq":1}`)); typ != "heartbeat" || ok {
		t.Fatalf("no time: %q %v", typ, ok)
	}
	if typ, _, ok := frameHeader([]byte(`{"ty`)); typ != "" || ok {
		t.Fatalf("truncated: %q %v", typ, ok)
	}
}

func TestPercentileAndPeak(t *testing.T) {
	var ds []time.Duration
	for i := 100; i >= 1; i-- {
		ds = append(ds, time.Duration(i)*time.Millisecond)
	}
	if p := percentile(ds, 50); p != 50*time.Millisecond {
		t.Fatalf("p50 = %v", p)
	}
	if p := percentile(ds, 99); p != 99*time.Millisecond {
		t.Fatalf("p99 = %v", p)
	}
	if p := percentile(ds, 100); p != 100*time.Millisecond {
		t.Fatalf("max = %v", p)
	}
	if percentile(nil, 50) != 0 {
		t.Fatal("empty")
	}
	// 3 attempts in [0,100ms), 1 in [100,200), 2 in [1000,1100).
	offs := []time.Duration{0, 50 * time.Millisecond, 99 * time.Millisecond, 100 * time.Millisecond, time.Second, 1050 * time.Millisecond}
	if most, busy := peak(offs, 100*time.Millisecond); most != 3 || busy != 3 {
		t.Fatalf("peak = %d, %d buckets; want 3, 3", most, busy)
	}
	if h := histogram(offs, 500*time.Millisecond); len(h) != 3 || h[0] != 4 || h[1] != 0 || h[2] != 2 {
		t.Fatalf("histogram = %v", h)
	}
}

func TestSummary(t *testing.T) {
	if s := summary([]float64{3, 1, 2}, 1); s != "2.0 [1.0–3.0]" {
		t.Fatalf("summary = %q", s)
	}
	if s := summary([]float64{4, 1, 2, 3}, 0); s != "2 [1–4]" { // median 2.5 rounds to even
		t.Fatalf("summary = %q", s)
	}
}

// -check (CI) must fail on each thing it is there to catch.
func TestFrameRunCheck(t *testing.T) {
	c := config{clients: 4, slow: 1}
	good := frameRun{Scenario: "default", FastFPS: 6.7, DroppedTotal: 10, SlowSnapshots: 2}
	if err := good.check(c); err != nil {
		t.Fatalf("good run failed the check: %v", err)
	}
	for name, bad := range map[string]func(r *frameRun){
		"client error": func(r *frameRun) { r.ClientErrors = 1 },
		"no frames":    func(r *frameRun) { r.FastFPS = 0 },
		"no drops":     func(r *frameRun) { r.DroppedTotal = 0 },
		"no catch-up":  func(r *frameRun) { r.SlowSnapshots = 0 },
	} {
		r := good
		bad(&r)
		if r.check(c) == nil {
			t.Errorf("%s: check passed", name)
		}
	}
	block := frameRun{Scenario: "block", FastFPS: 2} // waits instead of dropping
	if err := block.check(c); err != nil {
		t.Fatalf("block run: %v", err)
	}
}

func TestBackoffNoJitter(t *testing.T) {
	want := []time.Duration{1, 2, 4, 8, 16, 30, 30}
	for n, w := range want {
		if got := backoffNoJitter(n); got != w*time.Second {
			t.Fatalf("backoffNoJitter(%d) = %v, want %v", n, got, w*time.Second)
		}
	}
}
