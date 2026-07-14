// Package localmqtt is the agent's connection to the GATEWAY-LOCAL Mosquitto
// (the core_mosquitto add-on) — the broker Zigbee2MQTT publishes on. It is
// entirely separate from internal/mqtt (the cloud broker): different broker,
// different credentials, no availability/last-will semantics.
//
// Credentials: LOCAL_MQTT_* env wins (the Mac dev track), else the Supervisor
// services API mints them (the standard add-on pattern — Z2M gets its own the
// same way), else pairing is unavailable and the agent degrades gracefully.
package localmqtt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
)

// ErrUnavailable means no local-broker credentials could be resolved; pairing
// features should report themselves unavailable rather than fail the agent.
var ErrUnavailable = errors.New("local mqtt broker unavailable")

// Options is the resolved local-broker connection config.
type Options struct {
	Host     string
	Port     int
	Username string
	Password string
}

// Resolve finds local-broker connection options: env first, Supervisor second.
func Resolve(ctx context.Context) (Options, error) {
	if host := os.Getenv("LOCAL_MQTT_HOST"); host != "" {
		port := 1883
		if p, err := strconv.Atoi(os.Getenv("LOCAL_MQTT_PORT")); err == nil && p > 0 {
			port = p
		}

		return Options{
			Host:     host,
			Port:     port,
			Username: os.Getenv("LOCAL_MQTT_USERNAME"),
			Password: os.Getenv("LOCAL_MQTT_PASSWORD"),
		}, nil
	}

	token := os.Getenv("SUPERVISOR_TOKEN")
	if token == "" {
		return Options{}, ErrUnavailable
	}

	base := os.Getenv("SUPERVISOR_API")
	if base == "" {
		base = "http://supervisor"
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/services/mqtt", nil)
	if err != nil {
		return Options{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return Options{}, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return Options{}, fmt.Errorf("%w: supervisor services/mqtt: HTTP %d", ErrUnavailable, resp.StatusCode)
	}

	var body struct {
		Data struct {
			Host     string `json:"host"`
			Port     int    `json:"port"`
			Username string `json:"username"`
			Password string `json:"password"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return Options{}, err
	}
	if body.Data.Host == "" {
		return Options{}, ErrUnavailable
	}

	return Options{Host: body.Data.Host, Port: body.Data.Port, Username: body.Data.Username, Password: body.Data.Password}, nil
}

// Client is a thin paho wrapper for the local broker. Auto-reconnect is left
// to paho here (unlike the cloud client): local credentials never rotate.
type Client struct {
	inner paho.Client
	log   *slog.Logger
}

// Connect dials the local broker (10s timeout).
func Connect(opts Options, log *slog.Logger) (*Client, error) {
	pahoOpts := paho.NewClientOptions().
		AddBroker(fmt.Sprintf("tcp://%s:%d", opts.Host, opts.Port)).
		SetClientID("smart-home-agent-local").
		SetCleanSession(true).
		SetAutoReconnect(true).
		SetConnectTimeout(10 * time.Second)

	if opts.Username != "" {
		pahoOpts.SetUsername(opts.Username)
		pahoOpts.SetPassword(opts.Password)
	}

	inner := paho.NewClient(pahoOpts)
	token := inner.Connect()
	if !token.WaitTimeout(10*time.Second) || token.Error() != nil {
		return nil, fmt.Errorf("%w: connect: %v", ErrUnavailable, token.Error())
	}

	return &Client{inner: inner, log: log}, nil
}

// Publish sends a QoS-1 message to the local broker.
func (c *Client) Publish(topic string, payload []byte) error {
	token := c.inner.Publish(topic, 1, false, payload)
	if !token.WaitTimeout(10*time.Second) || token.Error() != nil {
		return fmt.Errorf("local publish %s: %v", topic, token.Error())
	}

	return nil
}

// Subscribe registers a handler at QoS 1.
func (c *Client) Subscribe(topic string, handler func(topic string, payload []byte)) error {
	token := c.inner.Subscribe(topic, 1, func(_ paho.Client, msg paho.Message) {
		handler(msg.Topic(), msg.Payload())
	})
	if !token.WaitTimeout(10*time.Second) || token.Error() != nil {
		return fmt.Errorf("local subscribe %s: %v", topic, token.Error())
	}

	return nil
}

// Close disconnects (250ms grace).
func (c *Client) Close() {
	c.inner.Disconnect(250)
}
