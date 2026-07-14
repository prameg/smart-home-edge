package pairing

import (
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/smart-home/edge/agent/internal/contract"
)

// Publisher is the slice of the local-broker client Z2M needs (satisfied by
// *localmqtt.Client); an interface so tests can fake it.
type Publisher interface {
	Publish(topic string, payload []byte) error
}

// Z2M drives Zigbee2MQTT's bridge API over the gateway-local broker.
// https://www.zigbee2mqtt.io/guide/usage/mqtt_topics_and_messages.html
type Z2M struct {
	pub  Publisher
	base string
	log  *slog.Logger
}

// NewZ2M builds the backend. base is Z2M's base topic ("zigbee2mqtt" unless
// the add-on was reconfigured).
func NewZ2M(pub Publisher, base string, log *slog.Logger) *Z2M {
	if base == "" {
		base = "zigbee2mqtt"
	}

	return &Z2M{pub: pub, base: base, log: log}
}

// BridgeEventTopic is the topic the caller must subscribe (routing the
// payloads into MapBridgeEvent + Manager.HandleBackendEvent).
func (z *Z2M) BridgeEventTopic() string {
	return z.base + "/bridge/event"
}

// Start opens the join window for the given seconds.
func (z *Z2M) Start(seconds int) error {
	return z.permitJoin(seconds)
}

// Stop closes the join window.
func (z *Z2M) Stop() error {
	return z.permitJoin(0)
}

func (z *Z2M) permitJoin(seconds int) error {
	payload, err := json.Marshal(map[string]int{"time": seconds})
	if err != nil {
		return err
	}

	if err := z.pub.Publish(z.base+"/bridge/request/permit_join", payload); err != nil {
		return fmt.Errorf("permit_join: %w", err)
	}

	return nil
}

// bridgeEvent is Z2M's zigbee2mqtt/bridge/event body (the fields we use).
type bridgeEvent struct {
	Type string `json:"type"`
	Data struct {
		FriendlyName string `json:"friendly_name"`
		IEEEAddress  string `json:"ieee_address"`
		Status       string `json:"status"`
		Supported    *bool  `json:"supported"`
		Definition   *struct {
			Model  string `json:"model"`
			Vendor string `json:"vendor"`
		} `json:"definition"`
	} `json:"data"`
}

// MapBridgeEvent translates one bridge/event message into a pairing Event.
// ok=false for event types pairing does not care about (device_leave, ...).
func MapBridgeEvent(raw []byte) (Event, bool) {
	var be bridgeEvent
	if err := json.Unmarshal(raw, &be); err != nil {
		return Event{}, false
	}

	device := &contract.PairingDevice{
		IEEE:         be.Data.IEEEAddress,
		FriendlyName: be.Data.FriendlyName,
	}
	if be.Data.Definition != nil {
		device.Model = be.Data.Definition.Model
		device.Vendor = be.Data.Definition.Vendor
	}

	switch be.Type {
	case "device_joined":
		return Event{Phase: contract.PairingPhaseDeviceFound, Device: device}, true
	case "device_interview":
		switch be.Data.Status {
		case "started":
			return Event{Phase: contract.PairingPhaseInterviewing, Device: device}, true
		case "successful":
			return Event{Phase: contract.PairingPhaseCompleted, Device: device}, true
		case "failed":
			return Event{Phase: contract.PairingPhaseFailed, Device: device, Reason: "interview failed"}, true
		}
	}

	return Event{}, false
}
