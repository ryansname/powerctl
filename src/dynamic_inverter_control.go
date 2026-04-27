package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/ryansname/powerctl/src/governor"
)

const (
	TopicDynamicAutoState = "homeassistant/switch/powerctl_dynamic_auto/state"

	// TopicCarChargingEnabledState is the state topic for the powerctl_car_charging switch.
	TopicCarChargingEnabledState = "homeassistant/switch/powerctl_car_charging/state"

	// TopicCarChargingBattery3CutoffCmd is the command topic for the Battery 3 SOC cutoff number entity.
	TopicCarChargingBattery3CutoffCmd = "powerctl/number/powerctl_car_charging_b3_cutoff/set"

	// TopicCarChargingBattery3CutoffState is the HA statestream topic for the cutoff entity.
	// HA publishes the optimistic entity state here on connect; powerctl reads from this.
	TopicCarChargingBattery3CutoffState = "homeassistant/number/powerctl_car_charging_b3_cutoff/state"

	dynamicMaxDischargeW = 3000.0
	dynamicMaxChargeW    = 3500.0
	dynamicTransferLimit = 4500.0

	// carChargingMinHeadroom requires at least this much transfer-limit headroom to engage.
	carChargingMinHeadroom = 1500.0

	// mpptBoostRampW is the watts added/removed per 1s tick (3W/s).
	mpptBoostRampW = 3.0

	// mpptBoostDeadbandTicks is the number of 1s ticks to hold the offset after throttling clears.
	mpptBoostDeadbandTicks = 60
)

// DynamicInverterConfig holds configuration for the dynamic (Multiplus) inverter controller.
type DynamicInverterConfig struct {
	Input DynamicInputConfig
}

// DynamicInverterState holds runtime state for the dynamic controller.
type DynamicInverterState struct {
	houseLoadMax        governor.RollingMinMax // 1-min max of house load
	houseSideGeneration governor.RollingMinMax // 1-min tracking of solar_1 + inverter_1_9
	mpptBoostOffset     float64               // discharge bias (0–dynamicMaxDischargeW) when MPPTs throttle
	mpptDeadbandTicks   int                   // countdown before ramping down after throttle clears
}

// DynamicDebugInfo contains mode states for the dynamic controller debug output.
type DynamicDebugInfo struct {
	Auto           bool
	Priority       string
	Setpoint       float64
	Headroom       float64
	Battery3SOC    float64
	Safety         bool
	CarCharging    string  // "" = disabled, "active", or gate reason (e.g. "gated: soc")
	MpptThrottling bool
	MpptBoost      float64 // current discharge bias in watts
}

// DynamicModeConstraint encodes a mode's desired setpoint and its allowed range.
// Negative setpoint = discharge; positive = charge.
// Constraint-only modes (TransferLimit, Safety) leave Target=0 (no preference).
// Intent modes (Supply, Charge, CarCharging) set Target to their desired value.
// Combine with add(); call Setpoint() to resolve.
//
// Invariant: at most one mode in a chain has a non-zero Target.
// intentConstraint enforces mutual exclusivity before building the chain.
type DynamicModeConstraint struct {
	Target       float64 // signed desired setpoint; 0 = no preference
	MinCharge    float64 // floor: must charge at least this much (positive; over-limit case)
	MaxDischarge float64 // cap: max discharge allowed (positive magnitude)
	MaxCharge    float64 // cap: max charge allowed
}

// add merges two constraints. Ranges intersect (most restrictive). Targets sum.
// Because at most one mode has a non-zero Target, the sum equals that mode's value.
func (a DynamicModeConstraint) add(b DynamicModeConstraint) DynamicModeConstraint {
	return DynamicModeConstraint{
		Target:       a.Target + b.Target,
		MinCharge:    max(a.MinCharge, b.MinCharge),
		MaxDischarge: min(a.MaxDischarge, b.MaxDischarge),
		MaxCharge:    min(a.MaxCharge, b.MaxCharge),
	}
}

// Setpoint clamps Target to [MinCharge-MaxDischarge, MaxCharge].
// The floor formula works because MinCharge>0 implies MaxDischarge=0:
// over-limit sets MinCharge and zeroes MaxDischarge; normal does the reverse.
func (c DynamicModeConstraint) Setpoint() float64 {
	return clamp(c.Target, c.MinCharge-c.MaxDischarge, c.MaxCharge)
}

func clamp(v, lo, hi float64) float64 { return max(lo, min(hi, v)) }

