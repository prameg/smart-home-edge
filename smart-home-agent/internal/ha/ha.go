// Package ha bridges to Home Assistant Core through the Supervisor proxy. An
// add-on with `homeassistant_api: true` gets a SUPERVISOR_TOKEN and can reach
// Core at http://supervisor/core/api (REST) and ws://supervisor/core/websocket
// (events) without knowing the user's own long-lived token.
//
// The agent uses two capabilities:
//   - outbound: call HA services (turn a light on, set a climate temperature)
//     to apply downlink commands and desired-shadow state.
//   - inbound: subscribe to `state_changed` events to produce uplink telemetry.
package ha

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/smart-home/edge/agent/internal/config"
	"github.com/smart-home/edge/agent/internal/contract"
)

// Client is a Supervisor-proxied Home Assistant client.
type Client struct {
	cfg  *config.Config
	http *http.Client
	log  *slog.Logger
}

// StateChange is a single normalized HA state_changed event.
type StateChange struct {
	EntityID    string         `json:"entity_id"`
	State       string         `json:"state"`
	Attributes  map[string]any `json:"attributes"`
	LastChanged time.Time      `json:"last_changed"`
}

// New builds an HA client.
func New(cfg *config.Config, log *slog.Logger) *Client {
	return &Client{
		cfg:  cfg,
		http: &http.Client{Timeout: 15 * time.Second},
		log:  log,
	}
}

