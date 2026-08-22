package main

import (
	"context"
	"fmt"
	"log"
	"math"
	"time"
)

// Statestream topics for the lounge AC pair. The IR entity carries desired state; the
// unit's own Daikin module (climate.lounge) is the only source here that reports what the
// AC is actually doing — its state arrives on TopicLoungeACState, shared with the tile
// worker. Temperature is an entity attribute, so those two depend on statestream's
// publish_attributes.
const (
	TopicLoungeACIRState = "homeassistant/climate/lounge_ac_ir/state"
	TopicLoungeACIRTemp  = "homeassistant/climate/lounge_ac_ir/temperature"
	TopicLoungeACTemp    = "homeassistant/climate/lounge/temperature"
)

const (
	loungeACIREntityID   = "climate.lounge_ac_ir"
	loungeACWiFiEntityID = "climate.lounge"

	hvacModeOff = "off"
	// Sentinel the IR state topic is pre-seeded with. Reads as invalid so a quiet AC
	// neither stalls startup nor looks like a command. See preSeededTopics.
	hvacModeUnknown = "unknown"

	// Gap before the unconditional re-send, so the first transfer finishes before the
	// second competes for the same sleepy end device.
	//
	// It also debounces, which is load-bearing: statestream publishes an entity's state
	// and each of its attributes as separate MQTT messages, so a mode change and its new
	// setpoint can land in different broadcasts. Starting a sequence on the half-updated
	// pair is fine only because the rest arrives and supersedes it well inside this
	// window. Shrinking this towards zero would re-send a setpoint nobody asked for.
	loungeACResendDelay = 5 * time.Second

	// Bounds failure only: confirmation succeeds the moment the unit agrees, so a
	// command that lands never waits this out. Needs to exceed the WiFi entity's poll
	// interval, or a good command gets overridden before it can be observed.
	loungeACConfirmTimeout = time.Minute

	// The IR entity steps in 0.5C, the WiFi entity in 1.0C. An exact compare would
	// never match a fractional setpoint, so every half-degree command would fall back.
	loungeACTempTolerance = 0.5

	// An absent setpoint reads as 0 rather than as an error: GetFloat yields a zero
	// value for a non-numeric payload, which is what `temperature` becomes (`None`)
	// while the unit is off. Range-check instead of trusting zero.
	loungeACMinTemp = 5.0
	loungeACMaxTemp = 35.0
)

// LoungeACTopics returns the statestream topics the lounge AC worker needs.
func LoungeACTopics() []string {
	return []string{
		TopicLoungeACIRState,
		TopicLoungeACIRTemp,
		TopicLoungeACState,
		TopicLoungeACTemp,
	}
}

// LoungeACSetting is one climate state: a mode, and a setpoint when the mode has one.
type LoungeACSetting struct {
	Mode    string
	Temp    float64
	HasTemp bool
}

func (s LoungeACSetting) String() string {
	if !s.HasTemp {
		return s.Mode
	}
	return fmt.Sprintf("%s/%.1fC", s.Mode, s.Temp)
}

// Valid reports whether the setting is usable. `off` needs no setpoint; every other mode
// does, since without one there is nothing to command or compare.
func (s LoungeACSetting) Valid() bool {
	if s.Mode == "" || s.Mode == hvacModeUnknown {
		return false
	}
	return s.Mode == hvacModeOff || s.HasTemp
}

// LoungeACInput holds the inputs for one lounge AC evaluation.
type LoungeACInput struct {
	Desired LoungeACSetting // climate.lounge_ac_ir — what was asked for
	Actual  LoungeACSetting // climate.lounge_ac — what the unit reports
}

// ExtractLoungeACInput reads both climate entities from DisplayData.
func ExtractLoungeACInput(data DisplayData) LoungeACInput {
	return LoungeACInput{
		Desired: readLoungeACSetting(data, TopicLoungeACIRState, TopicLoungeACIRTemp),
		Actual:  readLoungeACSetting(data, TopicLoungeACState, TopicLoungeACTemp),
	}
}

func readLoungeACSetting(data DisplayData, modeTopic, tempTopic string) LoungeACSetting {
	mode := data.GetString(modeTopic)
	// With the unit off the setpoint means nothing, and carrying it would make nudging
	// the dial while off compare as a new command — firing an IR frame to turn off an
	// already-off unit.
	if mode == hvacModeOff {
		return LoungeACSetting{Mode: mode}
	}
	temp := data.GetFloat(tempTopic).Current
	if temp < loungeACMinTemp || temp > loungeACMaxTemp {
		// Drop the value rather than carry it: settings are compared by equality to
		// detect a new command, and a fluctuating junk setpoint would look like one.
		return LoungeACSetting{Mode: mode}
	}
	return LoungeACSetting{Mode: mode, Temp: temp, HasTemp: true}
}

// loungeACMatches reports whether the unit reached what was asked of it.
func loungeACMatches(desired, actual LoungeACSetting) bool {
	if desired.Mode != actual.Mode {
		return false
	}
	// With the unit off a setpoint is meaningless, so the mode settles it.
	if desired.Mode == hvacModeOff {
		return true
	}
	if !desired.HasTemp || !actual.HasTemp {
		return false
	}
	return math.Abs(desired.Temp-actual.Temp) <= loungeACTempTolerance
}

// LoungeACAction is a side effect requested by EvaluateLoungeAC.
type LoungeACAction int

