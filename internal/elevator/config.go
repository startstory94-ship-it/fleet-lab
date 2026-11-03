package elevator

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strconv"
	"time"
)

// Point is a position on a floor's map.
type Point struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

// CarConfig describes one elevator.
type CarConfig struct {
	ID       string `json:"id"`
	Capacity int    `json:"capacity"` // robot slots, door (0) to back
	Floors   []int  `json:"floors"`   // floors it serves
	// Lobby is where robots wait for the car and come out of it, on every
	// floor unless Lobbies has that floor.
	Lobby   Point            `json:"lobby"`
	Lobbies map[string]Point `json:"lobbies,omitempty"` // floor ("3") -> lobby
	FloorS  float64          `json:"floor_s"`           // travel seconds per floor (estimates)
	DoorS   float64          `json:"door_s"`            // door open or close seconds (estimates)
}

// Serves reports whether the car stops at floor.
func (c CarConfig) Serves(floor int) bool { return slices.Contains(c.Floors, floor) }

// LobbyAt returns the lobby point on floor.
func (c CarConfig) LobbyAt(floor int) Point {
	if p, ok := c.Lobbies[strconv.Itoa(floor)]; ok {
		return p
	}
	return c.Lobby
}

func (c CarConfig) floorTime() time.Duration { return secs(c.FloorS) }
func (c CarConfig) doorTime() time.Duration  { return secs(c.DoorS) }

func secs(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }

// Timing holds the conductor's clocks. All cars share it.
type Timing struct {
	// BatchWindow: after the first robot reaches a lobby, wait this long for
	// others before planning, so they ride together.
	BatchWindow time.Duration
	// AcceptTimeout: every rider must accept its ride command by then, or
	// the trip is aborted before anyone boards.
	AcceptTimeout time.Duration
	// CallMargin is added to the estimated time for the car to reach the
	// origin with the door open; past it the trip is aborted.
	CallMargin time.Duration
	// Silence: no state from the car for this long means it is not
	// responding (it publishes every second).
	Silence time.Duration
	// TurnTimeout: the next robot moves if the one before has not reported
	// zone_clear (or failed) by then.
	TurnTimeout time.Duration
	// ExitRetries: a robot that reports exit_failed gets this many more
	// turns at the same stop before it is carried to the next one.
	ExitRetries int
	// HoldRefresh: while robots move through the door, hold is re-sent this
	// often, each asking for HoldFor more seconds.
	HoldRefresh time.Duration
	HoldFor     time.Duration
	// CmdRetry: call/goto is re-sent this often until the car shows it.
	CmdRetry time.Duration
	// TravelMargin is added to a travel estimate before "overdue".
	TravelMargin time.Duration
	// LeaseMargin is added to the whole trip's estimate for the lease.
	LeaseMargin time.Duration
	// SilenceMargin is added to each robot's announced blackout.
	SilenceMargin time.Duration
	// Backoff: after the car did not respond, no trip is planned for this long.
	Backoff time.Duration
	// TurnEstimate is the expected time for one robot to pass the door.
	TurnEstimate time.Duration
	// StuckRetry: a car blocked by a robot that could not get out retries
	// its exit this often.
	StuckRetry time.Duration
	// Republish: the retained trip is published again this often (it may
	// have been lost while the broker was away).
	Republish time.Duration
}

func DefaultTiming() Timing {
	return Timing{
		BatchWindow:   3 * time.Second,
		AcceptTimeout: 10 * time.Second,
		CallMargin:    15 * time.Second,
		Silence:       5 * time.Second,
		TurnTimeout:   15 * time.Second,
		ExitRetries:   1,
		HoldRefresh:   5 * time.Second,
		HoldFor:       15 * time.Second,
		CmdRetry:      5 * time.Second,
		TravelMargin:  15 * time.Second,
		LeaseMargin:   60 * time.Second,
		SilenceMargin: 20 * time.Second,
		Backoff:       30 * time.Second,
		TurnEstimate:  6 * time.Second,
		StuckRetry:    time.Minute,
		Republish:     10 * time.Second,
	}
}

// Config is the -elevators file.
type Config struct {
	Elevators []CarConfig `json:"elevators"`
	Timing    Timing      `json:"-"`
}

// timingJSON is the optional "timing" object of the file, in seconds.
type timingJSON struct {
	BatchWindowS   *float64 `json:"batch_window_s"`
	AcceptTimeoutS *float64 `json:"accept_timeout_s"`
	CallMarginS    *float64 `json:"call_margin_s"`
	SilenceS       *float64 `json:"silence_s"`
	TurnTimeoutS   *float64 `json:"turn_timeout_s"`
	ExitRetries    *int     `json:"exit_retries"`
	HoldRefreshS   *float64 `json:"hold_refresh_s"`
	HoldForS       *float64 `json:"hold_for_s"`
	CmdRetryS      *float64 `json:"cmd_retry_s"`
	TravelMarginS  *float64 `json:"travel_margin_s"`
	LeaseMarginS   *float64 `json:"lease_margin_s"`
	SilenceMarginS *float64 `json:"silence_margin_s"`
	BackoffS       *float64 `json:"backoff_s"`
	TurnEstimateS  *float64 `json:"turn_estimate_s"`
	StuckRetryS    *float64 `json:"stuck_retry_s"`
	RepublishS     *float64 `json:"republish_s"`
}