// transferLimitConstraint returns the range constraint enforcing the 4.5kW transfer limit.
// When over the limit, MaxDischarge=0 and MinCharge>0 (must absorb excess).
// When under the limit, MaxDischarge is capped to available headroom.
func transferLimitConstraint(solar1, inverter1to9 float64) DynamicModeConstraint {
	headroom := dynamicTransferLimit - solar1 - inverter1to9
	if headroom < 0 {
		return DynamicModeConstraint{
			MinCharge:    min(-headroom, dynamicMaxChargeW),
			MaxDischarge: 0,
			MaxCharge:    dynamicMaxChargeW,
		}
	}
	return DynamicModeConstraint{
		MaxDischarge: min(headroom, dynamicMaxDischargeW),
		MaxCharge:    dynamicMaxChargeW,
	}
}

// safetyConstraint returns a range constraint that blocks discharge when active
// (high AC frequency or grid-off with high Powerwall). Charging remains allowed.
func safetyConstraint(active bool) DynamicModeConstraint {
	if active {
		return DynamicModeConstraint{MaxDischarge: 0, MaxCharge: dynamicMaxChargeW}
	}
	return DynamicModeConstraint{MaxDischarge: dynamicMaxDischargeW, MaxCharge: dynamicMaxChargeW}
}

// carChargingSetpoint returns the desired Multiplus setpoint for car-charging mode
// (negative = discharge) along with a short status string. Returns 0 setpoint if any
// gate fails. Does not evaluate safety/tariff — those are handled downstream.
func carChargingSetpoint(input DynamicInput) (float64, string) {
	if input.CarBattery3Cutoff > 0 && input.Battery3SOC < input.CarBattery3Cutoff {
		return 0, "gated: b3 soc"
	}
	solarProducing := (input.Solar1Power + input.Solar2Power) > 200
	if !solarProducing && (input.CarBattery3Cutoff <= 0 || input.Battery3SOC < input.CarBattery3Cutoff) {
		return 0, "gated: no production"
	}
	headroom := dynamicTransferLimit - input.Solar1Power - input.Inverter1to9Power
	if headroom < carChargingMinHeadroom {
		return 0, "gated: headroom"
	}
	return -dynamicMaxDischargeW, "active"
}

// intentConstraint determines the winning control intent and returns it as a DynamicModeConstraint.
// Car charging overrides supply/charge when eligible. Peak tariff suppresses charge intent.
// Returns the constraint, the priority label, and the car-charging status string.
func intentConstraint(input DynamicInput, state *DynamicInverterState) (DynamicModeConstraint, string, string) {
	target := state.houseLoadMax.Max() - (input.Solar1Power + input.Solar2Power + input.Inverter1to9Power)

	var c DynamicModeConstraint
	var priority string
	switch {
	case target > 0:
		// Priority 2: Supply — discharge to fill gap
		c = DynamicModeConstraint{Target: -target, MaxDischarge: dynamicMaxDischargeW, MaxCharge: dynamicMaxChargeW}
		priority = "Supply"
	case input.Tariff == TariffPeak:
		// On-peak: suppress charge intent; Target stays 0 (no discharge forced either)
		c = DynamicModeConstraint{MaxDischarge: dynamicMaxDischargeW, MaxCharge: dynamicMaxChargeW}
		if input.Rebate {
			priority = "Peak+"
		} else {
			priority = "Peak"
		}
	default:
		// Priority 3: Charge from Surplus — absorb only the surplus, not all generation
		c = DynamicModeConstraint{Target: min(-target, dynamicMaxChargeW), MaxDischarge: dynamicMaxDischargeW, MaxCharge: dynamicMaxChargeW}
		priority = "Charge"
	}

	// Car charging overrides the default intent when eligible
	carStatus := ""
	if input.CarChargingEnabled {
		_, reason := carChargingSetpoint(input)
		carStatus = reason
		if reason == "active" {
			c = DynamicModeConstraint{Target: -dynamicMaxDischargeW, MaxDischarge: dynamicMaxDischargeW, MaxCharge: dynamicMaxChargeW}
			priority = "CarCharge"
		}
	}
	return c, priority, carStatus
}

