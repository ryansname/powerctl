package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
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

	// powerhouseTransferLimitW is the powerhouse cable's rating, shared by both
	// controllers so they cannot drift apart — the baseline one sizes its coarse
	// 255W inverter steps against it, the dynamic one trims the remainder.
	powerhouseTransferLimitW = 4500.0

	// dynamicExportLimit caps total sell-back at the grid meter, the whole-house
	// analogue of the powerhouse cable's transfer limit above. Unlike that one it
	// is not a conductor rating — it bounds what the whole site pushes back to the
	// grid, so the Multiplus stops discharging (and is eventually forced to absorb)
	// once the house as a whole is exporting this much.
	//
	// Starting deliberately above the nominal 5kW: this constraint is new, so the
	// first pass is set to bind only on genuine over-export rather than to trim
	// normal operation. Lower it toward 5000 once its behaviour is confirmed.
	dynamicExportLimit = 5500.0

	// carChargingMinHeadroom requires at least this much transfer-limit headroom to engage.
	carChargingMinHeadroom = 1500.0

	// b3ChargeTargetSOC is the SOC the forecast-aware charge limit aims to reach by the end of
	// the solar day. Above it, voluntary charging stops (only transfer-limit absorption remains).
	b3ChargeTargetSOC = 98.0

	// b3SolarEndThresholdKw is the (pre-multiplier Solcast) generation above which a forecast
	// period still counts as "solar producing" when locating the end of the solar day.
	b3SolarEndThresholdKw = 0.05

	// constraintProximity is how close (as a fraction of its limit) the setpoint must be to a
	// constraint for that constraint to be surfaced in the debug table.
	constraintProximity = 0.25

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

	// cvlVoltageEMATau is the smoothing time constant for the Battery 3 voltage feeding
	// the CVL overflow ramp. An EMA (not a rolling max) is used: the max rode the top of
	// the measurement noise, biasing the ramp high and latching transient spikes. ~8s
	// roughly halves discharge jitter while staying ~2-3s responsive.
	cvlVoltageEMATau = 8 * time.Second

	// cvlIRCompOhms strips the Multiplus's own IR contribution out of the measured terminal
	// voltage before it feeds the CVL ramp: V_adj = V_term - I_mp*R, with I_mp ≈ Multiplus
	// AC power / voltage (charging positive; inverter efficiency folds into R). Without this
	// the ramp reacts to a voltage the Multiplus itself moves — charging lifts V_term over the
	// ramp knee, the floor forces discharge, V_term collapses, the floor releases, and it
	// limit-cycles. V_adj = V_oc + I_solar*R is independent of the setpoint, breaking the loop.
	// ~1.5mΩ from observed ΔV/ΔI (~0.04V per ~27A swing).
	cvlIRCompOhms = 0.0015

	// cvlChargeTaperBandV is the width of the voltage band ending at the discharge ramp start
	// (CVL - cvlOverflowRampStartV) over which MaxCharge is tapered from full to zero. It hands
	// voluntary charging → 0 → discharge over continuously as V_adj rises, so the setpoint no
	// longer steps ~1.5kW when the discharge floor first engages.
	cvlChargeTaperBandV = 0.10

	// dynamicTickSeconds is the setpoint keepalive interval: the last setpoint is
	// republished to the Cerbo this often even when nothing changed. It is not the
	// compute cadence — the setpoint is recalculated on every inbound reading.
	// Declared in seconds and scaled up to a Duration, so a fractional tick stays
	// exact — deriving it the other way round would be integer division on a
	// Duration and would truncate.
	dynamicTickSeconds = 5.0
	dynamicTick        = dynamicTickSeconds * time.Second

	// --- voltage discharge limit ---------------------------------------------
	//
	// Exporting raises grid voltage, and the Multiplus is a strong actuator: a
	// step test measured ~3.3 V/kW, so its full 3kW swings the bus ~10V — enough
	// to trip the rest of the fleet. The limit is a discharge ceiling that only
	// ever moves relative to what the Multiplus is actually doing: it throttles
	// down from the current output while voltage is high, and only relaxes while
	// the output is pressed against the ceiling and voltage is low. It never
	// sits more than voltageAtLimitMarginW above the current output, because how
	// the bus behaves at a power level not yet run is unknown — a ceiling left
	// up at 3kW lets a step demand (CCL/CVL overflow, car charging) jump
	// straight there.
	//
	// Thresholds are on the raw Multiplus reading, which sits on a ~1V lattice
	// (…248.6, 249.6, 250.6…). Exact voltage does not matter, only whether to
	// throttle, and the raw reading presents within a couple of seconds.

	// voltageThrottleV: above this the ceiling sheds from the current output.
	voltageThrottleV = 252.0

	// voltageRelaxV: below this the ceiling may rise, if the output is at it.
	// Between this and voltageThrottleV the ceiling holds.
	voltageRelaxV = 251.0

	// Ceiling slew rates. Relaxing overshoot ≈ rate × lag × 3.3 V/kW: 50 W/s over
	// ~4s of reading lag is ~0.7V, so a relax that starts below 251 tops out
	// before the throttle threshold.
	voltageShedRateWps  = 50.0
	voltageRelaxRateWps = 50.0

	// voltageAtLimitMarginW is how close the output must be to the ceiling to
	// count as pressed against it, and how far above the output the ceiling may
	// sit. Covers the Multiplus's setpoint tracking error.
	voltageAtLimitMarginW = 150.0

	// voltageCapMaxDtSeconds bounds one ceiling step. The controller runs off
	// inbound readings, not a ticker, and the bridge feeding it drops updates when
	// its channel backs up — so a gap can be arbitrarily long. Without a bound, one
	// step after a stall would move the ceiling further than the slew rate allows.
	voltageCapMaxDtSeconds = 5.0

	// mp2PowerLagWindowSeconds covers the reporting lag of powerhouse_net_power and
	// site power relative to the Multiplus's own ~2s power reading. Both contain the
	// Multiplus's output, which the limits strip back out using its power reading.
	// Pairing a fresh Multiplus reading with a stale total double-counts every watt
	// the Multiplus has just added as headroom — positive feedback that let it run
	// straight through the cable limit. So the strip uses the least-discharging
	// Multiplus power over this window: credit is only taken for output the slow
	// meters have had time to see.
	mp2PowerLagWindowSeconds = 60

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
	cvlVoltageEMA       governor.EMA           // EMA-smoothed IR-compensated Battery 3 voltage (V_adj); CVL ramp input
	mp2PowerMax         governor.RollingMinMax // Multiplus AC power over mp2PowerLagWindowSeconds; Max = least discharging

	// voltageCapW is the current discharge ceiling in W. Persists across ticks —
	// it is the controller's only memory of how much curtailment the bus needs.
	voltageCapW float64
	// voltageCapInit guards the zero value: a fresh state starts at full
	// discharge, and the actual-output bound pulls it down on the first step.
	voltageCapInit bool
	// voltageCapAt is when the ceiling last stepped, so the next step can use
	// the real elapsed time.
	voltageCapAt time.Time
}

