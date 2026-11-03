package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"fleetlab/internal/elevator"
	"fleetlab/internal/task"
)

// outbox publishes for the task manager: robot commands (task.Sender) and
// the elevator channel (elevator.Link). The manager calls it with its lock
// held and a QoS 1 publish waits for the broker, so messages are queued and
// sent by one goroutine, in order. Calls never block.
//
//   - Robot commands: at most maxRobotCmds waiting; beyond that Send fails
//     and the manager re-sends later (as before).
//   - Elevator commands and turns are never refused. Beyond maxLift waiting
//     the oldest of them is dropped with a log line; the conductor re-sends
//     both (call/goto until the car shows it, the open turn every 5 s).
//   - Retained trips (and their clearing) are never dropped: a newer one for
//     the same car replaces the one still waiting (it holds the whole state),
//     and one that fails to publish is retried until a newer one exists.
type outbox struct {
	mu         sync.Mutex
	q          []outMsg
	robotCmds  int // robot commands in q
	liftMsgs   int // elevator commands and turns in q
	wake       chan struct{}
	log        *slog.Logger
	retryDelay time.Duration
}

const (
	maxRobotCmds = 512
	maxLift      = 2048
)

type msgKind int

const (
	kindRobot msgKind = iota
	kindLift          // elevator command or turn
	kindTrip          // retained trip or its clearing
)

type outMsg struct {
	kind   msgKind
	car    string
	topic  string
	retain bool
	body   []byte
	id     string // for logs
}

// publisher is mqttx.Client.
type publisher interface {
	Publish(ctx context.Context, topic string, qos byte, retain bool, payload []byte) error
}

func newOutbox(log *slog.Logger) *outbox {
	return &outbox{wake: make(chan struct{}, 1), log: log, retryDelay: time.Second}
}

func (q *outbox) put(m outMsg) error {
	q.mu.Lock()
	switch m.kind {
	case kindRobot:
		if q.robotCmds >= maxRobotCmds {
			q.mu.Unlock()
			return errors.New("command queue full")
		}
		q.robotCmds++
	case kindLift:
		if q.liftMsgs >= maxLift {
			for i, o := range q.q {
				if o.kind == kindLift {
					q.log.Warn("publish queue full; dropping an elevator message (it is re-sent)", "topic", o.topic, "id", o.id)
					q.q = append(q.q[:i], q.q[i+1:]...)
					q.liftMsgs--
					break
				}
			}
		}
		q.liftMsgs++
	case kindTrip:
		q.dropTrips(m.car)
	}
	q.q = append(q.q, m)
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
	return nil
}

// dropTrips removes waiting trip messages of car (a newer one replaces them).
func (q *outbox) dropTrips(car string) {
	keep := q.q[:0]
	for _, o := range q.q {
		if o.kind != kindTrip || o.car != car {
			keep = append(keep, o)
		}
	}
	clear(q.q[len(keep):])
	q.q = keep
}

func (q *outbox) pop() (outMsg, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.q) == 0 {
		return outMsg{}, false
	}
	m := q.q[0]
	q.q = q.q[1:]
	switch m.kind {
	case kindRobot:
		q.robotCmds--
	case kindLift:
		q.liftMsgs--
	}
	return m, true
}

// retry puts a trip message that failed back in front, unless a newer one
// for the same car is already waiting.
func (q *outbox) retry(m outMsg) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, o := range q.q {
		if o.kind == kindTrip && o.car == m.car {
			return
		}
	}
	q.q = append([]outMsg{m}, q.q...)
}

func (q *outbox) Send(robot string, c task.Command) error {
	body, _ := json.Marshal(c)
	return q.put(outMsg{kind: kindRobot, topic: "fleet/" + robot + "/cmd", body: body, id: c.CmdID})
}

func (q *outbox) Command(car string, c elevator.Cmd) error {
	body, _ := json.Marshal(c)
	return q.put(outMsg{kind: kindLift, car: car, topic: fmt.Sprintf(elevator.TopicCmd, car), body: body, id: c.CmdID})
}

func (q *outbox) PublishTurn(car string, t elevator.Turn) error {
	body, _ := json.Marshal(t)
	return q.put(outMsg{kind: kindLift, car: car, topic: fmt.Sprintf(elevator.TopicTurn, car), body: body,
		id: fmt.Sprintf("%s #%d", t.Trip, t.N)})
}

// PublishTrip is retained: robots and a restarted fleetd read the trip's
// last stage from it.
func (q *outbox) PublishTrip(car string, t elevator.Trip) error {
	body, _ := json.Marshal(t)
	return q.put(outMsg{kind: kindTrip, car: car, topic: fmt.Sprintf(elevator.TopicTrip, car), retain: true, body: body,
		id: t.ID + " " + t.Stage})
}

// ClearTrip empties the retained trip (zero-length retained publish).
func (q *outbox) ClearTrip(car string) error {
	return q.put(outMsg{kind: kindTrip, car: car, topic: fmt.Sprintf(elevator.TopicTrip, car), retain: true, body: []byte{},
		id: "clear"})
}

func (q *outbox) run(ctx context.Context, mq publisher) {
	for {
		m, ok := q.pop()
		if !ok {
			select {
			case <-ctx.Done():
				return
			case <-q.wake:
				continue
			}
		}
		pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := mq.Publish(pctx, m.topic, 1, m.retain, m.body)
		cancel()
		if err == nil {
			q.log.Debug("published", "topic", m.topic, "id", m.id)
			continue
		}
		q.log.Warn("publish", "topic", m.topic, "id", m.id, "err", err)
		if m.kind != kindTrip {
			continue // robot commands and elevator messages are re-sent by their owners
		}
		q.retry(m)
		select {
		case <-ctx.Done():
			return
		case <-time.After(q.retryDelay):
		}
	}
}
