package main

import (
	"context"
	"log"
	"os"
	"strconv"
	"time"

	"github.com/BurntSushi/toml"
)

// durableKind is the value type of a persisted control entity.
type durableKind int

const (
	kindBool durableKind = iota
	kindFloat
	kindString
)

// TopicHAStatus is Home Assistant's birth/will topic. HA publishes "online" on
// (re)connect and "offline" when it drops. Default for the MQTT integration;
// verify against the integration's birth-message config if it's been changed.
const TopicHAStatus = "homeassistant/status"

// HA's default MQTT switch state payloads.
const (
	payloadOn  = "ON"
	payloadOff = "OFF"
)

// durableEntry describes one powerctl-owned control entity whose state must
// survive a cold start. powerctl emits both the discovery config and the state
// for these, so the TOML state file is the source of truth: on (re)connect and
// on HA birth powerctl republishes the state; on change it rewrites the file.
//
// Deliberately excludes calibration (already retained + persisted by HA's SOC
// sensor), battery SOC (recomputed from calibration + energy meters), the
// inverter 10 setpoint (transient, always seeded to 0) and HA-owned helpers
// like the tank-voltage input_numbers.
type durableEntry struct {
	Key        string
	StateTopic string // homeassistant/.../state; seeded into msgChan and published on restore
	Kind       durableKind
	Fallback   string // used only before the file exists (first run)
}

// durableTopics is the registry of persisted control entities. Bool values are
// stored/published as HA's default switch payloads ("ON"/"OFF").
var durableTopics = []durableEntry{
	{Key: "powerctl_enabled", StateTopic: TopicPowerctlEnabledState, Kind: kindBool, Fallback: payloadOn},
	{Key: "inverter_enabled", StateTopic: TopicPowerhouseInvertersEnabledState, Kind: kindBool, Fallback: payloadOn},
	{Key: "expecting_power_cuts", StateTopic: TopicExpectingPowerCutsState, Kind: kindBool, Fallback: payloadOn},
	{Key: "dynamic_auto", StateTopic: TopicDynamicAutoState, Kind: kindBool, Fallback: payloadOn},
	{Key: "car_charging", StateTopic: TopicCarChargingEnabledState, Kind: kindBool, Fallback: payloadOff},
	{Key: "pw2_discharge_mode", StateTopic: TopicPW2DischargeMode, Kind: kindString, Fallback: PW2DischargeModeAuto},
	{Key: "car_charging_b3_cutoff", StateTopic: TopicCarChargingBattery3CutoffState, Kind: kindFloat, Fallback: "20"},
}

// durableStateTopics is the set of control state topics powerctl owns. The
// sender always publishes these (bypassing the enabled gate and change-detection)
// so HA can be restored on birth even when powerctl is disabled or the value is
// unchanged — reporting entity state is not device control.
var durableStateTopics = func() map[string]bool {
	m := make(map[string]bool, len(durableTopics))
	for _, e := range durableTopics {
		m[e.StateTopic] = true
	}
	return m
}()

// isControlStateTopic reports whether the topic is a powerctl-owned control
// entity's state topic (see durableTopics).
func isControlStateTopic(topic string) bool {
	return durableStateTopics[topic]
}

// LoadStateValues reads the TOML state file into a key→value map. A missing file
// returns an empty map (first run); a malformed file logs and returns empty so a
// corrupt file can't wedge startup.
func LoadStateValues(path string) map[string]string {
	values := map[string]string{}
	data, err := os.ReadFile(path) //nolint:gosec // path from operator-set --data-dir/env, not untrusted input
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("state store: cannot read %s: %v (using defaults)", path, err) //nolint:gosec // operator-set path
		}
		return values
	}
	if err := toml.Unmarshal(data, &values); err != nil {
		log.Printf("state store: cannot parse %s: %v (using defaults)", path, err) //nolint:gosec // operator-set path
		return map[string]string{}
	}
	return values
}

// resolvedValues merges file values over fallbacks for every durable entry, so
// the result always has a value for each key.
func resolvedValues(fileValues map[string]string) map[string]string {
	out := make(map[string]string, len(durableTopics))
	for _, e := range durableTopics {
		if v, ok := fileValues[e.Key]; ok && v != "" {
			out[e.Key] = v
		} else {
			out[e.Key] = e.Fallback
		}
	}
	return out
}

