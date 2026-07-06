// Package mqtt wraps the Eclipse Paho MQTT 3.1.1 client with the exact session
// semantics the contract requires:
//
//   - clean_session=false + a STABLE client id (= gateway uid) so the broker
//     retains the agent's subscriptions and buffers QoS-1 downlink across
//     reboots and network drops.
//   - a retained last-will of "offline" on homes/{uid}/availability, so an
//     ungraceful disconnect self-heals gateways.is_online on the cloud without
//     the agent having to do anything.
//
// On every (re)connect the caller's OnConnect hook re-publishes the retained
// "online" availability, re-subscribes, and reconciles buffered work.
//
// Reconnection is deliberately driven by the agent, not Paho's own
// auto-reconnect: a rotated MQTT password (recovered by the cloud) would make
// Paho retry a dead credential forever, so the agent watches Lost(), inspects
// the connect error (see IsAuthError), and re-provisions before reconnecting. A
// CredentialsProvider lets that rotation take effect on the next connect without
// rebuilding the client.
package mqtt

import (
	"crypto/tls"
	"fmt"
	"log/slog"
	"strings"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
	"github.com/smart-home/edge/agent/internal/contract"
)

// MessageHandler receives a downlink message body for a subscribed topic.
type MessageHandler func(topic string, payload []byte)

// Client is the agent's connection to the cloud broker.
type Client struct {
	inner             paho.Client
	log               *slog.Logger
	availabilityTopic string
	// lost signals a dropped live connection (buffered depth 1; a pending
	// signal is coalesced). The agent's run loop reads it to drive reconnection.
	lost chan error
}

// Options configures the broker connection.
type Options struct {
	Host        string
	Port        int
	TLS         bool
	TLSInsecure bool
	GatewayUID  string // stable client id + topic namespace
	// CredentialsProvider returns the username/password to use on each connect
	// attempt, so a recovered (rotated) password is picked up on reconnect
	// without rebuilding the client. Falls back to the static Username/Password
	// when nil.
	CredentialsProvider func() (username, password string)
	Username            string
	Password            string
	// OnConnect runs on every successful (re)connect, after the will/keepalive
	// are established. This is where the agent publishes "online", subscribes,
	// and reconciles.
	OnConnect func(c *Client)
}

// New builds (but does not connect) the MQTT client.
func New(opts Options, log *slog.Logger) *Client {
	scheme := "tcp"
	if opts.TLS {
		scheme = "ssl"
	}

	availabilityTopic := contract.AvailabilityTopic(opts.GatewayUID)

	c := &Client{log: log, availabilityTopic: availabilityTopic, lost: make(chan error, 1)}

	pahoOpts := paho.NewClientOptions().
		AddBroker(fmt.Sprintf("%s://%s:%d", scheme, opts.Host, opts.Port)).
		SetClientID(opts.GatewayUID).
		SetCleanSession(false).
		// Reconnection is agent-driven (see Lost / connectBroker): auto-reconnect
		// would loop on a stale password forever after a cloud-side rotation.
		SetAutoReconnect(false).
		SetConnectRetry(false).
		SetKeepAlive(30*time.Second).
		SetConnectTimeout(15*time.Second).
		// Retained last-will: an ungraceful drop marks the gateway offline.
		SetBinaryWill(availabilityTopic, []byte(contract.StatusOffline), 1, true)

	if opts.CredentialsProvider != nil {
		pahoOpts.SetCredentialsProvider(func() (string, string) {
			return opts.CredentialsProvider()
		})
	} else {
		pahoOpts.SetUsername(opts.Username).SetPassword(opts.Password)
	}

	if opts.TLS {
		pahoOpts.SetTLSConfig(&tls.Config{
			InsecureSkipVerify: opts.TLSInsecure, //nolint:gosec // dev-only escape hatch, off by default
			MinVersion:         tls.VersionTLS12,
		})
	}

	pahoOpts.SetOnConnectHandler(func(paho.Client) {
		log.Info("connected to broker", "availability_topic", availabilityTopic)

		if opts.OnConnect != nil {
			opts.OnConnect(c)
		}
	})

	pahoOpts.SetConnectionLostHandler(func(_ paho.Client, err error) {
		log.Warn("broker connection lost", "error", err)

		select {
		case c.lost <- err:
		default: // a drop is already pending; coalesce.
		}
	})

	c.inner = paho.NewClient(pahoOpts)

	return c
}

// Connect makes a single connection attempt and returns its result. Auto- and
// connect-retry are off, so a failure surfaces here (rather than being retried
// silently) and the agent decides whether to back off or re-provision.
func (c *Client) Connect() error {
	token := c.inner.Connect()
	token.Wait()

	return token.Error()
}

// Lost returns a channel that receives an error whenever a live connection
// drops. The agent's run loop selects on it to reconnect (re-provisioning first
// when the drop was a credential rejection).
func (c *Client) Lost() <-chan error {
	return c.lost
}

// IsAuthError reports whether a connect error is the broker refusing the
// credentials (CONNACK "bad user name or password" / "not authorized"), as
// opposed to a transient network/broker-down failure. That distinction is what
// lets the agent re-provision on a rotated password instead of backing off
// against a dead credential forever. Paho surfaces these as plain errors, so we
// match on the CONNACK message text.
func IsAuthError(err error) bool {
	if err == nil {
		return false
	}

	msg := strings.ToLower(err.Error())

	return strings.Contains(msg, "not authori") ||
		strings.Contains(msg, "bad user name or password") ||
		strings.Contains(msg, "bad username or password")
}

// Disconnect publishes a graceful retained "offline" and closes the connection.
func (c *Client) Disconnect() {
	// Best-effort graceful offline; the will covers the ungraceful case.
	c.PublishAvailability(contract.StatusOffline)
	c.inner.Disconnect(500)
}

// Publish sends a message. Returns an error only when the publish token fails;
// with QoS 1 the token resolves once the broker PUBACKs (or the client buffers
// it while offline).
func (c *Client) Publish(topic string, payload []byte, qos byte, retain bool) error {
	token := c.inner.Publish(topic, qos, retain, payload)

	// Bounded wait so a slow/absent broker never blocks the agent loop forever;
	// with a persistent session the message is stored and delivered on reconnect.
	if !token.WaitTimeout(5 * time.Second) {
		return nil
	}

	return token.Error()
}

// PublishAvailability publishes the retained liveness status ("online" on
// connect, "offline" on graceful shutdown) to the availability topic.
func (c *Client) PublishAvailability(status string) {
	_ = c.Publish(c.availabilityTopic, []byte(status), 1, true)
}

// Subscribe registers a handler for a topic at the given QoS. Safe to call again
// on reconnect (idempotent at the broker).
func (c *Client) Subscribe(topic string, qos byte, handler MessageHandler) error {
	token := c.inner.Subscribe(topic, qos, func(_ paho.Client, m paho.Message) {
		handler(m.Topic(), m.Payload())
	})
	token.Wait()

	return token.Error()
}

// Unsubscribe removes the broker-side subscriptions for the given topics. Used
// on unclaim to stop the command/shadow downlink; safe if not subscribed.
func (c *Client) Unsubscribe(topics ...string) error {
	token := c.inner.Unsubscribe(topics...)
	token.Wait()

	return token.Error()
}

// IsConnected reports whether the underlying client currently has a live
// connection to the broker.
func (c *Client) IsConnected() bool {
	return c.inner.IsConnected()
}