const (
	// ActionLoungeACResendIR re-issues the desired command to the IR entity. SmartIR
	// re-transmits on every service call, so this is a second IR transmission even
	// though nothing about the state changed.
	ActionLoungeACResendIR LoungeACAction = iota
	// ActionLoungeACFallbackWiFi issues the equivalent command to the WiFi entity after
	// IR failed to take. Terminal: nothing is watched afterwards.
	ActionLoungeACFallbackWiFi
)

type loungeACPhase int

const (
	loungeACIdle loungeACPhase = iota
	loungeACAwaitingResend
	loungeACConfirming
)

// LoungeACState holds controller state between evaluations.
type LoungeACState struct {
	seeded    bool
	desired   LoungeACSetting
	phase     loungeACPhase
	resendAt  time.Time
	confirmBy time.Time
}

// EvaluateLoungeAC advances the escalation for one tick. Pure: all time comes from now,
// all external state from in, and controller memory lives in state. Spec: specs/lounge-ac.md
func EvaluateLoungeAC(
	state *LoungeACState,
	in LoungeACInput,
	now time.Time,
) []LoungeACAction {
	if !in.Desired.Valid() {
		return nil
	}

	// First valid reading is the baseline, not a command: restarting powerctl must
	// never re-command the AC.
	if !state.seeded {
		state.seeded = true
		state.desired = in.Desired
		state.phase = loungeACIdle
		return nil
	}

	// Only a change in *desired* starts a sequence. Actual moving on its own means
	// someone used the physical remote, and powerctl must not fight them.
	if in.Desired != state.desired {
		state.desired = in.Desired
		state.phase = loungeACAwaitingResend
		state.resendAt = now.Add(loungeACResendDelay)
		return nil
	}

	switch state.phase {
	case loungeACAwaitingResend:
		if now.Before(state.resendAt) {
			return nil
		}
		state.phase = loungeACConfirming
		state.confirmBy = now.Add(loungeACConfirmTimeout)
		return []LoungeACAction{ActionLoungeACResendIR}

	case loungeACConfirming:
		// Succeed as soon as the unit agrees, rather than sitting out the timeout.
		if in.Actual.Valid() && loungeACMatches(state.desired, in.Actual) {
			state.phase = loungeACIdle
			return nil
		}
		if now.Before(state.confirmBy) {
			return nil
		}
		state.phase = loungeACIdle
		// Nothing trustworthy to compare against is not the same as a failed command:
		// don't fall back on the assumption that IR missed.
		if !in.Actual.Valid() {
			return nil
		}
		return []LoungeACAction{ActionLoungeACFallbackWiFi}
	}

	return nil
}

// loungeACWorker makes lounge AC commands stick. A single IR command is a multi-message
// Zigbee transfer to a sleepy end device that completes about 80% of the time, and SmartIR
// reports success either way — so a miss leaves HA desynced from the unit, and asking
// again for the same thing is deduplicated upstream and sends nothing. This re-sends once
// unconditionally, watches the WiFi entity for confirmation, and falls back to commanding
// it directly when the unit never followed. Spec: specs/lounge-ac.md
func loungeACWorker(ctx context.Context, dataChan <-chan DisplayData, sender *MQTTSender) {
	log.Println("Lounge AC worker started")

	state := &LoungeACState{}

	for {
		select {
		case data := <-dataChan:
			in := ExtractLoungeACInput(data)
			for _, action := range EvaluateLoungeAC(state, in, time.Now()) {
				switch action {
				case ActionLoungeACResendIR:
					log.Printf("Lounge AC: re-sending %v over IR\n", state.desired)
					resendLoungeACIR(sender, state.desired)
				case ActionLoungeACFallbackWiFi:
					log.Printf("Lounge AC: IR unconfirmed after %v (wanted %v, unit reports %v), falling back to WiFi\n",
						loungeACConfirmTimeout, state.desired, in.Actual)
					commandLoungeACWiFi(sender, state.desired)
				}
			}

		case <-ctx.Done():
			log.Println("Lounge AC worker stopped")
			return
		}
	}
}

// resendLoungeACIR re-issues the desired state to the SmartIR entity.
func resendLoungeACIR(sender *MQTTSender, setting LoungeACSetting) {
	if !setting.HasTemp {
		setLoungeACMode(sender, loungeACIREntityID, setting.Mode)
		return
	}
	// One call: SmartIR encodes the whole state into a single IR frame, and every frame
	// is a dozen Zigbee round trips, so splitting this would double the exposure to the
	// packet loss the worker exists to paper over.
	sender.CallService("climate", "set_temperature", loungeACIREntityID, map[string]any{
		"temperature": setting.Temp,
		"hvac_mode":   setting.Mode,
	})
}

// commandLoungeACWiFi issues the desired state to the Daikin after IR failed to take.
func commandLoungeACWiFi(sender *MQTTSender, setting LoungeACSetting) {
	// Two calls, unlike the IR path. Whether set_temperature honours an hvac_mode kwarg
	// is up to the integration — the climate component only forwards it — and a fallback
	// that silently applied the setpoint but not the mode would be worse than none.
	// There is no mesh cost to economise on here.
	setLoungeACMode(sender, loungeACWiFiEntityID, setting.Mode)
	if !setting.HasTemp {
		return
	}
	sender.CallService("climate", "set_temperature", loungeACWiFiEntityID, map[string]any{
		// The Daikin steps in whole degrees, and cooler is preferred.
		"temperature": math.Floor(setting.Temp),
	})
}

func setLoungeACMode(sender *MQTTSender, entityID, mode string) {
	sender.CallService("climate", "set_hvac_mode", entityID, map[string]any{"hvac_mode": mode})
}
