package main

import (
	"context"
	"log"
	"math"
	"time"
)

// Light statestream state topics (on/off). The four switch_as_x lights and the
// Shelly dimmer that the lights worker observes and controls.
const (
	TopicLightOutsideState  = "homeassistant/light/outside/state"
	TopicLightKitchenState  = "homeassistant/light/kitchen_light/state"
	TopicLightMumsRoomState = "homeassistant/light/mums_room_light/state"
	TopicLightGarageState   = "homeassistant/light/garage_floodlight/state"
	TopicLightRyansState    = "homeassistant/light/ryans_lights/state"

	// Ryan's lights brightness attribute (0-255), needed for the lerp dim. It is
	// null when the light is off, so it is pre-seeded (see preSeededTopics) to
	// avoid blocking startup and is only read while the light is on.
	TopicLightRyansBrightness = "homeassistant/light/ryans_lights/brightness"
)

// TopicSleepRyanPress is the command topic for the powerctl-owned "Sleep Ryan"
// button. Each press publishes here (momentary); it is routed straight to the
// lights worker rather than through statsWorker so repeated identical payloads
// are still seen as distinct presses.
const TopicSleepRyanPress = "powerctl/button/powerctl_sleep_ryan/press"

const (
	lightOutsideEntity = "light.outside"
	lightKitchenEntity = "light.kitchen_light"
	lightGarageEntity  = "light.garage_floodlight"
	lightRyansEntity   = "light.ryans_lights"

	// A: outside light auto-off delay while it's dark.
	outsideAutoOffDelay = 10 * time.Minute

	// B: kitchen-follows-mum night window starts at 19:30 local.
	kitchenNightStartHour = 19
	kitchenNightStartMin  = 30

	// D: sleep_ryan dim runs a 30-minute linear ramp from full (100%) to off,
	// starting when the button is pressed. The commanded brightness is the lerp
	// target clamped to never exceed the light's current level, so a light that's
	// already dim isn't brightened — it's left until the descending ramp catches
	// up, then followed down to off.
	dimDuration   = 30 * time.Minute
	brightnessMax = 255.0 // HA brightness attribute scale
	// Upward brightness jump (percentage points, tick-to-tick) that counts as a
	// manual increase and re-anchors the ramp. Above reporting jitter, below a
	// deliberate bump.
	reAnchorMarginPct = 3.0
)

// LightsTopics returns the statestream topics the lights worker reads. The sun
// topic is appended separately in main.go (shared with the AC tile worker).
func LightsTopics() []string {
	return []string{
		TopicLightOutsideState,
		TopicLightKitchenState,
		TopicLightMumsRoomState,
		TopicLightGarageState,
		TopicLightRyansState,
		TopicLightRyansBrightness,
	}
}

// LightInput is the externally observed state for one lights evaluation.
type LightInput struct {
	OutsideOn          bool
	KitchenOn          bool
	MumOn              bool
	GarageOn           bool
	RyansOn            bool
	RyansBrightnessPct float64 // current brightness of Ryan's lights, 0-100
	SunBelow           bool
	SleepRyanPressed   bool // a sleep_ryan press arrived since the last evaluation
}

// ExtractLightInput reads the lights worker inputs from DisplayData. The press
// flag is supplied by the worker (it arrives on a separate channel).
func ExtractLightInput(data DisplayData, sleepRyanPressed bool) LightInput {
	return LightInput{
		OutsideOn:          data.GetBoolean(TopicLightOutsideState),
		KitchenOn:          data.GetBoolean(TopicLightKitchenState),
		MumOn:              data.GetBoolean(TopicLightMumsRoomState),
		GarageOn:           data.GetBoolean(TopicLightGarageState),
		RyansOn:            data.GetBoolean(TopicLightRyansState),
		RyansBrightnessPct: data.GetFloat(TopicLightRyansBrightness).Current / brightnessMax * 100,
		SunBelow:           data.GetString(TopicSunState) == "below_horizon",
		SleepRyanPressed:   sleepRyanPressed,
	}
}

// LightAction is a side effect requested by EvaluateLights.
type LightAction int

