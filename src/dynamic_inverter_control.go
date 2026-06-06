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

	// b3ChargeLimitFullSOC / b3ChargeLimitZeroSOC define the SOC window over which voluntary
	// B3 charging is tapered. Below Full, the Multiplus may charge at full rate. Above Zero,
	// MaxCharge is 0 so only transfer-limit-forced absorption still occurs.
	b3ChargeLimitFullSOC = 60.0
	b3ChargeLimitZeroSOC = 85.0

	// b3DischargeLimitZeroSOC / b3DischargeLimitFullSOC define the Battery 3 SOC window over
	// which max discharge is tapered for low-SOC protection. At/below Zero, MaxDischarge is 0;
	// at/above Full, MaxDischarge is unrestricted. Linear in between, no hysteresis.
	b3DischargeLimitZeroSOC = 10.0
	b3DischargeLimitFullSOC = 20.0

	// pwOffsetMaxW is the extra discharge added to the intent when the Powerwall is fully low.
	// pwOffsetFullSOC / pwOffsetZeroSOC bound the linear ramp: full offset at/below Full SOC,
	// zero at/above Zero SOC. No hysteresis.
	pwOffsetMaxW    = 250.0
	pwOffsetFullSOC = 10.0
	pwOffsetZeroSOC = 20.0

	// cclOverflowHeadroomA is the target margin below CCL (in amps) that the Multiplus
	// maintains by discharging when solar pushes battery current above CCL - margin.
	cclOverflowHeadroomA = 5.0

	// cvlOverflowRampStartV is the voltage offset below CVL where the discharge floor
	// begins to ramp up from zero.
	cvlOverflowRampStartV = 0.20

	// cvlOverflowTargetOffsetV is the voltage offset below CVL where the ramp reaches
	// fraction=1 (floor matches current solar). Above CVL - targetOffset, fraction
	// exceeds 1.0 so the floor over-corrects, dragging voltage back to exactly this
	// point. Steady state settles at CVL - targetOffset.
	cvlOverflowTargetOffsetV = 0.02

	priorityCarCharge = "CarCharge"
	priorityCharge    = "Charge"
	prioritySafety    = "Safety"
)

// DynamicInverterConfig holds configuration for the dynamic (Multiplus) inverter controller.
type DynamicInverterConfig struct {
	Input DynamicInputConfig
}

// DynamicInverterState holds runtime state for the dynamic controller.
type DynamicInverterState struct {
	houseLoadMax        governor.RollingMinMax // 1-min max of house load
	houseSideGeneration governor.RollingMinMax // 1-min tracking of solar_1 + inverter_1_9
	cvlVoltageMax       governor.RollingMinMax // 10s rolling max of Battery 3 voltage; smooths CVL ramp input
}

// DynamicDebugInfo contains mode states for the dynamic controller debug output.
type DynamicDebugInfo struct {
	Auto         bool
	Priority     string
	Setpoint     float64
	Headroom     float64
	Battery3SOC  float64
	Safety       bool
	CarCharging  string  // "" = disabled, "active", or gate reason (e.g. "gated: soc")
	CCLOverflowW    float64 // watts the CCL-overflow constraint requires as minimum discharge
	CVLOverflowW    float64 // watts the CVL-overflow constraint requires as minimum discharge
	B3ChargeMaxW    float64 // max charge W from SOC lerp (dynamicMaxChargeW when unrestricted)
	B3DischargeMaxW float64 // max discharge W from B3 low-SOC taper (dynamicMaxDischargeW when unrestricted)
	PWOffsetW       float64 // extra discharge W added to intent from the Powerwall-low offset
}

// DynamicModeConstraint encodes a mode's desired setpoint and its allowed range.
// Negative setpoint = discharge; positive = charge.
// Constraint-only modes (TransferLimit, Safety, CCLOverflow, SOCChargeLimit) leave Target=0.
// Intent modes (Supply, Charge, CarCharging) set Target to their desired value.
// Combine with add(); call Setpoint() to resolve.
//
// Invariant: at most one mode in a chain has a non-zero Target.
// intentConstraint enforces mutual exclusivity before building the chain.
//
// Tie-break when lo > hi: clamp(v, lo, hi) returns lo (the lower bound) when lo > hi.
// In Setpoint(): lo = MinCharge - MaxDischarge, hi = MaxCharge (reduced by MinDischarge).
// When the transfer limit is over capacity it sets MinCharge=500 and MaxDischarge=0,
// making lo=500. If the SOC limit simultaneously caps MaxCharge=0, hi=0 and lo(500) > hi(0) →
// Setpoint returns 500W charge. Transfer-limit forced absorption always wins over MaxCharge caps,
// which is correct: the bus needs to shed that power regardless of B3's SOC.
type DynamicModeConstraint struct {
	Target       float64 // signed desired setpoint; 0 = no preference
	MinCharge    float64 // floor: must charge at least this much (positive; over-limit case)
	MinDischarge float64 // floor: must discharge at least this much (positive magnitude; CCL overflow)
	MaxDischarge float64 // cap: max discharge allowed (positive magnitude)
	MaxCharge    float64 // cap: max charge allowed
}