// DefaultCar is the single elevator fleetd uses without -elevators: E1,
// three slots, floors 1-10, lobby at (10, 0) on every floor.
func DefaultCar() CarConfig {
	return CarConfig{ID: "E1", Capacity: 3, Floors: []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10},
		Lobby: Point{X: 10, Y: 0}, FloorS: 2, DoorS: 2}
}

// DefaultConfig is DefaultCar with DefaultTiming.
func DefaultConfig() Config {
	return Config{Elevators: []CarConfig{DefaultCar()}, Timing: DefaultTiming()}
}

// Parse reads a config file's contents. Missing car fields get DefaultCar's
// values (except id, floors and lobby, which are required); timing fields
// left out keep DefaultTiming.
func Parse(data []byte) (Config, error) {
	var raw struct {
		Elevators []json.RawMessage `json:"elevators"`
		Timing    timingJSON        `json:"timing"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return Config{}, err
	}
	cfg := Config{Timing: DefaultTiming()}
	ids := map[string]bool{}
	for i, r := range raw.Elevators {
		d := DefaultCar()
		c := CarConfig{Capacity: d.Capacity, FloorS: d.FloorS, DoorS: d.DoorS}
		var probe struct {
			Lobby *Point `json:"lobby"`
		}
		if err := json.Unmarshal(r, &c); err != nil {
			return Config{}, fmt.Errorf("elevator %d: %w", i, err)
		}
		_ = json.Unmarshal(r, &probe)
		switch {
		case c.ID == "":
			return Config{}, fmt.Errorf("elevator %d: id is required", i)
		case ids[c.ID]:
			return Config{}, fmt.Errorf("elevator %s: duplicate id", c.ID)
		case len(c.Floors) < 2:
			return Config{}, fmt.Errorf("elevator %s: needs at least two floors", c.ID)
		case c.Capacity < 1:
			return Config{}, fmt.Errorf("elevator %s: capacity must be at least 1", c.ID)
		case probe.Lobby == nil:
			return Config{}, fmt.Errorf("elevator %s: lobby is required", c.ID)
		case c.FloorS <= 0 || c.DoorS <= 0:
			return Config{}, fmt.Errorf("elevator %s: floor_s and door_s must be positive", c.ID)
		}
		for k := range c.Lobbies {
			if _, err := strconv.Atoi(k); err != nil {
				return Config{}, fmt.Errorf("elevator %s: lobbies key %q is not a floor number", c.ID, k)
			}
		}
		ids[c.ID] = true
		cfg.Elevators = append(cfg.Elevators, c)
	}
	t := &cfg.Timing
	for _, f := range []struct {
		v   *float64
		dst *time.Duration
	}{
		{raw.Timing.BatchWindowS, &t.BatchWindow}, {raw.Timing.AcceptTimeoutS, &t.AcceptTimeout},
		{raw.Timing.CallMarginS, &t.CallMargin}, {raw.Timing.SilenceS, &t.Silence},
		{raw.Timing.TurnTimeoutS, &t.TurnTimeout}, {raw.Timing.HoldRefreshS, &t.HoldRefresh},
		{raw.Timing.HoldForS, &t.HoldFor}, {raw.Timing.CmdRetryS, &t.CmdRetry},
		{raw.Timing.TravelMarginS, &t.TravelMargin}, {raw.Timing.LeaseMarginS, &t.LeaseMargin},
		{raw.Timing.SilenceMarginS, &t.SilenceMargin}, {raw.Timing.BackoffS, &t.Backoff},
		{raw.Timing.TurnEstimateS, &t.TurnEstimate}, {raw.Timing.StuckRetryS, &t.StuckRetry},
		{raw.Timing.RepublishS, &t.Republish},
	} {
		if f.v != nil {
			if !(*f.v > 0) {
				return Config{}, fmt.Errorf("timing: every value must be a positive number of seconds, got %g", *f.v)
			}
			*f.dst = secs(*f.v)
		}
	}
	if t.HoldRefresh >= t.HoldFor {
		return Config{}, fmt.Errorf("timing: hold_refresh_s (%s) must be shorter than hold_for_s (%s), or the door closes between holds", t.HoldRefresh, t.HoldFor)
	}
	if raw.Timing.ExitRetries != nil {
		t.ExitRetries = max(*raw.Timing.ExitRetries, 0)
	}
	return cfg, nil
}

// Load reads the -elevators flag: "" is DefaultConfig, "none" is no
// elevators (every leg is a plain goto, as before stage 6), anything else a
// JSON file.
func Load(path string) (Config, error) {
	switch path {
	case "":
		return DefaultConfig(), nil
	case "none":
		return Config{Timing: DefaultTiming()}, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	return Parse(data)
}