const (
	ActionOutsideOff         LightAction = iota // A: turn the outside light off
	ActionGarageOn                              // C: garage floodlight on (mirror outside)
	ActionGarageOff                             // C: garage floodlight off (mirror outside)
	ActionKitchenOn                             // B: kitchen on (mum's room woke at night)
	ActionKitchenOff                            // B: kitchen off (mum's room went dark)
	ActionSetRyansBrightness                    // D: set Ryan's lights to state.dimReqPct
	ActionRyansOff                              // D: dim finished, turn Ryan's lights off
)

// LightControlState holds lights controller memory between evaluations.
type LightControlState struct {
	initialized bool

	prevOutsideOn bool
	prevMumOn     bool

	outsideArmed   bool      // A: auto-off pending for the current on-period
	outsideOnSince time.Time // A: when the current on-period began

	kitchenManagedByMum bool // B: powerctl turned the kitchen on; it owns turning it off

	dimActive     bool      // D: a sleep_ryan dim is in progress
	dimStart      time.Time // D: ramp origin (where the 100%→0% lerp starts)
	dimReqPct     int       // D: brightness % to command for ActionSetRyansBrightness
	dimLastReqPct int       // D: last commanded brightness % (-1 = none sent yet)
	dimPrevBNow   float64   // D: previous tick's brightness, to detect manual increases
}

// isKitchenNightWindow approximates Node-RED's "19:30 → sunrise" gate. Current
// code only has sun state (above/below horizon), not the sunrise time, so the
// morning end is approximated as "still dark before noon".
func isKitchenNightWindow(now time.Time, sunBelow bool) bool {
	h, m := now.Hour(), now.Minute()
	afterEvening := h > kitchenNightStartHour || (h == kitchenNightStartHour && m >= kitchenNightStartMin)
	earlyDark := sunBelow && h < 12
	return afterEvening || earlyDark
}

// EvaluateLights decides light actions for one tick. Pure: all time comes from
// now, all external state from in, and controller memory lives in state.
func EvaluateLights(
	state *LightControlState,
	in LightInput,
	now time.Time,
) []LightAction {
	var actions []LightAction

	// D: sleep_ryan 30-minute dim. A press (re)starts the ramp; it follows a
	// linear 100%→0% target, commanding brightness only once the descending ramp
	// has dropped to (or below) the light's current level, and turning the light
	// off at the end.
	if in.SleepRyanPressed {
		state.dimActive = true
		state.dimStart = now
		state.dimLastReqPct = -1
		state.dimPrevBNow = in.RyansBrightnessPct
	}
	if state.dimActive {
		elapsed := now.Sub(state.dimStart)
		switch {
		case !in.RyansOn:
			state.dimActive = false
		case elapsed >= dimDuration:
			actions = append(actions, ActionRyansOff)
			state.dimActive = false
		default:
			bNow := in.RyansBrightnessPct
			// If the light was brightened (an upward jump beyond jitter, vs our
			// own commands which only lower it), re-anchor the ramp origin so the
			// target matches the new level — the dim extends from there rather
			// than yanking the brightness back down.
			if bNow > state.dimPrevBNow+reAnchorMarginPct {
				clamped := min(bNow, 100.0)
				state.dimStart = now.Add(-time.Duration(float64(dimDuration) * (1 - clamped/100)))
				state.dimLastReqPct = -1
				elapsed = now.Sub(state.dimStart)
			}
			state.dimPrevBNow = bNow

			bTarget := 100 * (1 - float64(elapsed)/float64(dimDuration))
			bReq := min(bNow, bTarget)
			// Command only while the ramp is actually pulling the light down
			// (bReq below the current level), and only when the rounded target
			// changes, to avoid redundant service calls.
			// 0% would turn the light off early; the end-of-ramp does that.
			reqPct := max(int(math.Round(bReq)), 1)
			if bReq < bNow-0.5 && reqPct != state.dimLastReqPct {
				state.dimReqPct = reqPct
				state.dimLastReqPct = reqPct
				actions = append(actions, ActionSetRyansBrightness)
			}
		}
	}

	// A: outside auto-off. Arm on the off→on edge; while armed turn off
	// immediately during the day, or after 10 min while it's dark.
	if in.OutsideOn && !state.prevOutsideOn && state.initialized {
		state.outsideArmed = true
		state.outsideOnSince = now
	}
	if !in.OutsideOn {
		state.outsideArmed = false
	}
	if state.outsideArmed && in.OutsideOn {
		if !in.SunBelow || now.Sub(state.outsideOnSince) >= outsideAutoOffDelay {
			actions = append(actions, ActionOutsideOff)
			state.outsideArmed = false
		}
	}

	// B: kitchen follows mum's room at night. Mum on (at night, kitchen off) →
	// kitchen on; mum off → kitchen off but only if powerctl turned it on.
	if state.initialized {
		mumRising := in.MumOn && !state.prevMumOn
		mumFalling := !in.MumOn && state.prevMumOn
		switch {
		case mumRising && isKitchenNightWindow(now, in.SunBelow) && !in.KitchenOn:
			actions = append(actions, ActionKitchenOn)
			state.kitchenManagedByMum = true
		case mumFalling && state.kitchenManagedByMum:
			actions = append(actions, ActionKitchenOff)
			state.kitchenManagedByMum = false
		}
	}

	// C: garage floodlight mirrors the outside light. Command only on change
	// (service calls aren't deduped), and sync once on the first snapshot.
	outsideChanged := in.OutsideOn != state.prevOutsideOn
	if !state.initialized {
		outsideChanged = in.GarageOn != in.OutsideOn
	}
	if outsideChanged {
		if in.OutsideOn {
			actions = append(actions, ActionGarageOn)
		} else {
			actions = append(actions, ActionGarageOff)
		}
	}

	state.prevOutsideOn = in.OutsideOn
	state.prevMumOn = in.MumOn
	state.initialized = true
	return actions
}

