package main

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/ryansname/powerctl/src/governor"
)

const (
	TopicDynamicAutoState = "homeassistant/switch/powerctl_dynamic_auto/state"

	dynamicMaxDischargeW = 3000.0
	dynamicMaxChargeW    = 3500.0
	dynamicTransferLimit = 4500.0
)

// DynamicInverterConfig holds configuration for the dynamic (Multiplus) inverter controller.
type DynamicInverterConfig struct {
	Input DynamicInputConfig
}

// DynamicInverterState holds runtime state for the dynamic controller.
type DynamicInverterState struct {
	houseLoadMax        governor.RollingMinMax // 1-min max of house load
	houseSideGeneration governor.RollingMinMax // 1-min tracking of solar_1 + inverter_1_9
}

// DynamicDebugInfo contains mode states for the dynamic controller debug output.
type DynamicDebugInfo struct {
	Auto        bool
	Priority    string
	Setpoint    float64
	Headroom    float64
	Battery3SOC float64
	Safety      bool
}

// applyTransferLimit clamps the desired Multiplus setpoint to enforce the 4.5kW transfer limit.
// Negative setpoint = discharge (Multiplus outputs to house); positive = charge.
func applyTransferLimit(desired, solar1, inverter1to9 float64) float64 {
	headroom := dynamicTransferLimit - solar1 - inverter1to9
	if headroom < 0 {
		// Already over limit: force charging to absorb excess
		charge := -headroom
		if charge > dynamicMaxChargeW {
			charge = dynamicMaxChargeW
		}
		return charge
	}
	// Clamp discharge to available headroom; allow charging up to max
	minSetpoint := -headroom
	if minSetpoint < -dynamicMaxDischargeW {
		minSetpoint = -dynamicMaxDischargeW
	}
	if desired < minSetpoint {
		return minSetpoint
	}
	if desired > dynamicMaxChargeW {
		return dynamicMaxChargeW
	}
	return desired
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

	// Priority 2: Default Supply — discharge to fill gap
	target := state.houseLoadMax.Max() - (input.Solar1Power + input.Solar2Power + input.Inverter1to9Power)
	var desired float64
	var priority string
	if target > 0 {
		desired = -target
		priority = "Default Supply"
	} else {
		// Priority 3: Charge from Surplus — absorb available powerhouse-side generation
		desired = min(input.Solar1Power+input.Inverter1to9Power, dynamicMaxChargeW)
		priority = "Charge from Surplus"
	}

	setpoint := applyTransferLimit(desired, input.Solar1Power, input.Inverter1to9Power)

	// Safety: high frequency or grid-off with high Powerwall → no discharge (setpoint ≥ 0).
	// Charging is still allowed so excess generation is absorbed rather than wasted.
	safety := input.ACFreqP100_5Min > 52.75 || (!input.GridAvailable && input.PowerwallSOC > 90.0)
	if safety {
		if setpoint < 0 {
			setpoint = 0
		}
		priority = "Safety"
	}

	return setpoint, DynamicDebugInfo{
		Priority:    priority,
		Setpoint:    setpoint,
		Headroom:    headroom,
		Battery3SOC: input.Battery3SOC,
		Safety:      safety,
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

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	send := func(setpoint float64) {
		payload, _ := json.Marshal(map[string]float64{"value": setpoint})
		sender.Send(MQTTMessage{Topic: TopicMultiplusSetpointWrite, Payload: payload, QoS: 0})
	}

	for {
		select {
		case input := <-inputChan:
			autoSetpoint, debug := calculateDynamicSetpoint(input, state)
			debug.Auto = input.DynamicAutoEnabled

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
