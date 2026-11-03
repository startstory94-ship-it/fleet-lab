package fleet

import (
	"encoding/json"
	"log/slog"
	"strings"
)

// Topic filters the ingest handler expects to be subscribed to.
const (
	TopicPose          = "fleet/+/pose"
	TopicState         = "fleet/+/state"
	TopicAdapterStatus = "fleet/adapter/+/status"
)

// Ingest returns an MQTT message handler that feeds the registry.
func Ingest(g *Registry, log *slog.Logger) func(topic string, payload []byte) {
	return func(topic string, payload []byte) {
		parts := strings.Split(topic, "/")
		if len(parts) != 3 && len(parts) != 4 || parts[0] != "fleet" {
			return
		}
		if parts[1] == "adapter" {
			var st struct {
				Status string `json:"status"`
			}
			if err := json.Unmarshal(payload, &st); err != nil {
				log.Warn("bad adapter status", "topic", topic, "err", err)
				return
			}
			log.Info("adapter status", "host", parts[2], "status", st.Status)
			if st.Status == "offline" {
				g.AdapterOffline(parts[2])
			}
			return
		}
		name, kind := parts[1], parts[2]
		var err error
		switch kind {
		case "pose":
			var m PoseMsg
			if err = json.Unmarshal(payload, &m); err == nil {
				if err = m.Check(); err == nil && !g.ApplyPose(name, m) {
					log.Debug("stale pose dropped", "robot", name, "boot", m.Boot, "seq", m.Seq)
				}
			}
		case "state":
			var m StateMsg
			if err = json.Unmarshal(payload, &m); err == nil {
				if err = m.Check(); err == nil && !g.ApplyState(name, m) {
					log.Debug("stale state dropped", "robot", name, "boot", m.Boot, "seq", m.Seq)
				}
			}
		}
		if err != nil {
			g.rejected.Add(1)
			log.Warn("bad message", "topic", topic, "err", err)
		}
	}
}