// lightsWorker ports the Node-RED "Lights" flow: outside auto-off, kitchen
// following mum's room at night, garage mirroring outside, and the sleep_ryan
// slow dim of Ryan's lights. A/B/C run off DisplayData snapshots; D is triggered
// by presses delivered on pressChan.
func lightsWorker(
	ctx context.Context,
	dataChan <-chan DisplayData,
	pressChan <-chan SensorMessage,
	sender *MQTTSender,
) {
	log.Println("Lights worker started")

	state := &LightControlState{}
	pendingPress := false
	prevDimActive := false

	for {
		select {
		case <-pressChan:
			pendingPress = true

		case data := <-dataChan:
			in := ExtractLightInput(data, pendingPress)
			pendingPress = false

			actions := EvaluateLights(state, in, time.Now())

			// dimActive flips on only when a press lands while the light is on
			// (it's cleared again in the same evaluation if the light is off).
			if !prevDimActive && state.dimActive {
				log.Println("Lights: starting dim of Ryan's lights")
			}
			prevDimActive = state.dimActive

			for _, action := range actions {
				switch action {
				case ActionOutsideOff:
					log.Printf("Lights: auto-off outside (sunBelow=%v)\n", in.SunBelow)
					sender.CallService("light", "turn_off", lightOutsideEntity, nil)
				case ActionGarageOn:
					log.Println("Lights: garage floodlight on (mirror outside)")
					sender.CallService("light", "turn_on", lightGarageEntity, nil)
				case ActionGarageOff:
					log.Println("Lights: garage floodlight off (mirror outside)")
					sender.CallService("light", "turn_off", lightGarageEntity, nil)
				case ActionKitchenOn:
					log.Println("Lights: kitchen on (mum's room woke at night)")
					sender.CallService("light", "turn_on", lightKitchenEntity, nil)
				case ActionKitchenOff:
					log.Println("Lights: kitchen off (mum's room went dark)")
					sender.CallService("light", "turn_off", lightKitchenEntity, nil)
				case ActionSetRyansBrightness:
					sender.CallService("light", "turn_on", lightRyansEntity, map[string]any{
						"brightness_pct": state.dimReqPct,
					})
				case ActionRyansOff:
					log.Println("Lights: dim finished, turning Ryan's lights off")
					sender.CallService("light", "turn_off", lightRyansEntity, nil)
				}
			}

		case <-ctx.Done():
			log.Println("Lights worker stopped")
			return
		}
	}
}
