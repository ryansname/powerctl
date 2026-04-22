package main

import (
	"log"
	"math"
	"time"

	"github.com/ryansname/powerctl/src/governor"
)

const floatChargingState = "Float Charging"

// PowerRequest represents a power request from a rule.
type PowerRequest struct {
	Name  string
	Watts float64
}

// PowerLimit represents a power limit from a rule.
type PowerLimit struct {
	Name  string
	Watts float64
}

// InverterInfo holds information about a single inverter.
type InverterInfo struct {
	EntityID   string // e.g., "switch.powerhouse_inverter_1_switch_0"
	StateTopic string // e.g., "homeassistant/switch/powerhouse_inverter_1_switch_0/state"
}

// BatteryInverterGroup holds inverters for a single battery.
type BatteryInverterGroup struct {
	Name                 string
	ShortName            string // Short name for display (e.g., "B2")
	Inverters            []InverterInfo
	ChargeStateTopic     string
	SOCTopic             string
	BatteryVoltageTopic  string
	CapacityWh           float64 // Battery capacity in Wh
	SolarMultiplier      float64 // Multiplier for solar forecast
	AvailableEnergyTopic string  // Topic for battery available energy
}

// BatteryOverflowState holds per-battery runtime state for overflow mode.
type BatteryOverflowState struct {
	LastWatts  float64
	InFloat    bool
	Hysteresis *governor.SteppedHysteresis
}

// ModeState represents a mode's value and whether it's contributing to the final selection.
type ModeState struct {
	Name         string
	Watts        float64
	Contributing bool
}

// checkBatteryOverflow returns inverter count for overflow mode using SOC-based hysteresis.
// Requires Float Charging + 100% SOC to enter. Once entered, stays active while in Float.
// Watts can only decrease to prevent inverter flapping.
func checkBatteryOverflow(
	chargeState string,
	soc float64,
	shortName string,
	wattsPerInverter float64,
	state *BatteryOverflowState,
) PowerRequest {
	name := "Overflow (" + shortName + ")"
	inFloat := chargeState == floatChargingState

	if !inFloat {
		state.InFloat = false
		return PowerRequest{Name: name, Watts: 0}
	}

	if !state.InFloat && soc < 100 {
		return PowerRequest{Name: name, Watts: 0}
	}

	count := state.Hysteresis.Update(soc)
	watts := float64(count) * wattsPerInverter

	if !state.InFloat {
		state.InFloat = true
		state.LastWatts = watts
	} else {
		watts = min(watts, state.LastWatts)
		state.LastWatts = watts
	}

	return PowerRequest{Name: name, Watts: watts}
}

// forecastExcessRequest returns the power needed to reach 100% battery by solar end today.
func forecastExcessRequest(
	forecastRemainingWh float64,
	forecast governor.ForecastPeriods,
	availableWh float64,
	wattsPerInverter float64,
	battery BatteryInverterGroup,
	state *governor.ForecastExcessState,
) PowerRequest {
	input := governor.ForecastExcessInput{
		Now:                 time.Now(),
		ForecastRemainingWh: forecastRemainingWh,
		Forecast:            forecast,
		AvailableWh:         availableWh,
		InverterCount:       len(battery.Inverters),
		WattsPerInverter:    wattsPerInverter,
		SolarMultiplier:     battery.SolarMultiplier,
		CapacityWh:          battery.CapacityWh,
		ShortName:           battery.ShortName,
	}
	result := governor.ForecastExcessRequestCore(input, state)
	return PowerRequest{Name: result.Name, Watts: result.Watts}
}

// powerhouseTransferLimit returns the available capacity after accounting for solar generation.
func powerhouseTransferLimit(solar1P90_15Min float64, maxTransferPower float64) PowerLimit {
	return PowerLimit{Name: "PowerhouseTransfer", Watts: maxTransferPower - solar1P90_15Min}
}

// maxPowerRequest returns the PowerRequest with the highest watts.
func maxPowerRequest(a, b PowerRequest) PowerRequest {
	if a.Watts >= b.Watts {
		return a
	}
	return b
}

// calculateInverterCount computes how many inverters are needed for target power.
func calculateInverterCount(targetWatts, wattsPerInverter float64) int {
	if targetWatts <= 0 {
		return 0
	}
	count := int(math.Ceil(targetWatts / wattsPerInverter))
	return min(count, 9)
}

// maxInvertersForSOC returns the max inverters allowed based on SOC percentage.
func maxInvertersForSOC(socPercent float64, hysteresis *governor.SteppedHysteresis) int {
	return hysteresis.Update(socPercent)
}

// applyInverterChanges enables/disables inverters to match the desired count.
func applyInverterChanges(
	currentStates []bool,
	inverters []InverterInfo,
	sender *MQTTSender,
	desiredCount int,
) bool {
	changed := false

	for i, inv := range inverters {
		current := i < len(currentStates) && currentStates[i]
		desired := i < desiredCount

		if current != desired {
			if desired {
				log.Printf("Enabling %s\n", inv.EntityID)
				sender.CallService("switch", "turn_on", inv.EntityID, nil)
			} else {
				log.Printf("Disabling %s\n", inv.EntityID)
				sender.CallService("switch", "turn_off", inv.EntityID, nil)
			}
			changed = true
		}
	}

	return changed
}
