package agent

import "github.com/smart-home/edge/agent/internal/entitymap"

// serviceCall is a resolved HA service invocation.
type serviceCall struct {
	domain  string
	service string
	data    map[string]any
}

// domainTranslator turns an HA-native desired document (already split into its
// `state` string and `attributes` bag) into zero or more HA service calls for
// one domain. Zero calls means "nothing actuatable in this document".
type domainTranslator func(entityID, stateStr string, attrs map[string]any) []serviceCall

// domainTranslators is the per-domain capability table. Adding a new
// controllable domain is a single entry here (mirrored by the cloud's
// capability definition, which validates the desired payload and picks the UI
// controls). Anything not listed falls back to generic on/off via the
// homeassistant domain.
var domainTranslators = map[string]domainTranslator{
	"light":        translateLight,
	"switch":       translateOnOff("switch"),
	"fan":          translateFan,
	"lock":         translateLock,
	"cover":        translateCover,
	"climate":      translateClimate,
	"media_player": translateMediaPlayer,
}

// translateDesired maps a desired-state document onto the HA service calls that
// realize it. The document is HA-native (the same vocabulary reported state
// uses), so desired and reported are directly comparable:
//
//	{ "state": "on" | "heat" | "open" | ..., "attributes": { "brightness": 200, ... } }
//
// `state` carries the domain's primary target (on/off, hvac mode, lock/cover
// position, ...) and recognized `attributes` become service params. A single
// document can expand to several calls (e.g. climate set-mode + set-temperature),
// which the caller applies in order; an empty result means there was nothing to
// actuate.
func translateDesired(entityID string, state map[string]any) []serviceCall {
	domain := entitymap.DomainOf(entityID)
	stateStr, _ := state["state"].(string)
	attrs, _ := state["attributes"].(map[string]any)

	if translate, ok := domainTranslators[domain]; ok {
		return translate(entityID, stateStr, attrs)
	}

	return translateGeneric(entityID, stateStr, attrs)
}

func translateLight(entityID, stateStr string, attrs map[string]any) []serviceCall {
	if stateStr == "" {
		return nil
	}

	if stateStr == "off" {
		return []serviceCall{{domain: "light", service: "turn_off", data: base(entityID)}}
	}

	data := base(entityID)
	passthrough(data, attrs, "brightness", "brightness_pct", "color_temp", "color_temp_kelvin", "rgb_color", "hs_color", "xy_color", "color_name", "effect", "transition")

	return []serviceCall{{domain: "light", service: "turn_on", data: data}}
}

func translateFan(entityID, stateStr string, attrs map[string]any) []serviceCall {
	if stateStr == "" {
		// A bare speed change without an explicit on/off state.
		if _, ok := attrs["percentage"]; ok {
			data := base(entityID)
			passthrough(data, attrs, "percentage")

			return []serviceCall{{domain: "fan", service: "set_percentage", data: data}}
		}

		return nil
	}

	if stateStr == "off" {
		return []serviceCall{{domain: "fan", service: "turn_off", data: base(entityID)}}
	}

	data := base(entityID)
	passthrough(data, attrs, "percentage", "preset_mode")

	return []serviceCall{{domain: "fan", service: "turn_on", data: data}}
}

func translateLock(entityID, stateStr string, _ map[string]any) []serviceCall {
	switch stateStr {
	case "locked":
		return []serviceCall{{domain: "lock", service: "lock", data: base(entityID)}}
	case "unlocked":
		return []serviceCall{{domain: "lock", service: "unlock", data: base(entityID)}}
	default:
		return nil
	}
}

func translateCover(entityID, stateStr string, attrs map[string]any) []serviceCall {
	// An explicit position wins over the coarse open/closed state.
	if v, ok := attrs["position"]; ok {
		data := base(entityID)
		data["position"] = v

		return []serviceCall{{domain: "cover", service: "set_cover_position", data: data}}
	}

	switch stateStr {
	case "open":
		return []serviceCall{{domain: "cover", service: "open_cover", data: base(entityID)}}
	case "closed":
		return []serviceCall{{domain: "cover", service: "close_cover", data: base(entityID)}}
	default:
		return nil
	}
}

func translateClimate(entityID, stateStr string, attrs map[string]any) []serviceCall {
	var calls []serviceCall

	// The climate state IS the HVAC mode (heat/cool/auto/off/...).
	if stateStr != "" {
		data := base(entityID)
		data["hvac_mode"] = stateStr
		calls = append(calls, serviceCall{domain: "climate", service: "set_hvac_mode", data: data})
	}

	if v, ok := attrs["temperature"]; ok {
		data := base(entityID)
		data["temperature"] = v
		passthrough(data, attrs, "target_temp_high", "target_temp_low")
		calls = append(calls, serviceCall{domain: "climate", service: "set_temperature", data: data})
	}

	return calls
}

func translateMediaPlayer(entityID, stateStr string, attrs map[string]any) []serviceCall {
	var calls []serviceCall

	switch stateStr {
	case "off":
		calls = append(calls, serviceCall{domain: "media_player", service: "turn_off", data: base(entityID)})
	case "on", "idle":
		calls = append(calls, serviceCall{domain: "media_player", service: "turn_on", data: base(entityID)})
	case "playing":
		calls = append(calls, serviceCall{domain: "media_player", service: "media_play", data: base(entityID)})
	case "paused":
		calls = append(calls, serviceCall{domain: "media_player", service: "media_pause", data: base(entityID)})
	}

	if v, ok := attrs["volume_level"]; ok {
		data := base(entityID)
		data["volume_level"] = v
		calls = append(calls, serviceCall{domain: "media_player", service: "volume_set", data: data})
	}

	return calls
}

// translateOnOff builds a plain on/off translator for a domain whose only
// capability is turning on and off (e.g. switch).
func translateOnOff(domain string) domainTranslator {
	return func(entityID, stateStr string, _ map[string]any) []serviceCall {
		if stateStr == "" {
			return nil
		}

		service := "turn_on"
		if stateStr == "off" {
			service = "turn_off"
		}

		return []serviceCall{{domain: domain, service: service, data: base(entityID)}}
	}
}

// translateGeneric actuates on/off for any domain not in the table via the
// homeassistant helper domain (which fans out to the entity's real domain).
func translateGeneric(entityID, stateStr string, _ map[string]any) []serviceCall {
	if stateStr == "" {
		return nil
	}

	service := "turn_on"
	if stateStr == "off" {
		service = "turn_off"
	}

	return []serviceCall{{domain: "homeassistant", service: service, data: base(entityID)}}
}

func base(entityID string) map[string]any {
	return map[string]any{"entity_id": entityID}
}

// passthrough copies recognized attribute keys from the desired document into
// the service params, skipping absent keys.
func passthrough(dst, attrs map[string]any, keys ...string) {
	if attrs == nil {
		return
	}

	for _, key := range keys {
		if v, ok := attrs[key]; ok {
			dst[key] = v
		}
	}
}