// Version returns the running Home Assistant Core version (best-effort; empty on
// failure so provisioning metadata stays optional).
func (c *Client) Version(ctx context.Context) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.HARestBaseURL+"/config", nil)
	if err != nil {
		return ""
	}

	c.authorize(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()

	var body struct {
		Version string `json:"version"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return ""
	}

	return body.Version
}

// CallService invokes an HA service (e.g. domain "light", service "turn_on").
// `data` carries entity_id targeting plus any service params.
func (c *Client) CallService(ctx context.Context, domain, service string, data map[string]any) error {
	body, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("ha: encode service data: %w", err)
	}

	url := fmt.Sprintf("%s/services/%s/%s", c.cfg.HARestBaseURL, domain, service)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("ha: build service request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	c.authorize(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("ha: service call failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("ha: service %s.%s returned status %d", domain, service, resp.StatusCode)
	}

	return nil
}

// Test-light entities. A backing input_boolean stores the on/off state (the
// cloud ignores that domain), wrapped in a template light so an actual light.*
// entity — which the cloud does register — exists without any hardware.
const (
	testToggleObjectID = "bed_lights"
	testLightEntity    = "light.bed_light"
)

// CreateTestLight provisions a controllable template light (light.bed_light)
// for local/VM testing where there is no real hardware, so the cloud has a
// device to register and control after the next inventory publish. It is
// idempotent: entities that already exist are left untouched. Runs entirely over
// the Supervisor-proxied HA APIs (WS registry + REST config-flow) — no external
// script or extra token needed inside the add-on.
func (c *Client) CreateTestLight(ctx context.Context) error {
	if err := c.ensureTestToggle(ctx); err != nil {
		return err
	}

	return c.ensureTestTemplateLight(ctx)
}

// ensureTestToggle creates the input_boolean that stores the light's on/off
// state (HA slugifies the name into input_boolean.bed_lights).
func (c *Client) ensureTestToggle(ctx context.Context) error {
	if _, err := c.State(ctx, "input_boolean."+testToggleObjectID); err == nil {
		return nil // already exists
	}

	if _, err := c.wsCommand(ctx, "input_boolean/create", map[string]any{
		"name": "Bed Lights",
		"icon": "mdi:flashlight",
	}); err != nil {
		return fmt.Errorf("ha: create test toggle: %w", err)
	}

	return nil
}

// ensureTestTemplateLight drives the template integration's config-flow to build
// light.bed_light backed by the input_boolean above.
func (c *Client) ensureTestTemplateLight(ctx context.Context) error {
	if _, err := c.State(ctx, testLightEntity); err == nil {
		return nil // already exists
	}

	flow, err := c.restPost(ctx, "/config/config_entries/flow", map[string]any{"handler": "template"})
	if err != nil {
		return fmt.Errorf("ha: start template flow: %w", err)
	}

	flowID, ok := flow["flow_id"].(string)
	if !ok || flowID == "" {
		return fmt.Errorf("ha: template flow returned no flow_id")
	}

	if _, err := c.restPost(ctx, "/config/config_entries/flow/"+flowID, map[string]any{"next_step_id": "light"}); err != nil {
		return fmt.Errorf("ha: select template light step: %w", err)
	}

	toggle := "input_boolean." + testToggleObjectID
	result, err := c.restPost(ctx, "/config/config_entries/flow/"+flowID, map[string]any{
		"name":  "Bed Light",
		"state": fmt.Sprintf("{{ is_state('%s', 'on') }}", toggle),
		"turn_on": map[string]any{
			"action": "input_boolean.turn_on",
			"target": map[string]any{"entity_id": toggle},
		},
		"turn_off": map[string]any{
			"action": "input_boolean.turn_off",
			"target": map[string]any{"entity_id": toggle},
		},
	})
	if err != nil {
		return fmt.Errorf("ha: finish template light flow: %w", err)
	}

	if result["type"] != "create_entry" {
		return fmt.Errorf("ha: template light flow did not complete: %v", result["type"])
	}

	return nil
}

// restPost sends a JSON POST to an HA REST path and decodes the JSON object
// response. Used for the multi-step config-entries flow that has no WS command.
func (c *Client) restPost(ctx context.Context, path string, data map[string]any) (map[string]any, error) {
	body, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("ha: encode %s: %w", path, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.HARestBaseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("ha: build %s: %w", path, err)
	}

	req.Header.Set("Content-Type", "application/json")
	c.authorize(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ha: %s: %w", path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("ha: %s returned status %d", path, resp.StatusCode)
	}

	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("ha: decode %s: %w", path, err)
	}

	return out, nil
}

// CreatePersistentNotification posts (or updates, by stable notificationID) a
// notification in the HA UI — the agent uses it to show the claim code so a
// user never has to read add-on logs. Best-effort: a failure is logged at debug
// and swallowed, because surfacing the code must never block provisioning.
func (c *Client) CreatePersistentNotification(ctx context.Context, notificationID, title, message string) {
	if err := c.CallService(ctx, "persistent_notification", "create", map[string]any{
		"notification_id": notificationID,
		"title":           title,
		"message":         message,
	}); err != nil {
		c.log.Debug("create persistent notification failed", "notification_id", notificationID, "error", err)
	}
}

// DismissPersistentNotification clears a notification previously created with
// the same id (on claim). Best-effort, like its create counterpart.
func (c *Client) DismissPersistentNotification(ctx context.Context, notificationID string) {
	if err := c.CallService(ctx, "persistent_notification", "dismiss", map[string]any{
		"notification_id": notificationID,
	}); err != nil {
		c.log.Debug("dismiss persistent notification failed", "notification_id", notificationID, "error", err)
	}
}

// State fetches the current state of a single entity over the REST API. It is
// the read-back the agent uses to report HA's ACTUAL state after applying a
// desired shadow (rather than echoing the desired document) and to reconcile
// reported state on (re)connect. Returns an error if the entity is unknown
// (HA answers 404) so the caller can skip reporting a state that does not exist.
func (c *Client) State(ctx context.Context, entityID string) (StateChange, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.HARestBaseURL+"/states/"+entityID, nil)
	if err != nil {
		return StateChange{}, fmt.Errorf("ha: build state request: %w", err)
	}

	c.authorize(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return StateChange{}, fmt.Errorf("ha: state fetch failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return StateChange{}, fmt.Errorf("ha: state %s returned status %d", entityID, resp.StatusCode)
	}

	var st haState
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return StateChange{}, fmt.Errorf("ha: decode state: %w", err)
	}

	return StateChange{
		EntityID:    st.EntityID,
		State:       st.State,
		Attributes:  st.Attributes,
		LastChanged: st.LastChanged,
	}, nil
}

// EntityInventory returns the gateway's controllable/observable HA entities by
// reading the entity + device + area registries over the WebSocket API. The
// cloud filters this to allow-listed domains and upserts a device per entity
// (agent -> cloud device sync).
//
// Per entity: Name prefers the user override, then the integration's original
// name, then the entity_id. Area is the entity-level override, falling back to
// the entity's device's area (HA's own inheritance). The HA* grouping metadata
// (device id/name, device_class, unit_of_measurement) is HA's exact registry
// keys, best-effort — omitted when HA does not provide them.
func (c *Client) EntityInventory(ctx context.Context) ([]contract.InventoryEntity, error) {
	areasRaw, err := c.wsCommand(ctx, "config/area_registry/list", nil)
	if err != nil {
		return nil, err
	}

	devicesRaw, err := c.wsCommand(ctx, "config/device_registry/list", nil)
	if err != nil {
		return nil, err
	}

	entitiesRaw, err := c.wsCommand(ctx, "config/entity_registry/list", nil)
	if err != nil {
		return nil, err
	}

	var areas []struct {
		AreaID string `json:"area_id"`
		Name   string `json:"name"`
	}
	_ = json.Unmarshal(areasRaw, &areas)

	areaName := make(map[string]string, len(areas))
	for _, a := range areas {
		areaName[a.AreaID] = a.Name
	}

	// Device registry: HA's device metadata grouping. name_by_user overrides the
	// integration name; a device inherits an area the entity can fall back to.
	var devices []struct {
		ID         string  `json:"id"`
		Name       *string `json:"name"`
		NameByUser *string `json:"name_by_user"`
		AreaID     *string `json:"area_id"`
	}
	_ = json.Unmarshal(devicesRaw, &devices)

	type deviceMeta struct {
		name   string
		areaID string
	}
	deviceByID := make(map[string]deviceMeta, len(devices))
	for _, d := range devices {
		name := ""
		if d.NameByUser != nil && *d.NameByUser != "" {
			name = *d.NameByUser
		} else if d.Name != nil {
			name = *d.Name
		}

		areaID := ""
		if d.AreaID != nil {
			areaID = *d.AreaID
		}

		deviceByID[d.ID] = deviceMeta{name: name, areaID: areaID}
	}

	var entities []struct {
		EntityID            string  `json:"entity_id"`
		Name                *string `json:"name"`
		OriginalName        *string `json:"original_name"`
		AreaID              *string `json:"area_id"`
		DeviceID            *string `json:"device_id"`
		DeviceClass         *string `json:"device_class"`
		OriginalDeviceClass *string `json:"original_device_class"`
		UnitOfMeasurement   *string `json:"unit_of_measurement"`
		DisabledBy          *string `json:"disabled_by"`
		HiddenBy            *string `json:"hidden_by"`
	}
	_ = json.Unmarshal(entitiesRaw, &entities)

	out := make([]contract.InventoryEntity, 0, len(entities))

	for _, e := range entities {
		// Skip entities the user has disabled or hidden — they are not live.
		if e.EntityID == "" || e.DisabledBy != nil || e.HiddenBy != nil {
			continue
		}

		domain, _, _ := strings.Cut(e.EntityID, ".")

		name := e.EntityID
		if e.Name != nil && *e.Name != "" {
			name = *e.Name
		} else if e.OriginalName != nil && *e.OriginalName != "" {
			name = *e.OriginalName
		}

		haDeviceID := ""
		haDeviceName := ""
		if e.DeviceID != nil {
			haDeviceID = *e.DeviceID
			haDeviceName = deviceByID[haDeviceID].name
		}

		// Area: entity-level override wins, else the device's area.
		areaID := ""
		if e.AreaID != nil && *e.AreaID != "" {
			areaID = *e.AreaID
		} else if haDeviceID != "" {
			areaID = deviceByID[haDeviceID].areaID
		}

		deviceClass := ""
		if e.DeviceClass != nil && *e.DeviceClass != "" {
			deviceClass = *e.DeviceClass
		} else if e.OriginalDeviceClass != nil && *e.OriginalDeviceClass != "" {
			deviceClass = *e.OriginalDeviceClass
		}

		unit := ""
		if e.UnitOfMeasurement != nil {
			unit = *e.UnitOfMeasurement
		}

		out = append(out, contract.InventoryEntity{
			EntityID:          e.EntityID,
			Domain:            domain,
			Name:              name,
			Area:              areaName[areaID],
			HADeviceID:        haDeviceID,
			HADeviceName:      haDeviceName,
			DeviceClass:       deviceClass,
			UnitOfMeasurement: unit,
		})
	}

	return out, nil
}

// SubscribeRegistryChanges emits a signal whenever HA's entity/area/device
// registry changes, so the agent can re-report its inventory. Like
// SubscribeStateChanges it reconnects with backoff and closes the channel only
// when ctx is done. Signals are coalesced (the channel is buffered, size 1):
// the agent re-fetches the full inventory on each signal, so a missed duplicate
// is harmless.
func (c *Client) SubscribeRegistryChanges(ctx context.Context) <-chan struct{} {
	out := make(chan struct{}, 1)

	go func() {
		defer close(out)

		backoff := time.Second

		for ctx.Err() == nil {
			if err := c.streamRegistryOnce(ctx, out); err != nil && ctx.Err() == nil {
				c.log.Warn("ha registry stream ended, reconnecting", "error", err, "backoff", backoff.String())

				select {
				case <-ctx.Done():
					return
				case <-time.After(backoff):
				}

				if backoff < 30*time.Second {
					backoff *= 2
				}

				continue
			}

			backoff = time.Second
		}
	}()

	return out
}

// streamRegistryOnce subscribes to the registry_updated events on one WS session
// and signals `out` on each, until the connection or ctx ends.
func (c *Client) streamRegistryOnce(ctx context.Context, out chan<- struct{}) error {
	if c.cfg.SupervisorToken == "" {
		return fmt.Errorf("missing SUPERVISOR_TOKEN (is homeassistant_api: true set on the add-on?)")
	}

	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+c.cfg.SupervisorToken)

	conn, _, err := websocket.DefaultDialer.DialContext(ctx, c.cfg.HAWebsocketURL, headers)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()

	if err := c.handshake(conn); err != nil {
		return err
	}

	var msgID atomic.Int64
	for _, eventType := range []string{"entity_registry_updated", "area_registry_updated", "device_registry_updated"} {
		if err := conn.WriteJSON(map[string]any{
			"id":         msgID.Add(1),
			"type":       "subscribe_events",
			"event_type": eventType,
		}); err != nil {
			return fmt.Errorf("subscribe %s: %w", eventType, err)
		}
	}

	c.log.Info("subscribed to HA registry_updated events")

	for {
		var frame wsFrame
		if err := conn.ReadJSON(&frame); err != nil {
			return fmt.Errorf("read: %w", err)
		}

		if frame.Type != "event" {
			continue
		}

		select {
		case out <- struct{}{}:
		default: // a signal is already pending; coalesce.
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// wsCommand opens a one-shot WS session, sends a single command (msgType plus
// any extra fields), and returns its result payload. Used for registry snapshots
// (area/entity lists) and one-off registry mutations (creating a helper entity).
func (c *Client) wsCommand(ctx context.Context, msgType string, extra map[string]any) (json.RawMessage, error) {
	if c.cfg.SupervisorToken == "" {
		return nil, fmt.Errorf("missing SUPERVISOR_TOKEN (is homeassistant_api: true set on the add-on?)")
	}

	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+c.cfg.SupervisorToken)

	conn, _, err := websocket.DefaultDialer.DialContext(ctx, c.cfg.HAWebsocketURL, headers)
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()

	if err := c.handshake(conn); err != nil {
		return nil, err
	}

	const id = 1
	msg := map[string]any{"id": id, "type": msgType}
	for k, v := range extra {
		msg[k] = v
	}

	if err := conn.WriteJSON(msg); err != nil {
		return nil, fmt.Errorf("send %s: %w", msgType, err)
	}

	for {
		var frame struct {
			ID      int             `json:"id"`
			Type    string          `json:"type"`
			Success bool            `json:"success"`
			Result  json.RawMessage `json:"result"`
			Error   struct {
				Message string `json:"message"`
			} `json:"error"`
		}

		if err := conn.ReadJSON(&frame); err != nil {
			return nil, fmt.Errorf("read %s: %w", msgType, err)
		}

		if frame.Type == "result" && frame.ID == id {
			if !frame.Success {
				return nil, fmt.Errorf("%s failed: %s", msgType, frame.Error.Message)
			}

			return frame.Result, nil
		}
	}
}

// SubscribeStateChanges opens the HA WebSocket, authenticates, subscribes to
// state_changed, and streams normalized events onto the returned channel until
// ctx is cancelled. It reconnects with backoff on any error; the channel stays
// open across reconnects and is closed only when ctx is done.
func (c *Client) SubscribeStateChanges(ctx context.Context) <-chan StateChange {
	out := make(chan StateChange, 256)

	go func() {
		defer close(out)

		backoff := time.Second

		for ctx.Err() == nil {
			if err := c.streamOnce(ctx, out); err != nil && ctx.Err() == nil {
				c.log.Warn("ha websocket stream ended, reconnecting", "error", err, "backoff", backoff.String())

				select {
				case <-ctx.Done():
					return
				case <-time.After(backoff):
				}

				if backoff < 30*time.Second {
					backoff *= 2
				}

				continue
			}

			backoff = time.Second
		}
	}()

	return out
}

// streamOnce runs a single WebSocket session: auth handshake, subscribe, then
// pump events until the connection or ctx ends.
func (c *Client) streamOnce(ctx context.Context, out chan<- StateChange) error {
	if c.cfg.SupervisorToken == "" {
		return fmt.Errorf("missing SUPERVISOR_TOKEN (is homeassistant_api: true set on the add-on?)")
	}

	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+c.cfg.SupervisorToken)

	conn, _, err := websocket.DefaultDialer.DialContext(ctx, c.cfg.HAWebsocketURL, headers)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	// Close the socket when ctx is cancelled so the blocking ReadJSON returns.
	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()

	if err := c.handshake(conn); err != nil {
		return err
	}

	var msgID atomic.Int64
	subID := msgID.Add(1)

	if err := conn.WriteJSON(map[string]any{
		"id":         subID,
		"type":       "subscribe_events",
		"event_type": "state_changed",
	}); err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}

	c.log.Info("subscribed to HA state_changed events")

	for {
		var frame wsFrame
		if err := conn.ReadJSON(&frame); err != nil {
			return fmt.Errorf("read: %w", err)
		}

		if frame.Type != "event" || frame.Event.EventType != "state_changed" {
			continue
		}

		newState := frame.Event.Data.NewState
		if newState == nil {
			continue // entity removed; nothing to report
		}

		select {
		case out <- StateChange{
			EntityID:    newState.EntityID,
			State:       newState.State,
			Attributes:  newState.Attributes,
			LastChanged: newState.LastChanged,
		}:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// handshake performs the HA auth_required -> auth -> auth_ok exchange. When
// connecting through the Supervisor proxy (ws://supervisor/core/websocket), the
// Bearer token must also be sent on the HTTP upgrade request — see
// https://github.com/home-assistant/supervisor/issues/5028.
func (c *Client) handshake(conn *websocket.Conn) error {
	var hello wsFrame
	if err := conn.ReadJSON(&hello); err != nil {
		return fmt.Errorf("auth hello: %w", err)
	}

	switch hello.Type {
	case "auth_ok":
		return nil
	case "auth_required":
		// continue below
	default:
		return fmt.Errorf("unexpected first frame %q", hello.Type)
	}

	if err := conn.WriteJSON(map[string]string{
		"type":         "auth",
		"access_token": c.cfg.SupervisorToken,
	}); err != nil {
		return fmt.Errorf("auth send: %w", err)
	}

	var result wsFrame
	if err := conn.ReadJSON(&result); err != nil {
		return fmt.Errorf("auth result: %w", err)
	}

	if result.Type != "auth_ok" {
		if result.Message != "" {
			return fmt.Errorf("auth rejected: %s (%s)", result.Type, result.Message)
		}

		return fmt.Errorf("auth rejected: %s", result.Type)
	}

	return nil
}

func (c *Client) authorize(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+c.cfg.SupervisorToken)
	req.Header.Set("Accept", "application/json")
}

// wsFrame is the subset of the HA websocket protocol the agent reads.
type wsFrame struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Event   struct {
		EventType string `json:"event_type"`
		Data      struct {
			EntityID string   `json:"entity_id"`
			NewState *haState `json:"new_state"`
			OldState *haState `json:"old_state"`
		} `json:"data"`
	} `json:"event"`
}

type haState struct {
	EntityID    string         `json:"entity_id"`
	State       string         `json:"state"`
	Attributes  map[string]any `json:"attributes"`
	LastChanged time.Time      `json:"last_changed"`
}
