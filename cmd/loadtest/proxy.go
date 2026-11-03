package main

import (
	"io"
	"net"
	"sync"
	"time"
)

// proxy forwards TCP between the fake robots and the broker so the load test
// can cut every robot's connection at once (a broker restart or a site-wide
// network blip, as the robots see it) while fleetd stays connected directly.
// During an outage it accepts and closes at once, so a reconnect attempt
// fails fast the way it does against a broker that is down.
//
// The robots' first hop is this listener, so in a storm its TCP accept
// queue (the OS listen backlog) is what overflows first, as a broker's own
// listener would; refused dials show up as "actively refused" (Windows) or
// "connection refused" in the report.
type proxy struct {
	ln     net.Listener
	target string

	mu         sync.Mutex
	conns      map[net.Conn]struct{}
	downUntil  time.Time
	dialFailed int // proxy -> broker dials that failed
}

func newProxy(target string) (*proxy, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	p := &proxy{ln: ln, target: target, conns: map[net.Conn]struct{}{}}
	go p.serve()
	return p, nil
}

func (p *proxy) addr() string { return p.ln.Addr().String() }

func (p *proxy) serve() {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
		down := time.Now().Before(p.downUntil)
		p.mu.Unlock()
		if down {
			c.Close()
			continue
		}
		go p.pipe(c)
	}
}

func (p *proxy) pipe(c net.Conn) {
	b, err := net.DialTimeout("tcp", p.target, 5*time.Second)
	if err != nil {
		p.mu.Lock()
		p.dialFailed++
		p.mu.Unlock()
		c.Close()
		return
	}
	p.mu.Lock()
	if time.Now().Before(p.downUntil) { // cut while dialling
		p.mu.Unlock()
		c.Close()
		b.Close()
		return
	}
	p.conns[c], p.conns[b] = struct{}{}, struct{}{}
	p.mu.Unlock()
	done := make(chan struct{}, 2)
	go func() { io.Copy(b, c); done <- struct{}{} }()
	go func() { io.Copy(c, b); done <- struct{}{} }()
	<-done
	c.Close()
	b.Close()
	<-done
	p.mu.Lock()
	delete(p.conns, c)
	delete(p.conns, b)
	p.mu.Unlock()
}

// cut closes every proxied connection now and refuses new ones for d.
func (p *proxy) cut(d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.downUntil = time.Now().Add(d)
	for c := range p.conns {
		c.Close()
	}
}

func (p *proxy) dialFailures() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.dialFailed
}

func (p *proxy) close() {
	p.ln.Close()
	p.cut(time.Hour)
}
