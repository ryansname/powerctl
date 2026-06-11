package main

import (
	"context"
	"log"
	"time"
)

// TopicPumpSwitchState is the statestream state topic for the water pump switch.
const TopicPumpSwitchState = "homeassistant/switch/pump/state"

// TopicTankFlushModeState is the state topic for the powerctl-owned flush mode binary sensor.
const TopicTankFlushModeState = "powerctl/binary_sensor/powerctl_tank_flush_mode/state"

const (
	pumpEntityID      = "switch.pump"
	pumpTimerEntityID = "timer.pump_time_remaining"
	pumpRunDuration   = "03:00:00"

	pumpStopHeaderPercent       = 90.0 // stop pumping at/above this level (any time)
	pumpStartHeaderPercent      = 75.0 // daily-check start threshold (normal)
	pumpStartFlushHeaderPercent = 15.0 // daily-check start threshold (flush mode)
	pumpStartFloorHeaderPercent = 5.0  // start below this level (any time)

	dailyPumpCheckHour = 11 // daily check fires only during the 11:00 hour, local time

	// Service calls bypass the sender's payload dedupe, so rate-limit them here.
	pumpCommandCooldown = 60 * time.Second
)

// PumpTopics returns the statestream topics the pump control worker needs.
func PumpTopics() []string {
	return []string{
		TopicPumpSwitchState,
		TopicHeaderTankLevelsState,
	}
}

// PumpInput holds the inputs for one pump control evaluation.
type PumpInput struct {
	HeaderPercent float64
	HeaderValid   bool
	PumpOn        bool
}

// ExtractPumpInput reads pump control inputs from DisplayData. The header tank level
// comes from powerctl's own published sensor so decisions match what HA displays.
func ExtractPumpInput(data DisplayData) PumpInput {
	var levels HeaderTankLevels
	data.GetJSON(TopicHeaderTankLevelsState, &levels)
	return PumpInput{
		HeaderPercent: levels.PercentFull,
		// Real readings are roughly 0-100; only the pre-seeded sentinel is far negative.
		HeaderValid: levels.PercentFull > tankLevelSentinel/2,
		PumpOn:      data.GetBoolean(TopicPumpSwitchState),
	}
}

// IsFlushMode reports whether t falls in a tank flush period: the first fortnight
// of every third month (January, April, July, October).
func IsFlushMode(t time.Time) bool {
	if t.Day() > 14 {
		return false
	}
	switch t.Month() {
	case time.January, time.April, time.July, time.October:
		return true
	default:
		return false
	}
}

// PumpAction is a side effect requested by EvaluatePump.
type PumpAction int

const (
	// ActionStartPumpTimer starts the HA pump timer; HA automations turn the pump on
	// while the timer runs and off when it finishes.
	ActionStartPumpTimer PumpAction = iota
	// ActionTurnOffPump turns the pump switch off immediately.
	ActionTurnOffPump
)

// PumpControlState holds pump controller state between evaluations.
type PumpControlState struct {
	lastCheckDay    string    // local date ("2006-01-02") of the last consumed daily check
	lastStartSent   time.Time // cooldowns for service calls
	lastTurnOffSent time.Time
}

// EvaluatePump decides pump actions for one tick. Pure: all time comes from now,
// all external state from in, and controller memory lives in state.
func EvaluatePump(
	state *PumpControlState,
	in PumpInput,
	now time.Time,
) []PumpAction {
	// Invalid tank data never triggers a start or stop. The daily check is not
	// consumed either, so it can still fire if data returns within the 11:00 hour.
	if !in.HeaderValid {
		return nil
	}

	var actions []PumpAction

	// Stop rule: header full, pump still running (any time of day).
	if in.PumpOn && in.HeaderPercent >= pumpStopHeaderPercent {
		if now.Sub(state.lastTurnOffSent) >= pumpCommandCooldown {
			actions = append(actions, ActionTurnOffPump)
			state.lastTurnOffSent = now
		}
	}

	// Floor rule: header critically low (any time of day).
	wantStart := in.HeaderPercent < pumpStartFloorHeaderPercent

	// Daily check: fires at the first valid evaluation during the 11:00 hour, once per
	// day. If powerctl isn't running (or data is invalid) for that whole hour, the
	// day's check is skipped — no catch-up later in the day.
	day := now.Format("2006-01-02")
	if day != state.lastCheckDay && now.Hour() == dailyPumpCheckHour {
		state.lastCheckDay = day
		threshold := pumpStartHeaderPercent
		if IsFlushMode(now) {
			threshold = pumpStartFlushHeaderPercent
		}
		if in.HeaderPercent < threshold {
			wantStart = true
		}
	}

	if wantStart && !in.PumpOn && now.Sub(state.lastStartSent) >= pumpCommandCooldown {
		actions = append(actions, ActionStartPumpTimer)
		state.lastStartSent = now
	}

	return actions
}

// pumpControlWorker keeps the header tank topped up: a once-daily start check during the
// 11:00 hour (deep-drain threshold during flush periods), a critical-low start floor, and
// a header-full stop rule. Starting means starting the HA pump timer — the existing HA
// automations own turning the pump on and remain the failsafe for turning it off.
func pumpControlWorker(ctx context.Context, dataChan <-chan DisplayData, sender *MQTTSender) {
	log.Println("Pump control worker started")

	state := &PumpControlState{}

	for {
		select {
		case data := <-dataChan:
			now := time.Now()

			// Publish flush mode for HA visibility (sender dedupes unchanged payloads).
			flushPayload := "OFF"
			if IsFlushMode(now) {
				flushPayload = "ON"
			}
			sender.Send(MQTTMessage{
				Topic:   TopicTankFlushModeState,
				Payload: []byte(flushPayload),
				QoS:     1,
			})

			in := ExtractPumpInput(data)
			for _, action := range EvaluatePump(state, in, now) {
				switch action {
				case ActionStartPumpTimer:
					log.Printf("Pump control: starting %s for %s (header %.1f%%, flush=%v)\n",
						pumpTimerEntityID, pumpRunDuration, in.HeaderPercent, IsFlushMode(now))
					sender.CallService("timer", "start", pumpTimerEntityID, map[string]any{"duration": pumpRunDuration})
				case ActionTurnOffPump:
					log.Printf("Pump control: header %.1f%% >= %.0f%%, turning pump off\n",
						in.HeaderPercent, pumpStopHeaderPercent)
					sender.CallService("switch", "turn_off", pumpEntityID, nil)
				}
			}

		case <-ctx.Done():
			log.Println("Pump control worker stopped")
			return
		}
	}
}
