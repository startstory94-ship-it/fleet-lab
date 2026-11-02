// Package mqttx wraps autopaho with the defaults fleetd needs: automatic
// reconnect with jittered exponential backoff and re-subscription on every
// (re)connect.
package mqttx

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/url"
	"time"

	"github.com/eclipse/paho.golang/autopaho"
	"github.com/eclipse/paho.golang/paho"
)

// Handler receives every message on a subscribed topic. It runs on the MQTT
// client's goroutine and must not block.
type Handler func(topic string, payload []byte)

// Subscription is one topic filter to (re)subscribe on connect.
type Subscription struct {
	Topic string
	QoS   byte
}

type Client struct {
	cm *autopaho.ConnectionManager
}

// Backoff returns the wait before reconnect attempt n: exponential from 1 s,
// capped at 30 s, with ±50% jitter so many clients don't retry in lockstep.
func Backoff(n int) time.Duration {
	base := time.Second << min(n, 5) // 1, 2, 4, 8, 16, 32 s
	base = min(base, 30*time.Second)
	return time.Duration(float64(base) * (0.5 + rand.Float64()))
}

// Connect starts a managed connection and returns immediately; the connection
// is (re)established in the background until ctx is cancelled.
func Connect(ctx context.Context, log *slog.Logger, brokerURL, clientID string,
	subs []Subscription, h Handler) (*Client, error) {
	u, err := url.Parse(brokerURL)
	if err != nil {
		return nil, fmt.Errorf("mqtt url: %w", err)
	}
	opts := make([]paho.SubscribeOptions, len(subs))
	for i, s := range subs {
		opts[i] = paho.SubscribeOptions{Topic: s.Topic, QoS: s.QoS}
	}

	cfg := autopaho.ClientConfig{
		ServerUrls:                    []*url.URL{u},
		KeepAlive:                     5,
		CleanStartOnInitialConnection: true,
		ReconnectBackoff:              Backoff,
		ConnectTimeout:                5 * time.Second,
		OnConnectionUp: func(cm *autopaho.ConnectionManager, _ *paho.Connack) {
			log.Info("mqtt connected", "broker", u.Host)
			go func() {
				if _, err := cm.Subscribe(ctx, &paho.Subscribe{Subscriptions: opts}); err != nil {
					log.Error("mqtt subscribe", "err", err)
				}
			}()
		},
		OnConnectError: func(err error) { log.Warn("mqtt connect", "err", err) },
		ClientConfig: paho.ClientConfig{
			ClientID: clientID,
			OnPublishReceived: []func(paho.PublishReceived) (bool, error){
				func(pr paho.PublishReceived) (bool, error) {
					h(pr.Packet.Topic, pr.Packet.Payload)
					return true, nil
				},
			},
			OnServerDisconnect: func(d *paho.Disconnect) {
				log.Warn("mqtt server disconnect", "reason", d.ReasonCode)
			},
			OnClientError: func(err error) { log.Warn("mqtt client error", "err", err) },
		},
	}
	cm, err := autopaho.NewConnection(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &Client{cm: cm}, nil
}

// Publish sends one message. It fails if not currently connected.
func (c *Client) Publish(ctx context.Context, topic string, qos byte, retain bool, payload []byte) error {
	_, err := c.cm.Publish(ctx, &paho.Publish{Topic: topic, QoS: qos, Retain: retain, Payload: payload})
	return err
}

// Close disconnects cleanly and waits for the connection manager to stop.
func (c *Client) Close(ctx context.Context) {
	_ = c.cm.Disconnect(ctx)
	select {
	case <-c.cm.Done():
	case <-ctx.Done():
	}
}
