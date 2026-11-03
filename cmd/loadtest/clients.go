package main

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// wsClient is one dashboard on /ws/live. A slow one sleeps after every frame
// it reads, so the server has to drop frames for it (or, with
// -bench-block-slow, wait for it).
type wsClient struct {
	slow   bool
	delay  time.Duration
	rcvBuf int // socket receive buffer for this client; 0: OS default

	mu     sync.Mutex
	counts counts
	// Frame time -> received. Fast clients: deltas (latency). Slow clients:
	// deltas and snapshots, i.e. how old the state they are shown is.
	lats []latency
	err  error // the connection failed after it was up (dial retries do not count)
}

type counts struct {
	Frames, Bytes, Deltas, Snapshots, Heartbeats int64
}

func (c counts) sub(o counts) counts {
	return counts{c.Frames - o.Frames, c.Bytes - o.Bytes, c.Deltas - o.Deltas, c.Snapshots - o.Snapshots, c.Heartbeats - o.Heartbeats}
}

type latency struct {
	at time.Time
	d  time.Duration
}

func (c *wsClient) snapshot() counts {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.counts
}

// latencies returns the frame ages recorded in [from, to).
func (c *wsClient) latencies(from, to time.Time) []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []time.Duration
	for _, l := range c.lats {
		if !l.at.Before(from) && l.at.Before(to) {
			out = append(out, l.d)
		}
	}
	return out
}

// run reads frames until ctx ends. It retries the dial for a while: fleetd
// answers 503 when its subscriber limit is reached.
func (c *wsClient) run(ctx context.Context, url string) {
	var conn *websocket.Conn
	for {
		var err error
		conn, _, err = websocket.Dial(ctx, url, c.dialOptions())
		if err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(200 * time.Millisecond):
		}
	}
	defer conn.CloseNow()
	conn.SetReadLimit(64 << 20)
	for {
		_, data, err := conn.Read(ctx)
		now := time.Now()
		if err != nil {
			if ctx.Err() == nil {
				c.mu.Lock()
				c.err = err
				c.mu.Unlock()
			}
			return
		}
		typ, ft, ok := frameHeader(data)
		c.mu.Lock()
		c.counts.Frames++
		c.counts.Bytes += int64(len(data))
		switch typ {
		case "delta":
			c.counts.Deltas++
			if ok {
				c.lats = append(c.lats, latency{now, now.Sub(ft)})
			}
		case "snapshot":
			c.counts.Snapshots++
			if ok && c.slow {
				c.lats = append(c.lats, latency{now, now.Sub(ft)})
			}
		case "heartbeat":
			c.counts.Heartbeats++
		}
		c.mu.Unlock()
		if c.slow {
			select {
			case <-ctx.Done():
				return
			case <-time.After(c.delay):
			}
		}
	}
}

// dialOptions gives a slow client a small socket receive buffer when asked,
// so it backs up into the server's queue sooner.
func (c *wsClient) dialOptions() *websocket.DialOptions {
	if c.rcvBuf <= 0 {
		return nil
	}
	d := &net.Dialer{}
	tr := &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := d.DialContext(ctx, network, addr)
		if tc, ok := conn.(*net.TCPConn); ok {
			tc.SetReadBuffer(c.rcvBuf)
		}
		return conn, err
	}}
	return &websocket.DialOptions{HTTPClient: &http.Client{Transport: tr}}
}

// frameHeader reads type and time from the start of a /ws/live frame
// ({"type":"delta","seq":42,"time":"...",...}) without decoding the robots:
// twenty clients decoding 100 KB frames would load the machine the server
// runs on.
func frameHeader(data []byte) (typ string, t time.Time, ok bool) {
	head := data[:min(len(data), 256)]
	typ = field(head, `"type":"`)
	ts := field(head, `"time":"`)
	if ts == "" {
		return typ, time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	return typ, t, err == nil
}

func field(b []byte, key string) string {
	i := bytes.Index(b, []byte(key))
	if i < 0 {
		return ""
	}
	rest := b[i+len(key):]
	j := bytes.IndexByte(rest, '"')
	if j < 0 {
		return ""
	}
	return string(rest[:j])
}