// LoadStateSeeds returns SensorMessages that seed powerctl's own view of the
// durable topics into statsWorker, so it doesn't wait for the startup timeout.
func LoadStateSeeds(path string) []SensorMessage {
	values := resolvedValues(LoadStateValues(path))
	msgs := make([]SensorMessage, 0, len(durableTopics))
	for _, e := range durableTopics {
		msgs = append(msgs, SensorMessage{Topic: e.StateTopic, Value: values[e.Key]})
	}
	return msgs
}

// stateRestoreMessages publishes each durable entity's value to its state topic
// with retain=false. Sent after subscribing so the publish loops back to
// populate powerctl's own view, and received by HA to restore the entity after
// the discovery config re-creates it.
func stateRestoreMessages(values map[string]string) []MQTTMessage {
	msgs := make([]MQTTMessage, 0, len(durableTopics))
	for _, e := range durableTopics {
		msgs = append(msgs, MQTTMessage{
			Topic:   e.StateTopic,
			Payload: []byte(values[e.Key]),
			QoS:     1,
			Retain:  false,
		})
	}
	return msgs
}

// clearRetainedStateMessages purges any stale retained value on the control
// state topics (empty payload, retain=true) — notably car_charging's historical
// retained OFF. Sent once at startup before the first restore.
func clearRetainedStateMessages() []MQTTMessage {
	msgs := make([]MQTTMessage, 0, len(durableTopics))
	for _, e := range durableTopics {
		msgs = append(msgs, MQTTMessage{
			Topic:   e.StateTopic,
			Payload: []byte{},
			QoS:     1,
			Retain:  true,
		})
	}
	return msgs
}

// snapshotDurable reads the current value of each durable topic from DisplayData,
// formatted as it is stored/published. Topics not yet present in the data are
// skipped so a good file value isn't overwritten by a zero before the first
// reading (or the on-connect restore) lands.
func snapshotDurable(data DisplayData) map[string]string {
	out := make(map[string]string, len(durableTopics))
	for _, e := range durableTopics {
		if _, present := data.TopicData[e.StateTopic]; !present {
			continue
		}
		switch e.Kind {
		case kindBool:
			if data.GetBoolean(e.StateTopic) {
				out[e.Key] = payloadOn
			} else {
				out[e.Key] = payloadOff
			}
		case kindFloat:
			out[e.Key] = strconv.FormatFloat(data.GetFloat(e.StateTopic).Current, 'f', -1, 64)
		case kindString:
			out[e.Key] = data.GetString(e.StateTopic)
		}
	}
	return out
}

// writeStateFile writes the values as TOML atomically (temp file + rename).
func writeStateFile(path string, values map[string]string) error {
	buf, err := toml.Marshal(values)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o600); err != nil { //nolint:gosec // path from operator-set --data-dir/env, not untrusted input
		return err
	}
	return os.Rename(tmp, path) //nolint:gosec // paths from operator-set --data-dir/env, not untrusted input
}

// stateStoreWorker persists the durable control state to the TOML file and keeps
// HA in sync. On a ~10s tick it snapshots the durable topics; if any changed it
// rewrites the file atomically and republishes the changed state topics
// (retain=false). The file is the cross-restart source of truth; the on-connect
// and HA-birth restores read it back.
func stateStoreWorker(
	ctx context.Context,
	dataChan <-chan DisplayData,
	path string,
	sender *MQTTSender,
) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	var latest DisplayData
	haveData := false
	lastWritten := LoadStateValues(path)

	for {
		select {
		case data := <-dataChan:
			latest = data
			haveData = true

		case <-ticker.C:
			if !haveData {
				continue
			}
			current := snapshotDurable(latest)
			if len(current) == 0 {
				continue
			}

			// Merge current over the last written snapshot so topics not yet
			// present don't drop keys we already persisted.
			merged := make(map[string]string, len(lastWritten)+len(current))
			for k, v := range lastWritten {
				merged[k] = v
			}
			changed := false
			for k, v := range current {
				if merged[k] != v {
					changed = true
				}
				merged[k] = v
			}
			if !changed {
				continue
			}

			if err := writeStateFile(path, merged); err != nil {
				log.Printf("state store: write %s failed: %v", path, err) //nolint:gosec // operator-set path
				continue
			}
			// Republish only the changed control state so HA stays in sync.
			for _, e := range durableTopics {
				v, ok := current[e.Key]
				if ok && lastWritten[e.Key] != v {
					sender.Send(MQTTMessage{
						Topic:   e.StateTopic,
						Payload: []byte(v),
						QoS:     1,
						Retain:  false,
					})
				}
			}
			lastWritten = merged
			log.Printf("state store: persisted control state to %s", path) //nolint:gosec // operator-set path

		case <-ctx.Done():
			return
		}
	}
}
