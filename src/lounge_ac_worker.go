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

	// The escalation schedule, all measured from the desired change rather than chained
	// off each other, so the whole sequence is over in a bounded 14 seconds no matter how
	// any single step goes.
	//
	// The 5s lead-in is also a debounce, which is load-bearing: statestream publishes an
	// entity's state and each of its attributes as separate MQTT messages, so a mode
	// change and its new setpoint can land in different broadcasts. Starting a sequence on
	// the half-updated pair is fine only because the rest arrives and supersedes it well
	// inside this window. Shrinking this towards zero would re-send a setpoint nobody
	// asked for.
	//
	// Each refresh is followed by 2s of slack — a LAN round trip to the Daikin module and
	// the statestream hop back — so the step after it decides on a current reading rather
	// than the stale one the refresh was asked to replace.
	loungeACFirstRefresh  = 5 * time.Second
	loungeACResendIRAt    = 7 * time.Second
	loungeACSecondRefresh = 12 * time.Second
	loungeACFallbackAt    = 14 * time.Second

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
	// ActionLoungeACRefreshWiFi asks HA to poll the Daikin module now, instead of waiting
	// out its own poll interval, so the re-send decision is made against a current reading.
	ActionLoungeACRefreshWiFi LoungeACAction = iota
	// ActionLoungeACResendIR re-issues the desired command to the IR entity. SmartIR
	// re-transmits on every service call, so this is a second IR transmission even
	// though nothing about the state changed.
	ActionLoungeACResendIR
	// ActionLoungeACFallbackWiFi issues the equivalent command to the WiFi entity after
	// IR failed to take. Terminal: nothing is watched afterwards.
	ActionLoungeACFallbackWiFi
)

// loungeACStep is one entry in the escalation schedule.
type loungeACStep struct {
	after  time.Duration // from the desired change
	action LoungeACAction
}

// loungeACSchedule is the whole escalation: poll, re-send, poll again, then give up on IR
// and drive the unit directly. Each poll exists to make the step after it a decision on a
// current reading; a match at any point ends the sequence before the rest of it runs.
var loungeACSchedule = []loungeACStep{
	{loungeACFirstRefresh, ActionLoungeACRefreshWiFi},
	{loungeACResendIRAt, ActionLoungeACResendIR},
	{loungeACSecondRefresh, ActionLoungeACRefreshWiFi},
	{loungeACFallbackAt, ActionLoungeACFallbackWiFi},
}

// LoungeACState holds controller state between evaluations.
type LoungeACState struct {
	seeded    bool
	desired   LoungeACSetting
	running   bool      // a sequence is in flight
	startedAt time.Time // when desired last changed
	step      int       // next entry of loungeACSchedule to fire
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
		return nil
	}

	// Only a change in *desired* starts a sequence. Actual moving on its own means
	// someone used the physical remote, and powerctl must not fight them.
	if in.Desired != state.desired {
		state.desired = in.Desired
		state.running = true
		state.startedAt = now
		state.step = 0
		return nil
	}

	if !state.running {
		return nil
	}

	// Checked before every step, not just after the re-send: the moment the unit is
	// observed doing what was asked there is nothing left to escalate, and each remaining
	// step costs either a poll or a dozen Zigbee round trips.
	if in.Actual.Valid() && loungeACMatches(state.desired, in.Actual) {
		state.running = false
		return nil
	}

	if state.step >= len(loungeACSchedule) {
		state.running = false
		return nil
	}

	next := loungeACSchedule[state.step]
	if now.Before(state.startedAt.Add(next.after)) {
		return nil
	}
	// One step per evaluation. A stalled broadcast can leave several due at once, and
	// firing them together would put two IR frames on the mesh in the same instant —
	// exactly the collision the schedule's spacing exists to avoid.
	state.step++
	if state.step >= len(loungeACSchedule) {
		state.running = false
	}

	// Nothing trustworthy to compare against is not the same as a failed command: don't
	// take over the unit on the assumption that IR missed.
	if next.action == ActionLoungeACFallbackWiFi && !in.Actual.Valid() {
		return nil
	}
	return []LoungeACAction{next.action}
}

// loungeACWorker makes lounge AC commands stick. A single IR command is a multi-message
// Zigbee transfer to a sleepy end device that completes about 80% of the time, and SmartIR
// reports success either way — so a miss leaves HA desynced from the unit, and asking
// again for the same thing is deduplicated upstream and sends nothing. This polls the WiFi
// entity for the truth, re-sends once if it disagrees, watches for confirmation, and falls
// back to commanding the WiFi entity directly when the unit never followed.
// Spec: specs/lounge-ac.md
func loungeACWorker(ctx context.Context, dataChan <-chan DisplayData, sender *MQTTSender) {
	log.Println("Lounge AC worker started")

	state := &LoungeACState{}
	var last LoungeACInput
	var seen, wasRunning bool

	for {
		select {
		case data := <-dataChan:
			in := ExtractLoungeACInput(data)
			// Both entities are logged on change. The IR entity is the only thing that
			// starts a sequence and the WiFi entity is the only thing that can end one
			// early, so between them these two lines explain every decision below.
			switch {
			case !seen:
				log.Printf("Lounge AC: baseline is %v, unit reports %v\n", in.Desired, in.Actual)
				seen = true
			default:
				if in.Desired != last.Desired {
					log.Printf("Lounge AC: requested %v (was %v)\n", in.Desired, last.Desired)
				}
				if in.Actual != last.Actual {
					log.Printf("Lounge AC: unit reports %v (was %v)\n", in.Actual, last.Actual)
				}
			}
			last = in

			actions := EvaluateLoungeAC(state, in, time.Now())
			for _, action := range actions {
				switch action {
				case ActionLoungeACRefreshWiFi:
					log.Printf("Lounge AC: polling %s for the truth about %v\n",
						loungeACWiFiEntityID, state.desired)
					refreshLoungeACWiFi(sender)
				case ActionLoungeACResendIR:
					log.Printf("Lounge AC: unit still reports %v, re-sending %v over IR\n",
						in.Actual, state.desired)
					resendLoungeACIR(sender, state.desired)
				case ActionLoungeACFallbackWiFi:
					log.Printf("Lounge AC: IR unconfirmed after %v (wanted %v, unit reports %v), commanding %s directly\n",
						loungeACFallbackAt, state.desired, in.Actual, loungeACWiFiEntityID)
					commandLoungeACWiFi(sender, state.desired)
				}
			}

			// The quiet outcomes are the common ones, so say them: without this a
			// sequence that succeeded and one that was never started look identical.
			if wasRunning && !state.running && len(actions) == 0 {
				if loungeACMatches(state.desired, in.Actual) {
					log.Printf("Lounge AC: %v confirmed, sequence done\n", state.desired)
				} else {
					log.Printf("Lounge AC: giving up on %v, unit reports %v\n", state.desired, in.Actual)
				}
			}
			wasRunning = state.running

		case <-ctx.Done():
			log.Println("Lounge AC worker stopped")
			return
		}
	}
}

// refreshLoungeACWiFi forces HA to poll the Daikin module now. Not a climate service:
// `homeassistant.update_entity` drives the integration's own update, so it reports what
// the unit is doing rather than changing it.
func refreshLoungeACWiFi(sender *MQTTSender) {
	sender.CallService("homeassistant", "update_entity", loungeACWiFiEntityID, nil)
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
