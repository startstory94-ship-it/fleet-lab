package live

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"fleetlab/internal/fleet"
)

// The benchmark-only switches (cmd/loadtest) must really do what they say,
// or the load test compares the default with itself.

// EncodePerClient: one encode per subscriber per delta, distinct buffers with
// the same bytes. (The default, one encode shared by all, is
// TestEncodeOncePerFrame.)
func TestBenchEncodePerClient(t *testing.T) {
	f := newFake("r1", "r2")
	b := New(Config{Buffer: 8, EncodePerClient: true}, f, f)
	b.Tick()
	var subs []*Sub
	for range 5 {
		s, unsub := subscribe(b)
		defer unsub()
		next(t, s) // snapshot
		subs = append(subs, s)
	}
	before := b.Stats().Encodes
	f.edit("r1", func(r *fleet.Robot) { r.X = 7 })
	b.Tick()
	if got := b.Stats().Encodes - before; got != 5 {
		t.Fatalf("%d encodes for one delta and 5 subscribers, want 5", got)
	}
	var first []byte
	for i, s := range subs {
		data, fr := next(t, s)
		if fr.Type != Delta || len(fr.Robots) != 1 || fr.Robots[0].X != 7 {
			t.Fatalf("sub %d got %+v", i, fr)
		}
		if i == 0 {
			first = data
			continue
		}
		if &data[0] == &first[0] || !bytes.Equal(data, first) {
			t.Fatalf("sub %d: shared=%v equal=%v, want its own copy of the same bytes", i, &data[0] == &first[0], bytes.Equal(data, first))
		}
	}
}

// BlockOnSlow: a full queue makes Tick wait for the reader instead of
// dropping; nothing is dropped or resynced and the reader sees every delta.
func TestBenchBlockOnSlow(t *testing.T) {
	f := newFake("r1")
	b := New(Config{Buffer: 1, BlockOnSlow: true, BlockTimeout: 5 * time.Second}, f, f)
	b.Tick()
	s, unsub := subscribe(b)
	defer unsub()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got := make(chan Frame, 16)
	go func() {
		for i := 0; i < 4; i++ {
			time.Sleep(30 * time.Millisecond) // slower than the ticks below
			data, err := s.Next(ctx)
			if err != nil {
				close(got)
				return
			}
			var fr Frame
			if json.Unmarshal(data, &fr) != nil {
				close(got)
				return
			}
			got <- fr
		}
		close(got)
	}()
	start := time.Now()
	for i := 1; i <= 3; i++ {
		f.edit("r1", func(r *fleet.Robot) { r.X = float64(10 * i) })
		b.Tick()
	}
	if time.Since(start) < 30*time.Millisecond {
		t.Fatal("Tick did not wait for the slow reader")
	}
	var kinds []string
	var seqs []uint64
	for fr := range got {
		kinds = append(kinds, fr.Type)
		seqs = append(seqs, fr.Seq)
	}
	st := b.Stats()
	if len(kinds) != 4 || kinds[0] != Snapshot || kinds[1] != Delta || kinds[3] != Delta || seqs[3] != 4 {
		t.Fatalf("reader got %v %v, want the snapshot and all 3 deltas", kinds, seqs)
	}
	if st.Dropped != 0 || st.Resyncs != 0 || st.Blocked == 0 {
		t.Fatalf("stats %+v, want blocking (blocked > 0) and no drops", st)
	}
}

// BlockOnSlow gives up after BlockTimeout and falls back to drop + resync,
// so a client that never reads cannot wedge the broadcaster.
func TestBenchBlockTimeoutFallsBack(t *testing.T) {
	f := newFake("r1")
	b := New(Config{Buffer: 1, BlockOnSlow: true, BlockTimeout: 20 * time.Millisecond}, f, f)
	b.Tick()
	_, unsub := subscribe(b) // never read
	defer unsub()
	for i := 1; i <= 3; i++ {
		f.edit("r1", func(r *fleet.Robot) { r.X = float64(i) })
		b.Tick()
	}
	if st := b.Stats(); st.Blocked == 0 || st.Dropped == 0 || st.Resyncs == 0 {
		t.Fatalf("stats %+v, want a timed-out wait, then drop and resync", st)
	}
}