// add merges two constraints. Ranges intersect (most restrictive). Targets sum.
// Because at most one mode has a non-zero Target, the sum equals that mode's value.
func (a DynamicModeConstraint) add(b DynamicModeConstraint) DynamicModeConstraint {
	return DynamicModeConstraint{
		Target:       a.Target + b.Target,
		MinCharge:    max(a.MinCharge, b.MinCharge),
		MinDischarge: max(a.MinDischarge, b.MinDischarge),
		MaxDischarge: min(a.MaxDischarge, b.MaxDischarge),
		MaxCharge:    min(a.MaxCharge, b.MaxCharge),
	}
}

// Setpoint clamps Target to [MinCharge-MaxDischarge, hi] where hi is MaxCharge reduced
// by MinDischarge when active. See type-level comment for the lo>hi tie-break.
func (c DynamicModeConstraint) Setpoint() float64 {
	lo := c.MinCharge - c.MaxDischarge
	hi := c.MaxCharge
	if c.MinDischarge > 0 {
		hi = min(hi, -c.MinDischarge)
	}
	return clamp(c.Target, lo, hi)
}

func clamp(v, lo, hi float64) float64 { return max(lo, min(hi, v)) }

// transferLimitConstraint returns the range constraint enforcing the 4.5kW transfer limit.
// When over the limit, MaxDischarge=0 and MinCharge>0 (must absorb excess).
// When under the limit, MaxDischarge is capped to available headroom.
//
// busLoadExcludingMP2 = sensor.powerhouse_net_power + MultiplusACPower.
// powerhouse_net_power is the actual cable flow (accounts for powerhouse loads), but
// includes MP2's current output. Adding MultiplusACPower (negative when inverting)
// strips MP2 back out so its own discharge doesn't reduce the headroom available to itself.
func transferLimitConstraint(busLoadExcludingMP2 float64) DynamicModeConstraint {
	headroom := dynamicTransferLimit - busLoadExcludingMP2
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

// cvlOverflowConstraint returns a MinDischarge floor of (fraction * solar34W). The fraction
// ramps from 0 at (CVL - rampStartV) up to 1 at (CVL - targetOffsetV), then continues to
// grow past 1 between (CVL - targetOffsetV) and CVL. The over-correction above the target
// voltage drags voltage back down; equilibrium settles at exactly (CVL - targetOffsetV).
// Returns no-op when CVL is unknown.
func cvlOverflowConstraint(voltage, cvl, solar34W float64) DynamicModeConstraint {
	base := DynamicModeConstraint{
		MaxDischarge: dynamicMaxDischargeW,
		MaxCharge:    dynamicMaxChargeW,
	}
	if cvl <= 0 {
		return base
	}
	rampWidth := cvlOverflowRampStartV - cvlOverflowTargetOffsetV
	fraction := max(0, (voltage-(cvl-cvlOverflowRampStartV))/rampWidth)
	base.MinDischarge = min(fraction*solar34W, dynamicMaxDischargeW)
	return base
}

// cclOverflowConstraint returns a MinDischarge floor to keep battery current
// cclOverflowHeadroomA amps below the BMS CCL. When solar (battery-side DC amps) exceeds
// CCL minus the headroom, the Multiplus must discharge the difference to relieve MPPT throttling.
// Returns zero constraint (no effect) when solar is within the allowed window.
func cclOverflowConstraint(solar3A, solar4A, ccl, voltage float64) DynamicModeConstraint {
	overflowA := (solar3A + solar4A) - (ccl - cclOverflowHeadroomA)
	overflowW := max(0, overflowA) * voltage
	return DynamicModeConstraint{
		MinDischarge: min(overflowW, dynamicMaxDischargeW),
		MaxDischarge: dynamicMaxDischargeW,
		MaxCharge:    dynamicMaxChargeW,
	}
}

// b3SOCChargeLimit tapers MaxCharge linearly from dynamicMaxChargeW at b3ChargeLimitFullSOC
// to 0W at b3ChargeLimitZeroSOC. This suppresses voluntary charging as B3 approaches full
// while leaving transfer-limit forced absorption unaffected (MinCharge beats MaxCharge via
// the lo>hi tie-break — see DynamicModeConstraint comment).
func b3SOCChargeLimit(soc float64) DynamicModeConstraint {
	fraction := clamp((b3ChargeLimitZeroSOC-soc)/(b3ChargeLimitZeroSOC-b3ChargeLimitFullSOC), 0, 1)
	return DynamicModeConstraint{
		MaxDischarge: dynamicMaxDischargeW,
		MaxCharge:    dynamicMaxChargeW * fraction,
	}
}

// b3SOCDischargeLimit caps MaxDischarge linearly from 0W at b3DischargeLimitZeroSOC up to
// dynamicMaxDischargeW at b3DischargeLimitFullSOC, protecting Battery 3 from over-discharge
// at low SOC. Held flat (0 below Zero, unrestricted above Full). No hysteresis.
func b3SOCDischargeLimit(soc float64) DynamicModeConstraint {
	fraction := clamp((soc-b3DischargeLimitZeroSOC)/(b3DischargeLimitFullSOC-b3DischargeLimitZeroSOC), 0, 1)
	return DynamicModeConstraint{
		MaxDischarge: dynamicMaxDischargeW * fraction,
		MaxCharge:    dynamicMaxChargeW,
	}
}

// powerwallLowOffset returns extra discharge watts (positive) to add to the discharge intent
// when the Powerwall is low: pwOffsetMaxW at pwOffsetFullSOC, ramping linearly to 0 at
// pwOffsetZeroSOC, held flat outside that band. No hysteresis.
func powerwallLowOffset(powerwallSOC float64) float64 {
	fraction := clamp((pwOffsetZeroSOC-powerwallSOC)/(pwOffsetZeroSOC-pwOffsetFullSOC), 0, 1)
	return pwOffsetMaxW * fraction
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
	headroom := dynamicTransferLimit - (input.PowerhouseNetPower + input.MultiplusACPower)
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
		priority = priorityCharge
	}

	// Car charging overrides the default intent when eligible
	carStatus := ""
	if input.CarChargingEnabled {
		_, reason := carChargingSetpoint(input)
		carStatus = reason
		if reason == "active" {
			c = DynamicModeConstraint{Target: -dynamicMaxDischargeW, MaxDischarge: dynamicMaxDischargeW, MaxCharge: dynamicMaxChargeW}
			priority = priorityCarCharge
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
	state.cvlVoltageMax.Update(input.Battery3Voltage)

	busLoad := input.PowerhouseNetPower + input.MultiplusACPower
	headroom := dynamicTransferLimit - busLoad

	// Safety: high frequency or grid-off with high Powerwall → no discharge.
	// Charging is still allowed so excess generation is absorbed rather than wasted.
	isSafety := input.ACFreqP100_5Min > 52.75 || (!input.GridAvailable && input.PowerwallSOC > 90.0)

	intent, priority, carStatus := intentConstraint(input, state)

	// Powerwall-low offset: add extra discharge to the intent. Increases existing discharge;
	// when charging/neutral it only reduces charge toward 0 and never forces net discharge.
	pwOffset := powerwallLowOffset(input.PowerwallSOC)
	if intent.Target < 0 {
		intent.Target -= pwOffset
	} else {
		intent.Target = max(0, intent.Target-pwOffset)
	}

	tl := transferLimitConstraint(busLoad)
	sfty := safetyConstraint(isSafety)
	cclOF := cclOverflowConstraint(input.Solar3BatteryCurrent, input.Solar4BatteryCurrent, input.Battery3CCL, input.Battery3Voltage)
	cvlOF := cvlOverflowConstraint(state.cvlVoltageMax.Max(), input.Battery3CVL, input.Solar34Power)
	dischargeLimit := b3SOCDischargeLimit(input.Battery3SOC)
	if isSafety {
		priority = prioritySafety
	}

	// Compose: intent (single non-zero Target) → hard range constraints (Target=0).
	// sfty and tl narrow the allowed range; cclOF/cvlOF enforce a minimum discharge floor;
	// socLimit tapers MaxCharge as B3 fills (transfer-limit MinCharge still wins via lo>hi).
	// phChargeLimit prevents charging from drawing power through the cable from the house side.
	socLimit := b3SOCChargeLimit(input.Battery3SOC)
	phChargeLimit := DynamicModeConstraint{
		MaxDischarge: dynamicMaxDischargeW,
		MaxCharge:    max(0, input.Solar1Power+input.Inverter1to9Power),
	}
	setpoint := intent.add(sfty).add(tl).add(cclOF).add(cvlOF).add(socLimit).add(dischargeLimit).add(phChargeLimit).Setpoint()

	return setpoint, DynamicDebugInfo{
		Priority:        priority,
		Setpoint:        setpoint,
		Headroom:        headroom,
		Battery3SOC:     input.Battery3SOC,
		Safety:          isSafety,
		CarCharging:     carStatus,
		CCLOverflowW:    cclOF.MinDischarge,
		CVLOverflowW:    cvlOF.MinDischarge,
		B3ChargeMaxW:    socLimit.MaxCharge,
		B3DischargeMaxW: dischargeLimit.MaxDischarge,
		PWOffsetW:       pwOffset,
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
		cvlVoltageMax:       governor.NewRollingMinMaxSeconds(10),
	}

	var lastSetpoint float64
	var prevCarChargingActive bool
	var carChargingActiveSeen bool
	var prevCarChargingEnabled bool

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	send := func(setpoint float64) {
		payload, _ := json.Marshal(map[string]float64{haServiceValueKey: setpoint})
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
				if autoSetpoint != lastSetpoint {
					send(autoSetpoint)
				}
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