// DynamicDebugInfo contains mode states for the dynamic controller debug output.
type DynamicDebugInfo struct {
	Auto               bool
	Priority           string
	Setpoint           float64
	Headroom           float64
	HeadroomActive     bool // true when the transfer-limit headroom is near/binding the setpoint
	Battery3SOC        float64
	Safety             bool
	CarCharging        string  // "" = disabled, "active", or gate reason (e.g. "gated: soc")
	CCLOverflowW       float64 // watts the CCL-overflow constraint requires as minimum discharge
	CCLChargeMaxW      float64 // max charge W the CCL headroom allows (dynamicMaxChargeW when unrestricted)
	CVLOverflowW       float64 // watts the CVL-overflow constraint requires as minimum discharge
	B3ChargeMaxW       float64 // max charge W from forecast charge limit (dynamicMaxChargeW when unrestricted)
	B3ExpectedFinalKwh float64 // projected EOD B3 energy from current SOC + forecast battery-side solar (no powerhouse charging)
	B3DischargeMaxW    float64 // max discharge W from B3 low-SOC taper (dynamicMaxDischargeW when unrestricted)
	PWOffsetW          float64 // extra discharge W added to intent from the Powerwall-low offset
	ExportCapW         float64 // max discharge W the whole-house export limit allows (dynamicMaxDischargeW when unrestricted)
	// BusVoltage is the Multiplus reading driving VoltageCapW, the current
	// discharge ceiling. Unlike the other fields these are reported
	// unconditionally: the ceiling is stateful, so hiding it while it happens not
	// to bind would make it impossible to tell settling from hunting.
	BusVoltage  float64 // V, raw Multiplus AC-in reading (0 = no reading)
	VoltageCapW float64 // W, current discharge ceiling from the voltage limit
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
// The caller passes the windowed (settled) MP2 power, not the instantaneous one — see
// mp2PowerLagWindowSeconds.
func transferLimitConstraint(busLoadExcludingMP2 float64) DynamicModeConstraint {
	headroom := powerhouseTransferLimitW - busLoadExcludingMP2
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

// exportLimitConstraint returns the range constraint enforcing the whole-house
// sell-back limit. Structurally identical to transferLimitConstraint: when over
// the limit, MaxDischarge=0 and MinCharge>0 (must absorb the excess); when under,
// MaxDischarge is capped to the remaining export headroom.
//
// exportExcludingMP2 = MultiplusACPower - SitePower, i.e. what the house would be
// exporting (positive) if the Multiplus were idle.
//
// The sign juggling matters. SitePower is +ve importing / -ve exporting, so raw
// export is -SitePower. SitePower already contains the Multiplus's own output
// (MultiplusACPower, -ve when inverting), which must be stripped back out or the
// inverter eats its own headroom: every watt it exports would appear as less
// headroom, and it would throttle itself to a standstill well short of the limit.
// Removing its contribution gives -(SitePower - MultiplusACPower), the expression
// above.
//
// Deliberately NOT clamped at zero. When the house is importing, exportExcludingMP2
// goes negative and the headroom correctly exceeds dynamicExportLimit — the
// Multiplus may discharge that much before any of it reaches the grid. This mirrors
// transferLimitConstraint, which likewise takes its bus load signed.
func exportLimitConstraint(exportExcludingMP2 float64) DynamicModeConstraint {
	headroom := dynamicExportLimit - exportExcludingMP2
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

// voltageCapDt returns the seconds elapsed since the ceiling last stepped and
// records now as the new step time. The controller recalculates on every inbound
// reading (~1s, and skippable when the input bridge drops one) rather than on
// dynamicTick, so the interval has to be measured rather than assumed. The first
// step has no predecessor to measure against and so advances by nothing; the
// clamp also absorbs a backwards clock.
func voltageCapDt(state *DynamicInverterState, now time.Time) float64 {
	prev := state.voltageCapAt
	state.voltageCapAt = now
	if prev.IsZero() {
		return 0
	}
	return clamp(now.Sub(prev).Seconds(), 0, voltageCapMaxDtSeconds)
}

// updateVoltageCap steps the discharge ceiling by dt seconds against the
// Multiplus voltage reading v and its actual discharge, and returns the
// resulting constraint.
//
//   - v > throttle threshold: shed from the current output (not from a
//     ceiling that may be well above it).
//   - v < relax threshold and the output is pressed against the ceiling: rise
//     at the relax rate.
//   - otherwise: hold.
//
// In every case the ceiling is first bounded to the actual output plus the
// at-limit margin, so it can never be wound up above what has actually been run.
// No reading (v=0) holds; relaxing on absent data would quietly disable the
// limit. Charging is never restricted: it pulls voltage down.
func updateVoltageCap(
	state *DynamicInverterState,
	v float64,
	actualDischargeW float64,
	dt float64,
) DynamicModeConstraint {
	if !state.voltageCapInit {
		state.voltageCapW = dynamicMaxDischargeW
		state.voltageCapInit = true
	}

	atLimit := actualDischargeW >= state.voltageCapW-voltageAtLimitMarginW
	state.voltageCapW = min(state.voltageCapW, actualDischargeW+voltageAtLimitMarginW)

	if v > 0 {
		switch {
		case v > voltageThrottleV:
			state.voltageCapW = min(state.voltageCapW, actualDischargeW) - voltageShedRateWps*dt
		case v < voltageRelaxV && atLimit:
			state.voltageCapW += voltageRelaxRateWps * dt
		}
	}
	state.voltageCapW = clamp(state.voltageCapW, 0, dynamicMaxDischargeW)

	return DynamicModeConstraint{
		MaxDischarge: state.voltageCapW,
		MaxCharge:    dynamicMaxChargeW,
	}
}

// stepVoltageCap advances the voltage ceiling for this input. Distinct from the
// export limit — that one bounds how much power leaves the site, this one bounds
// the voltage rise exporting causes.
func stepVoltageCap(input DynamicInput, state *DynamicInverterState) DynamicModeConstraint {
	return updateVoltageCap(
		state,
		input.MultiplusACInVoltage,
		max(0, -input.MultiplusACPower),
		voltageCapDt(state, time.Now()),
	)
}

// safetyConstraint returns a range constraint that blocks discharge when active
// (high AC frequency or grid-off with high Powerwall). Charging remains allowed.
func safetyConstraint(active bool) DynamicModeConstraint {
	if active {
		return DynamicModeConstraint{MaxDischarge: 0, MaxCharge: dynamicMaxChargeW}
	}
	return DynamicModeConstraint{MaxDischarge: dynamicMaxDischargeW, MaxCharge: dynamicMaxChargeW}
}

// cvlCompensatedVoltage strips the Multiplus's own IR contribution out of the measured terminal
// voltage so the CVL ramp reacts to battery state rather than to its own setpoint:
//
//	V_adj = V_term - I_mp*R,  I_mp ≈ multiplusACPower / V_term  (charging positive)
//
// V_adj = V_oc + I_solar*R is independent of the setpoint, which is what breaks the CVL
// charge/discharge limit cycle. See cvlIRCompOhms. No-op at 0V.
func cvlCompensatedVoltage(vTerm, multiplusACPower float64) float64 {
	if vTerm <= 0 {
		return vTerm
	}
	impA := multiplusACPower / vTerm
	return vTerm - impA*cvlIRCompOhms
}

// cvlOverflowConstraint shapes charging and discharging around the CVL as a single continuous
// curve in the (IR-compensated, smoothed) voltage:
//   - Below the discharge ramp start (CVL - rampStartV), MaxCharge tapers from full to 0 over
//     cvlChargeTaperBandV, so voluntary charging winds down before the discharge floor engages.
//   - From (CVL - rampStartV) the MinDischarge floor is (fraction * solar34W): fraction ramps
//     0→1 up to (CVL - targetOffsetV), then past 1 toward CVL. The over-correction drags voltage
//     back down; equilibrium settles at exactly (CVL - targetOffsetV).
//
// The taper→floor handoff is continuous (charge → 0 → discharge, no step). Returns no-op when
// CVL is unknown.
func cvlOverflowConstraint(voltage, cvl, solar34W float64) DynamicModeConstraint {
	base := DynamicModeConstraint{
		MaxDischarge: dynamicMaxDischargeW,
		MaxCharge:    dynamicMaxChargeW,
	}
	if cvl <= 0 {
		return base
	}
	rampStart := cvl - cvlOverflowRampStartV

	// Taper MaxCharge to zero over the band ending at rampStart for a smooth charge→discharge
	// handoff. Full charge below the band; zero at/above rampStart (the discharge regime).
	chargeFrac := clamp((rampStart-voltage)/cvlChargeTaperBandV, 0, 1)
	base.MaxCharge = chargeFrac * dynamicMaxChargeW

	rampWidth := cvlOverflowRampStartV - cvlOverflowTargetOffsetV
	fraction := max(0, (voltage-rampStart)/rampWidth)
	base.MinDischarge = min(fraction*solar34W, dynamicMaxDischargeW)
	return base
}

// cclOverflowConstraint keeps total battery current cclOverflowHeadroomA amps below the BMS CCL,
// in both directions. headroomA is the amps of charge current still available beyond battery-side
// solar before reaching CCL - headroom; converted to watts (× voltage) the same way for charge and
// discharge:
//   - headroomA > 0: cap MaxCharge at headroomA × voltage so solar + Multiplus charge stays under
//     the limit (the Multiplus may still charge the remaining headroom).
//   - headroomA < 0: solar alone already exceeds the limit, so force MinDischarge of the excess
//     (−headroomA × voltage) to relieve MPPT throttling.
//
// Returns no constraint when voltage is unavailable (0V at startup).
func cclOverflowConstraint(solar3A, solar4A, ccl, voltage float64) DynamicModeConstraint {
	if voltage <= 0 {
		return DynamicModeConstraint{MaxDischarge: dynamicMaxDischargeW, MaxCharge: dynamicMaxChargeW}
	}
	headroomA := (ccl - cclOverflowHeadroomA) - (solar3A + solar4A)
	headroomW := headroomA * voltage
	return DynamicModeConstraint{
		MinDischarge: min(max(0, -headroomW), dynamicMaxDischargeW),
		MaxDischarge: dynamicMaxDischargeW,
		MaxCharge:    min(max(0, headroomW), dynamicMaxChargeW),
	}
}

// b3ForecastChargeLimit caps voluntary Multiplus charging so Battery 3 reaches b3ChargeTargetSOC
// by the end of the forecast solar day, accounting for the battery-side solar (Solar 3 & 4) still
// expected to arrive. The remaining-solar forecast is used as published: it already declines
// monotonically as the day is consumed, so carrying a running minimum over it added no pessimism
// and only risked latching an overnight zero for the whole day. The energy gap to target minus
// that expected solar is the deficit the powerhouse side must supply; spreading it over the
// hours until solar end gives the rate cap.
// MaxCharge is 0 once battery-side solar alone covers the gap (or B3 is already at target), and
// full once solar has ended (the surplus-only charge intent gates actual charging anyway).
// Transfer-limit forced absorption is unaffected — MinCharge beats MaxCharge via the lo>hi
// tie-break (see DynamicModeConstraint comment).
func b3ForecastChargeLimit(
	soc float64,
	capacityWh float64,
	forecastRemainingWh float64,
	solarMultiplier float64,
	forecast governor.ForecastPeriods,
	now time.Time,
) DynamicModeConstraint {
	c := DynamicModeConstraint{
		MaxDischarge: dynamicMaxDischargeW,
		MaxCharge:    dynamicMaxChargeW,
	}

	gapWh := max(0, (b3ChargeTargetSOC-soc)/100*capacityWh)
	if gapWh == 0 {
		c.MaxCharge = 0 // already at/above target: no voluntary charge
		return c
	}

	deficitWh := gapWh - solarMultiplier*forecastRemainingWh
	if deficitWh <= 0 {
		c.MaxCharge = 0 // battery-side solar alone reaches target
		return c
	}

	hours := forecast.FindSolarEndTime(b3SolarEndThresholdKw).Sub(now).Hours()
	if hours <= 0 {
		return c // solar ended (or no forecast): allow full charge to close the gap
	}

	c.MaxCharge = clamp(deficitWh/hours, 0, dynamicMaxChargeW)
	return c
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
// busLoad is the powerhouse bus load excluding the Multiplus (see calculateDynamicSetpoint).
func carChargingSetpoint(input DynamicInput, busLoad float64) (float64, string) {
	if input.CarBattery3Cutoff > 0 && input.Battery3SOC < input.CarBattery3Cutoff {
		return 0, "gated: b3 soc"
	}
	solarProducing := (input.Solar1Power + input.Solar2Power) > 200
	if !solarProducing && (input.CarBattery3Cutoff <= 0 || input.Battery3SOC < input.CarBattery3Cutoff) {
		return 0, "gated: no production"
	}
	headroom := powerhouseTransferLimitW - busLoad
	if headroom < carChargingMinHeadroom {
		return 0, "gated: headroom"
	}
	return -dynamicMaxDischargeW, "active"
}

// intentConstraint determines the winning control intent and returns it as a DynamicModeConstraint.
// Car charging overrides supply/charge when eligible. Peak tariff suppresses charge intent.
// Returns the constraint, the priority label, and the car-charging status string.
func intentConstraint(
	input DynamicInput,
	state *DynamicInverterState,
	busLoad float64,
) (DynamicModeConstraint, string, string) {
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
		_, reason := carChargingSetpoint(input, busLoad)
		carStatus = reason
		if reason == "active" {
			c = DynamicModeConstraint{Target: -dynamicMaxDischargeW, MaxDischarge: dynamicMaxDischargeW, MaxCharge: dynamicMaxChargeW}
			priority = priorityCarCharge
		}
	}
	return c, priority, carStatus
}

// calculateDynamicSetpoint computes the desired Multiplus setpoint from a DynamicInput.
// volt is the voltage ceiling from stepVoltageCap, taken as an argument so the other
// constraints can be exercised without it. Returns the clamped setpoint and debug
// info. Updates state as a side effect.
func calculateDynamicSetpoint(
	input DynamicInput,
	state *DynamicInverterState,
	volt DynamicModeConstraint,
) (float64, DynamicDebugInfo) {
	state.houseLoadMax.Update(input.HouseLoad)
	state.houseSideGeneration.Update(input.Solar1Power + input.Inverter1to9Power)
	state.cvlVoltageEMA.Update(cvlCompensatedVoltage(input.Battery3Voltage, input.MultiplusACPower))

	// Strip the Multiplus back out of the slow cable and site meters using its
	// least-discharging power over their lag window, not its instantaneous value.
	// See mp2PowerLagWindowSeconds.
	state.mp2PowerMax.Update(input.MultiplusACPower)
	mp2Settled := state.mp2PowerMax.Max()
	busLoad := input.PowerhouseNetPower + mp2Settled
	headroom := powerhouseTransferLimitW - busLoad

	// Safety: high frequency or grid-off with high Powerwall → no discharge.
	// Charging is still allowed so excess generation is absorbed rather than wasted.
	isSafety := input.ACFreqP100_5Min > 52.75 || (!input.GridAvailable && input.PowerwallSOC > 90.0)

	intent, priority, carStatus := intentConstraint(input, state, busLoad)

	// Powerwall-low offset: add extra discharge to the intent. Increases existing discharge;
	// when charging/neutral it only reduces charge toward 0 and never forces net discharge.
	pwOffset := powerwallLowOffset(input.PowerwallSOC)
	if intent.Target < 0 {
		intent.Target -= pwOffset
	} else {
		intent.Target = max(0, intent.Target-pwOffset)
	}

	tl := transferLimitConstraint(busLoad)
	// Whole-house sell-back cap, independent of the powerhouse cable limit above:
	// the cable can be well within rating while the site as a whole over-exports.
	exp := exportLimitConstraint(mp2Settled - input.SitePower)
	sfty := safetyConstraint(isSafety)
	cclOF := cclOverflowConstraint(input.Solar3BatteryCurrent, input.Solar4BatteryCurrent, input.Battery3CCL, input.Battery3Voltage)
	cvlOF := cvlOverflowConstraint(state.cvlVoltageEMA.Value(), input.Battery3CVL, input.Solar34Power)
	dischargeLimit := b3SOCDischargeLimit(input.Battery3SOC)
	if isSafety {
		priority = prioritySafety
	}

	// Compose: intent (single non-zero Target) → hard range constraints (Target=0).
	// sfty and tl narrow the allowed range; cclOF/cvlOF enforce a minimum discharge floor;
	// socLimit caps MaxCharge to whatever powerhouse charging B3 needs to reach target by solar
	// end given the forecast (transfer-limit MinCharge still wins via lo>hi).
	// phChargeLimit prevents charging from drawing power through the cable from the house side.
	now := time.Now()
	socLimit := b3ForecastChargeLimit(
		input.Battery3SOC,
		input.Battery3CapacityWh,
		input.ForecastRemainingWh,
		input.SolarMultiplier,
		input.DetailedForecast,
		now,
	)
	phChargeLimit := DynamicModeConstraint{
		MaxDischarge: dynamicMaxDischargeW,
		MaxCharge:    max(0, input.Solar1Power+input.Inverter1to9Power),
	}
	setpoint := intent.add(sfty).add(tl).add(exp).add(volt).add(cclOF).add(cvlOF).add(socLimit).add(dischargeLimit).add(phChargeLimit).Setpoint()

	// Surface a constraint in the debug output only when the setpoint is within
	// constraintProximity of its limit — i.e. it is binding or close to binding. Constraints
	// further away are reported at their inactive sentinel so the table hides them.
	nearCap := func(capW float64) bool { // MaxCharge ceiling (positive)
		return capW < dynamicMaxChargeW && math.Abs(setpoint-capW) <= constraintProximity*capW
	}
	nearDischargeCap := func(maxDisW float64) bool { // MaxDischarge floor (negative side)
		return maxDisW < dynamicMaxDischargeW && math.Abs(setpoint+maxDisW) <= constraintProximity*maxDisW
	}
	nearFloor := func(minDisW float64) bool { // MinDischarge forced discharge
		return minDisW > 0 && math.Abs(setpoint+minDisW) <= constraintProximity*minDisW
	}

	cclOverflowW := 0.0
	if nearFloor(cclOF.MinDischarge) {
		cclOverflowW = cclOF.MinDischarge
	}
	cvlOverflowW := 0.0
	if nearFloor(cvlOF.MinDischarge) {
		cvlOverflowW = cvlOF.MinDischarge
	}
	cclChargeMaxW := dynamicMaxChargeW
	if nearCap(cclOF.MaxCharge) {
		cclChargeMaxW = cclOF.MaxCharge
	}
	b3ChargeMaxW := dynamicMaxChargeW
	if nearCap(socLimit.MaxCharge) {
		b3ChargeMaxW = socLimit.MaxCharge
	}
	b3DischargeMaxW := dynamicMaxDischargeW
	if nearDischargeCap(dischargeLimit.MaxDischarge) {
		b3DischargeMaxW = dischargeLimit.MaxDischarge
	}
	// Export limit: same near-or-binding treatment as the other caps. Its forced
	// absorption (MinCharge, when the site is already over-exporting) surfaces via
	// the same rule the transfer limit uses.
	exportCapW := dynamicMaxDischargeW
	if nearDischargeCap(exp.MaxDischarge) || exp.MinCharge > 0 {
		exportCapW = exp.MaxDischarge
	}
	// Transfer-limit headroom counts as a constraint too: show it when discharge is near the
	// headroom cap, or when over the limit (forcing charge).
	headroomActive := nearDischargeCap(tl.MaxDischarge) || tl.MinCharge > 0

	return setpoint, DynamicDebugInfo{
		Priority:        priority,
		Setpoint:        setpoint,
		Headroom:        headroom,
		HeadroomActive:  headroomActive,
		Battery3SOC:     input.Battery3SOC,
		Safety:          isSafety,
		CarCharging:     carStatus,
		CCLOverflowW:    cclOverflowW,
		CCLChargeMaxW:   cclChargeMaxW,
		CVLOverflowW:    cvlOverflowW,
		B3ChargeMaxW:    b3ChargeMaxW,
		B3DischargeMaxW: b3DischargeMaxW,
		PWOffsetW:       pwOffset,
		ExportCapW:      exportCapW,
		BusVoltage:      input.MultiplusACInVoltage,
		VoltageCapW:     volt.MaxDischarge,
		// Projected EOD energy if only battery-side solar charges B3 (no powerhouse charging):
		// current stored energy + forecast solar, in kWh.
		B3ExpectedFinalKwh: (input.Battery3SOC/100*input.Battery3CapacityWh + input.SolarMultiplier*input.ForecastRemainingWh) / 1000,
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
		cvlVoltageEMA:       governor.NewEMA(cvlVoltageEMATau),
		mp2PowerMax:         governor.NewRollingMinMaxSeconds(mp2PowerLagWindowSeconds),
	}

	var lastSetpoint float64
	var prevCarChargingActive bool
	var carChargingActiveSeen bool
	var prevCarChargingEnabled bool

	ticker := time.NewTicker(dynamicTick)
	defer ticker.Stop()

	send := func(setpoint float64) {
		payload, _ := json.Marshal(map[string]float64{haServiceValueKey: setpoint})
		sender.Send(MQTTMessage{Topic: TopicMultiplusSetpointWrite, Payload: payload, QoS: 0})
	}

	disableCarCharging := func(reason string) {
		log.Printf("Car charging auto-disable: %s\n", reason)
		// retain=false: durability of control state lives in the TOML state file +
		// re-announce, not in a retained MQTT message (see state_store.go).
		sender.Send(MQTTMessage{
			Topic:   TopicCarChargingEnabledState,
			Payload: []byte("OFF"),
			QoS:     1,
			Retain:  false,
		})
	}

	for {
		select {
		case input := <-inputChan:
			autoSetpoint, debug := calculateDynamicSetpoint(input, state, stepVoltageCap(input, state))
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
