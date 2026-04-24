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
	// The entity is optimistic (no state_topic), so powerctl reads commands directly.
	TopicCarChargingBattery3CutoffCmd = "powerctl/number/powerctl_car_charging_battery3_cutoff/set"

	dynamicMaxDischargeW = 3000.0
	dynamicMaxChargeW    = 3500.0
	dynamicTransferLimit = 4500.0

	// carChargingMinHeadroom requires at least this much transfer-limit headroom to engage.
	carChargingMinHeadroom = 1500.0
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
	Auto         bool
	Priority     string
	Setpoint     float64
	Headroom     float64
	Battery3SOC  float64
	Safety       bool
	CarCharging  string // "" = disabled, "active", or gate reason (e.g. "gated: soc")
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
	// Request max discharge; applyTransferLimit clamps to the 4.5kW transfer limit and
	// the 3kW Multiplus discharge cap.
	return applyTransferLimit(-dynamicMaxDischargeW, input.Solar1Power, input.Inverter1to9Power), "active"
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
		priority = "Supply"
	} else {
		// Priority 3: Charge from Surplus — absorb only the surplus, not all generation
		desired = min(-target, dynamicMaxChargeW)
		priority = "Charge"
	}

	// On-peak tariff: prefer exporting over charging. Suppress charge intent;
	// transfer-limit safety can still force a charge if house-side generation exceeds 4.5kW.
	if input.Tariff == TariffPeak && desired > 0 {
		desired = 0
		if input.Rebate {
			priority = "Peak+"
		} else {
			priority = "Peak"
		}
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
	var prevCarChargingActive bool
	var carChargingActiveSeen bool

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
			debug.CarCharging = ""

			// Car charging mode: override auto setpoint with max safe Multiplus discharge.
			// Only active when dynamic auto is on AND car charging toggle is on.
			// Safety (high freq / grid-off + PW>90) takes precedence — don't override if active.
			if input.DynamicAutoEnabled && input.CarChargingEnabled {
				carSetpoint, reason := carChargingSetpoint(input)
				debug.CarCharging = reason
				if !debug.Safety && carSetpoint < autoSetpoint {
					// More discharge than auto would pick — take it.
					autoSetpoint = carSetpoint
					debug.Priority = "CarCharge"
					debug.Setpoint = autoSetpoint
				}

				switch {
				case input.CarBattery3Cutoff > 0 && input.Battery3SOC < input.CarBattery3Cutoff:
					disableCarCharging(fmt.Sprintf("Battery 3 SOC %.1f%% below cutoff %.1f%%", input.Battery3SOC, input.CarBattery3Cutoff))
				case carChargingActiveSeen && prevCarChargingActive && !input.CarChargingActive:
					disableCarCharging("car charger stopped charging")
				}
			}

			// Track car charging edge (independent of toggle state).
			prevCarChargingActive = input.CarChargingActive
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