// calculateDynamicSetpoint computes the desired Multiplus setpoint from a DynamicInput.
// Returns the clamped setpoint and debug info. Updates state as a side effect.
func calculateDynamicSetpoint(
	input DynamicInput,
	state *DynamicInverterState,
) (float64, DynamicDebugInfo) {
	state.houseLoadMax.Update(input.HouseLoad)
	state.houseSideGeneration.Update(input.Solar1Power + input.Inverter1to9Power)

	headroom := dynamicTransferLimit - input.Solar1Power - input.Inverter1to9Power

	// Safety: high frequency or grid-off with high Powerwall → no discharge.
	// Charging is still allowed so excess generation is absorbed rather than wasted.
	isSafety := input.ACFreqP100_5Min > 52.75 || (!input.GridAvailable && input.PowerwallSOC > 90.0)

	// MPPT boost: ramp a discharge bias at 100W/30s every 1s tick.
	// Anti-windup: only ramp up when discharge is possible (headroom > 0, no safety event).
	// A deadband holds the offset for 60s after throttling clears.
	switch {
	case input.MpptThrottling && !isSafety && headroom > 0:
		state.mpptBoostOffset += mpptBoostRampW
		state.mpptDeadbandTicks = mpptBoostDeadbandTicks
	case state.mpptDeadbandTicks > 0:
		state.mpptDeadbandTicks--
	default:
		state.mpptBoostOffset -= mpptBoostRampW
	}
	state.mpptBoostOffset = clamp(state.mpptBoostOffset, 0, dynamicMaxDischargeW)

	intent, priority, carStatus := intentConstraint(input, state)

	// Apply MPPT boost: push intent target toward discharge. Safety and transfer-limit
	// constraints in the composition chain still clamp the result correctly.
	intent.Target -= state.mpptBoostOffset

	tl   := transferLimitConstraint(input.Solar1Power, input.Inverter1to9Power)
	sfty := safetyConstraint(isSafety)
	if isSafety {
		priority = "Safety"
	}

	// Compose: intent (single non-zero Target) → hard range constraints (Target=0).
	// tl and sfty narrow the allowed range without changing the target sum.
	setpoint := intent.add(sfty).add(tl).Setpoint()

	return setpoint, DynamicDebugInfo{
		Priority:       priority,
		Setpoint:       setpoint,
		Headroom:       headroom,
		Battery3SOC:    input.Battery3SOC,
		Safety:         isSafety,
		CarCharging:    carStatus,
		MpptThrottling: input.MpptThrottling,
		MpptBoost:      state.mpptBoostOffset,
	}
}

// dynamicInverterControl actively manages the Multiplus II setpoint.
// In auto mode it calculates the setpoint; in manual mode it passes through the HA value.
// Always publishes to Cerbo every 5 seconds (no zero-setpoint exception).
func dynamicInverterControl(
	ctx context.Context,
	inputChan <-chan DynamicInput,
	sender *MQTTSender,
	debugChan chan<- DynamicDebugInfo,
) {
	log.Println("Dynamic inverter control started")

	state := &DynamicInverterState{
		houseLoadMax:        governor.NewRollingMinMaxSeconds(60),
		houseSideGeneration: governor.NewRollingMinMaxSeconds(60),
	}

	var lastSetpoint float64
	var prevCarChargingActive bool
	var carChargingActiveSeen bool
	var prevCarChargingEnabled bool

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	send := func(setpoint float64) {
		payload, _ := json.Marshal(map[string]float64{"value": setpoint})
		sender.Send(MQTTMessage{Topic: TopicMultiplusSetpointWrite, Payload: payload, QoS: 0})
	}

	disableCarCharging := func(reason string) {
		log.Printf("Car charging auto-disable: %s\n", reason)
		sender.Send(MQTTMessage{
			Topic:   TopicCarChargingEnabledState,
			Payload: []byte("OFF"),
			QoS:     1,
			Retain:  true,
		})
	}

	for {
		select {
		case input := <-inputChan:
			autoSetpoint, debug := calculateDynamicSetpoint(input, state)
			debug.Auto = input.DynamicAutoEnabled

			// Car charging auto-disable state machine (setpoint logic is inside calculateDynamicSetpoint).
			if input.DynamicAutoEnabled && input.CarChargingEnabled {
				switch {
				case input.CarBattery3Cutoff > 0 && input.Battery3SOC < input.CarBattery3Cutoff:
					disableCarCharging(fmt.Sprintf("Battery 3 SOC %.1f%% below cutoff %.1f%%", input.Battery3SOC, input.CarBattery3Cutoff))
				case carChargingActiveSeen && prevCarChargingActive && !input.CarChargingActive:
					disableCarCharging("car charger stopped charging")
				}
			}

			// Press force-data-update on the car when charging is first enabled.
			if input.CarChargingEnabled && !prevCarChargingEnabled {
				sender.CallService("button", "press", "button.plb942_force_data_update", nil)
			}

			// Track edges (independent of toggle state).
			prevCarChargingActive = input.CarChargingActive
			prevCarChargingEnabled = input.CarChargingEnabled
			carChargingActiveSeen = true

			if input.DynamicAutoEnabled {
				lastSetpoint = autoSetpoint
			} else {
				lastSetpoint = input.MultiplusSetpointCmd
				debug.Priority = "Manual"
				debug.Setpoint = lastSetpoint
			}

			if debugChan != nil {
				select {
				case debugChan <- debug:
				default:
				}
			}

		case <-ticker.C:
			send(lastSetpoint)

		case <-ctx.Done():
			log.Println("Dynamic inverter control stopped")
			return
		}
	}
}
