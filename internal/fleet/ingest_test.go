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
