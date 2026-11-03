package fleet

import (
	"io"
	"log/slog"
	"testing"
)

func TestIngestRoutesByTopic(t *testing.T) {
	g, _, _ := newRecordingRegistry()
	in := Ingest(g, slog.New(slog.NewTextHandler(io.Discard, nil)))

	in("fleet/robot_1/pose", []byte(`{"boot":"b","seq":1,"host":"pc1","x":1.5,"y":2,"floor":3}`))
	in("fleet/robot_1/state", []byte(`{"boot":"b","seq":1,"host":"pc1","mode":"MOVING","battery":80}`))

	r, ok := g.Get("robot_1")
	if !ok {
		t.Fatal("robot_1 not registered")
	}
	if r.X != 1.5 || r.Floor != 3 || r.Mode != "MOVING" || r.Battery != 80 || r.Host != "pc1" {
		t.Errorf("robot_1 = %+v", r)
	}

	in("fleet/adapter/pc1/status", []byte(`{"host":"pc1","status":"offline"}`))
	if r, _ := g.Get("robot_1"); r.Conn != Offline {
		t.Errorf("after adapter offline: %s, want OFFLINE", r.Conn)
	}
}

func TestIngestIgnoresJunk(t *testing.T) {
	g, _, _ := newRecordingRegistry()
	in := Ingest(g, slog.New(slog.NewTextHandler(io.Discard, nil)))

	for _, c := range []struct{ topic, payload string }{
		{"fleet/robot_1/pose", `not json`},
		{"fleet/robot_1/unknown", `{"seq":1}`},
		{"other/robot_1/pose", `{"boot":"b","seq":1}`},
		{"fleet/robot_1", `{}`},
		{"fleet/a/b/c/d", `{}`},
		{"fleet/adapter/pc1/status", `not json`},
		{"fleet/adapter/pc1/status", `{"status":"online"}`}, // online alone changes nothing
	} {
		in(c.topic, []byte(c.payload))
	}
	if n := len(g.Snapshot()); n != 0 {
		t.Errorf("registry has %d robots after junk input, want 0: %+v", n, g.Snapshot())
	}
}

// A value that cannot be shown or encoded is refused whole; the robot keeps
// its last good values and the refusal is counted.
func TestIngestRejectsOutOfRange(t *testing.T) {
	g, _, _ := newRecordingRegistry()
	in := Ingest(g, slog.New(slog.NewTextHandler(io.Discard, nil)))
	in("fleet/r/pose", []byte(`{"boot":"b","seq":1,"x":1.5,"y":2,"yaw":0.5,"v":0.8,"floor":1}`))
	in("fleet/r/state", []byte(`{"boot":"b","seq":1,"mode":"IDLE","battery":80}`))

	bad := []struct{ topic, payload string }{
		{"fleet/r/pose", `{"boot":"b","seq":2,"x":1e306,"y":0,"floor":1}`},
		{"fleet/r/pose", `{"boot":"b","seq":3,"x":0,"y":-1000001,"floor":1}`},
		{"fleet/r/pose", `{"boot":"b","seq":4,"x":0,"y":0,"v":1e9,"floor":1}`},
		{"fleet/r/pose", `{"boot":"b","seq":5,"x":0,"y":0,"yaw":1e400,"floor":1}`}, // bad JSON number
		{"fleet/r/state", `{"boot":"b","seq":2,"mode":"IDLE","battery":-1}`},
		{"fleet/r/state", `{"boot":"b","seq":3,"mode":"IDLE","battery":100.5}`},
		{"fleet/r/state", `{"boot":"b","seq":4,"mode":"IDLE","battery":50,"silence_s":1e300}`},
	}
	for _, c := range bad {
		in(c.topic, []byte(c.payload))
	}
	r, _ := g.Get("r")
	if r.X != 1.5 || r.Y != 2 || r.Yaw != 0.5 || r.V != 0.8 || r.Battery != 80 || r.Conn != Online {
		t.Errorf("robot changed by refused messages: %+v", r)
	}
	if got := g.Rejected(); got != uint64(len(bad)) {
		t.Errorf("Rejected = %d, want %d", got, len(bad))
	}
	// The limits themselves are accepted.
	in("fleet/r/pose", []byte(`{"boot":"b","seq":6,"x":-1e6,"y":1e6,"v":-100,"floor":2}`))
	in("fleet/r/state", []byte(`{"boot":"b","seq":5,"mode":"IDLE","battery":100}`))
	if r, _ := g.Get("r"); r.X != -1e6 || r.Floor != 2 || r.Battery != 100 {
		t.Errorf("values at the limits refused: %+v", r)
	}
}
