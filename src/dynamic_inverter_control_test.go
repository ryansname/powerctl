package main

import (
	"testing"

	"github.com/ryansname/powerctl/src/governor"
	"github.com/stretchr/testify/assert"
)

func makeTestDynamicState() *DynamicInverterState {
	return &DynamicInverterState{
		houseLoadMax:        governor.NewRollingMinMaxSeconds(60),
		houseSideGeneration: governor.NewRollingMinMaxSeconds(60),
	}
}

func makeBaseDynamicInput() DynamicInput {
	return DynamicInput{
		HouseLoad:          1000,
		Solar1Power:        0,
		Solar2Power:        0,
		Inverter1to9Power:  0,
		MultiplusACPower:   0,
		Battery3SOC:        65.0,
		GridAvailable:      true,
		ACFreqP100_5Min:    50.0,
		PowerwallSOC:       50.0,
		DynamicAutoEnabled: true,
		Battery3CCL:     150.0,
		Battery3Voltage: 53.0,
	}
}

// stableThrottle pins the throttle discharge offset so it won't step on the first tick.
func stableThrottle(state *DynamicInverterState, offsetW float64) {
	state.throttleDischargeW = offsetW
	state.throttleTracking = offsetW > 0
	state.throttleRampTicks = 0
}

// --- transferLimitConstraint + DynamicModeConstraint.Setpoint tests ---

func TestTransferLimit_NoGeneration_DischargePassesThrough(t *testing.T) {
	// No generation: full headroom=4500W → MaxDischarge=3000 → -1000 passes through
	tl := transferLimitConstraint(0, 0)
	got := DynamicModeConstraint{Target: -1000, MaxDischarge: dynamicMaxDischargeW, MaxCharge: dynamicMaxChargeW}.add(tl).Setpoint()
	assert.InDelta(t, -1000.0, got, 0.001)
}

func TestTransferLimit_DischargeCapAtHeadroom(t *testing.T) {
	// solar=1kW + i1-9=3kW → headroom=500W → discharge capped at 500W
	tl := transferLimitConstraint(1000, 3000)
	got := DynamicModeConstraint{Target: -2000, MaxDischarge: dynamicMaxDischargeW, MaxCharge: dynamicMaxChargeW}.add(tl).Setpoint()
	assert.InDelta(t, -500.0, got, 0.001)
}

func TestTransferLimit_OverLimit_FloorIsMinCharge(t *testing.T) {
	// solar=2kW + i1-9=3kW → headroom=-500W → MinCharge=500; intent=0 → clamped up to 500
	tl := transferLimitConstraint(2000, 3000)
	got := DynamicModeConstraint{MaxDischarge: dynamicMaxDischargeW, MaxCharge: dynamicMaxChargeW}.add(tl).Setpoint()
	assert.InDelta(t, 500.0, got, 0.001)
}

func TestTransferLimit_OverLimit_LargeExcess_CapsAtMaxCharge(t *testing.T) {
	// i1-9=8kW → headroom=-3.5kW → MinCharge capped at 3500W
	tl := transferLimitConstraint(0, 8000)
	got := DynamicModeConstraint{MaxDischarge: dynamicMaxDischargeW, MaxCharge: dynamicMaxChargeW}.add(tl).Setpoint()
	assert.InDelta(t, 3500.0, got, 0.001)
}

func TestTransferLimit_ChargeIntentPassesThrough(t *testing.T) {
	// Desired 1000W charge, plenty of headroom
	tl := transferLimitConstraint(500, 500)
	got := DynamicModeConstraint{Target: 1000, MaxDischarge: dynamicMaxDischargeW, MaxCharge: dynamicMaxChargeW}.add(tl).Setpoint()
	assert.InDelta(t, 1000.0, got, 0.001)
}

func TestTransferLimit_DischargeCapAt3000(t *testing.T) {
	// No generation, intent -5000W → Multiplus discharge cap at 3000W
	tl := transferLimitConstraint(0, 0)
	got := DynamicModeConstraint{Target: -5000, MaxDischarge: dynamicMaxDischargeW, MaxCharge: dynamicMaxChargeW}.add(tl).Setpoint()
	assert.InDelta(t, -3000.0, got, 0.001)
}

func TestTransferLimit_ChargeCapAt3500(t *testing.T) {
	// Intent way over max charge
	tl := transferLimitConstraint(0, 0)
	got := DynamicModeConstraint{Target: 5000, MaxDischarge: dynamicMaxDischargeW, MaxCharge: dynamicMaxChargeW}.add(tl).Setpoint()
	assert.InDelta(t, 3500.0, got, 0.001)
}

// --- calculateDynamicSetpoint tests ---

func TestCalculateDynamic_Safety_HighFreq_PreventsDischarge(t *testing.T) {
	// High freq: safety clamps discharge to 0; house load would normally request discharge
	state := makeTestDynamicState()
	input := makeBaseDynamicInput()
	input.ACFreqP100_5Min = 53.0
	input.HouseLoad = 1000 // would normally discharge 1000W

	setpoint, debug := calculateDynamicSetpoint(input, state)
	assert.InDelta(t, 0.0, setpoint, 0.001)
	assert.Equal(t, "Safety", debug.Priority)
	assert.True(t, debug.Safety)
}

func TestCalculateDynamic_Safety_GridOffHighPowerwall(t *testing.T) {
	state := makeTestDynamicState()
	input := makeBaseDynamicInput()
	input.GridAvailable = false
	input.PowerwallSOC = 91.0

	setpoint, debug := calculateDynamicSetpoint(input, state)
	assert.InDelta(t, 0.0, setpoint, 0.001)
	assert.Equal(t, "Safety", debug.Priority)
	assert.True(t, debug.Safety)
}

func TestCalculateDynamic_Safety_HighFreq_AllowsForcedCharge(t *testing.T) {
	// High freq + over transfer limit: safety blocks discharge but charge intent is preserved.
	// Surplus=(2000+3000-1000)=4000 → desired=min(4000,3500)=3500; floor=500 → 3500 wins.
	state := makeTestDynamicState()
	input := makeBaseDynamicInput()
	input.ACFreqP100_5Min = 53.0
	input.Solar1Power = 2000
	input.Inverter1to9Power = 3000 // headroom=-500 → MinCharge=500

	setpoint, debug := calculateDynamicSetpoint(input, state)
	assert.InDelta(t, 3500.0, setpoint, 0.001)
	assert.True(t, debug.Safety)
}

func TestCalculateDynamic_DefaultSupply_NoGeneration(t *testing.T) {
	state := makeTestDynamicState()
	input := makeBaseDynamicInput()
	input.HouseLoad = 2000 // 2kW needed, no generation

	setpoint, debug := calculateDynamicSetpoint(input, state)
	assert.Equal(t, "Supply", debug.Priority)
	// target = 2000 - 0 = 2000, desired = -2000, headroom = 4500 → setpoint = -2000
	assert.InDelta(t, -2000.0, setpoint, 0.001)
}

func TestCalculateDynamic_DefaultSupply_CappedByHeadroom(t *testing.T) {
	state := makeTestDynamicState()
	input := makeBaseDynamicInput()
	// target = 5000 - (1000+0+3000) = 1000, desired = -1000, headroom = 4500-1000-3000 = 500 → -500
	input.HouseLoad = 5000
	input.Solar1Power = 1000
	input.Inverter1to9Power = 3000
	input.Solar2Power = 0

	setpoint, debug := calculateDynamicSetpoint(input, state)
	assert.Equal(t, "Supply", debug.Priority)
	assert.InDelta(t, -500.0, setpoint, 0.001)
}

func TestCalculateDynamic_DefaultSupply_CappedAt3000(t *testing.T) {
	state := makeTestDynamicState()
	input := makeBaseDynamicInput()
	input.HouseLoad = 5000 // large load, no generation → discharge cap at 3000W

	setpoint, debug := calculateDynamicSetpoint(input, state)
	assert.Equal(t, "Supply", debug.Priority)
	assert.InDelta(t, -3000.0, setpoint, 0.001)
}

func TestCalculateDynamic_ChargeFromSurplus(t *testing.T) {
	state := makeTestDynamicState()
	input := makeBaseDynamicInput()
	input.HouseLoad = 500
	input.Solar1Power = 1000
	input.Solar2Power = 500
	input.Inverter1to9Power = 1000
	// target = 500 - (1000+500+1000) = -2000 → surplus = 2000W → charge 2000W
	// headroom = 4500-1000-1000 = 2500 → setpoint = 2000

	setpoint, debug := calculateDynamicSetpoint(input, state)
	assert.Equal(t, "Charge", debug.Priority)
	assert.InDelta(t, 2000.0, setpoint, 0.001)
}

func TestCalculateDynamic_ChargeFromSurplus_SmallSurplus(t *testing.T) {
	// Solar barely exceeds house load — should charge only the 100W surplus, not all solar
	state := makeTestDynamicState()
	input := makeBaseDynamicInput()
	input.HouseLoad = 1000
	input.Solar1Power = 1100
	// target = 1000 - 1100 = -100 → surplus = 100W → charge 100W

	setpoint, debug := calculateDynamicSetpoint(input, state)
	assert.Equal(t, "Charge", debug.Priority)
	assert.InDelta(t, 100.0, setpoint, 0.001)
}

func TestCalculateDynamic_ChargeFromSurplus_ForcedChargeByTransferLimit(t *testing.T) {
	// headroom=-1500 → MinCharge=1500 (floor); surplus=6000 → desired=3500.
	// desired(3500) ≥ floor(1500) → 3500 is preserved.
	state := makeTestDynamicState()
	input := makeBaseDynamicInput()
	input.HouseLoad = 0
	input.Solar1Power = 2000
	input.Inverter1to9Power = 4000 // total on bus = 6kW > 4500W → headroom=-1500

	setpoint, debug := calculateDynamicSetpoint(input, state)
	assert.Equal(t, "Charge", debug.Priority)
	assert.InDelta(t, 3500.0, setpoint, 0.001)
}

func TestCalculateDynamic_ChargeFromSurplus_CapAt3500(t *testing.T) {
	state := makeTestDynamicState()
	input := makeBaseDynamicInput()
	input.HouseLoad = 0
	input.Solar1Power = 2000
	input.Inverter1to9Power = 2000 // headroom = 500W
	// P3: desired = min(4000, 3500) = 3500, clamped to 500W headroom → wait no
	// headroom = 4500 - 4000 = 500W, so max discharge is -500W (or no discharge)
	// desired = 3500 (charge), 3500 > 0 so no clamping needed, just cap at 3500

	setpoint, debug := calculateDynamicSetpoint(input, state)
	assert.Equal(t, "Charge", debug.Priority)
	assert.InDelta(t, 3500.0, setpoint, 0.001)
}

// --- regression tests ---

func TestConstraint_OverLimit_DesiredChargeTakesPrecedence(t *testing.T) {
	// transferLimitConstraint(2000, 3000): headroom=-500 → MinCharge=500, MaxDischarge=0
	// An intent of 3500W charge should be preserved — 3500 ≥ floor(500).
	tl := transferLimitConstraint(2000, 3000)
	got := DynamicModeConstraint{Target: 3500,
		MaxDischarge: dynamicMaxDischargeW, MaxCharge: dynamicMaxChargeW}.add(tl).Setpoint()
	assert.InDelta(t, 3500.0, got, 0.001)
}

func TestCalculateDynamic_ChargeFromSurplus_OverLimitPreservesHigherCharge(t *testing.T) {
	// Solar1=2000, Inverter1to9=3000 → headroom=-500 → floor=500W
	// Surplus=5000W → desired=min(5000, 3500)=3500W → should not be replaced by floor.
	state := makeTestDynamicState()
	input := makeBaseDynamicInput()
	input.HouseLoad = 0
	input.Solar1Power = 2000
	input.Inverter1to9Power = 3000

	setpoint, debug := calculateDynamicSetpoint(input, state)
	assert.Equal(t, "Charge", debug.Priority)
	assert.InDelta(t, 3500.0, setpoint, 0.001)
}

func TestCalculateDynamic_Headroom(t *testing.T) {
	state := makeTestDynamicState()
	input := makeBaseDynamicInput()
	input.HouseLoad = 3000
	input.Solar1Power = 500
	input.Inverter1to9Power = 1000

	_, debug := calculateDynamicSetpoint(input, state)
	// headroom = 4500 - 500 - 1000 = 3000
	assert.InDelta(t, 3000.0, debug.Headroom, 0.001)
	assert.InDelta(t, input.Battery3SOC, debug.Battery3SOC, 0.001)
}

// --- isThrottling unit tests ---

func TestIsThrottling_MpptMode2_NotThrottling(t *testing.T) {
	// Both MPPTs in MPPT mode (mode 2) — freely tracking max power, not BMS-throttled.
	input := makeBaseDynamicInput()
	input.Solar3MpptMode = 2
	input.Solar4MpptMode = 2
	input.Solar3BatteryCurrent = 40
	input.Solar4BatteryCurrent = 40
	assert.False(t, isThrottling(input))
}

func TestIsThrottling_MpptAtHardwareCeiling_NotThrottling(t *testing.T) {
	// Both MPPTs in mode 1 at 45A (and within the 2A buffer) — hardware ceiling, not BMS throttling.
	input := makeBaseDynamicInput()
	input.Solar3MpptMode = 1
	input.Solar4MpptMode = 1
	input.Solar3BatteryCurrent = 45
	input.Solar4BatteryCurrent = 44 // within 2A buffer
	assert.False(t, isThrottling(input))
}

func TestIsThrottling_MpptMode1BelowHardwareCeiling_Throttling(t *testing.T) {
	// Both MPPTs in mode 1 at below 45A — BMS is holding them back.
	input := makeBaseDynamicInput()
	input.Solar3MpptMode = 1
	input.Solar4MpptMode = 1
	input.Solar3BatteryCurrent = 38
	input.Solar4BatteryCurrent = 40
	assert.True(t, isThrottling(input))
}

func TestIsThrottling_OneMpptThrottled_ReturnsTrue(t *testing.T) {
	// Only one MPPT throttled (e.g. Multiplus charging pushes one over CCL limit).
	input := makeBaseDynamicInput()
	input.Solar3MpptMode = 1
	input.Solar3BatteryCurrent = 38 // BMS-throttled
	input.Solar4MpptMode = 2
	input.Solar4BatteryCurrent = 45 // free MPPT
	assert.True(t, isThrottling(input))
}

func TestIsThrottling_AllOff_NotThrottling(t *testing.T) {
	// Night — both MPPTs off (mode 0).
	input := makeBaseDynamicInput()
	input.Solar3MpptMode = 0
	input.Solar4MpptMode = 0
	input.Solar3BatteryCurrent = 0
	input.Solar4BatteryCurrent = 0
	assert.False(t, isThrottling(input))
}

// --- throttle state machine tests ---

// tickN calls calculateDynamicSetpoint n times with the given input and returns the last debug.
func tickN(n int, input DynamicInput, state *DynamicInverterState) DynamicDebugInfo {
	var debug DynamicDebugInfo
	for range n {
		_, debug = calculateDynamicSetpoint(input, state)
	}
	return debug
}

// throttlingInput returns a DynamicInput with both MPPTs in mode 1 below the hardware ceiling.
func throttlingInput() DynamicInput {
	input := makeBaseDynamicInput()
	input.Solar3MpptMode = 1
	input.Solar4MpptMode = 1
	input.Solar3BatteryCurrent = 38
	input.Solar4BatteryCurrent = 40
	return input
}

func TestThrottle_MpptAtHardwareCeiling_NoBatteryThrottle(t *testing.T) {
	// Bug fix: MPPTs at their own 45A ceiling (mode 1 at 45A) must NOT trigger the ramp.
	state := makeTestDynamicState()
	input := makeBaseDynamicInput()
	input.Solar3MpptMode = 1
	input.Solar4MpptMode = 1
	input.Solar3BatteryCurrent = 45
	input.Solar4BatteryCurrent = 45

	debug := tickN(20, input, state)
	assert.InDelta(t, 0.0, debug.ThrottleDischargeW, 0.001)
	assert.False(t, debug.ThrottleActive)
}

func TestThrottle_RealThrottle_MultiplusInverting(t *testing.T) {
	// MPPTs in mode 1 below ceiling while Multiplus inverting — offset ramps up.
	state := makeTestDynamicState()
	input := throttlingInput()
	input.MultiplusACPower = -3000

	debug1 := tickN(throttleRampIntervalS, input, state)
	assert.InDelta(t, throttleRampStepW, debug1.ThrottleDischargeW, 0.001)
	assert.True(t, debug1.ThrottleActive)

	debug2 := tickN(throttleRampIntervalS, input, state)
	assert.InDelta(t, 2*throttleRampStepW, debug2.ThrottleDischargeW, 0.001)
}

func TestThrottle_RealThrottle_MultiplusCharging(t *testing.T) {
	// MPPTs in mode 1 below ceiling while Multiplus charging from AC — ramp still fires.
	state := makeTestDynamicState()
	input := throttlingInput()
	input.MultiplusACPower = 2000

	debug := tickN(throttleRampIntervalS, input, state)
	assert.InDelta(t, throttleRampStepW, debug.ThrottleDischargeW, 0.001)
}

func TestThrottle_RampClampAtMaxDischarge(t *testing.T) {
	state := makeTestDynamicState()
	state.throttleDischargeW = dynamicMaxDischargeW - throttleRampStepW + 1
	state.throttleRampTicks = throttleRampIntervalS - 1
	input := throttlingInput()

	_, debug := calculateDynamicSetpoint(input, state)
	assert.InDelta(t, dynamicMaxDischargeW, debug.ThrottleDischargeW, 0.001)
}

func TestThrottle_TrackingPhase_SlewsToTarget(t *testing.T) {
	// After ramping, throttle clears. Offset should slew DOWN to the CCL−5A target.
	// excessA = 75+75 - (150-5) = 5A → targetW = 5*53 = 265W
	// Steps to converge from 500W: 500→450→400→350→300→265 (5 steps × 10 ticks = 50 ticks)
	state := makeTestDynamicState()
	state.throttleDischargeW = 500
	state.throttleRampTicks = 0
	input := makeBaseDynamicInput()
	input.Battery3ChargeCurrent = 130 // clear of CCL+hysteresis
	input.Solar3BatteryCurrent = 75
	input.Solar4BatteryCurrent = 75
	input.Battery3Voltage = 53

	// After one step interval: max(265, 500-50) = 450
	debug := tickN(throttleRampIntervalS, input, state)
	assert.InDelta(t, 450.0, debug.ThrottleDischargeW, 0.001)
	assert.True(t, state.throttleTracking)

	// After 4 more steps (40 more ticks): 450→400→350→300→265
	debug = tickN(4*throttleRampIntervalS, input, state)
	assert.InDelta(t, 265.0, debug.ThrottleDischargeW, 0.001)
}

func TestThrottle_TrackingPhase_SolarDrops_DecaysToZero(t *testing.T) {
	// Solar drops significantly → excessA < 0 → targetW = 0 → offset decays to 0
	state := makeTestDynamicState()
	state.throttleDischargeW = 200
	state.throttleRampTicks = 0
	input := makeBaseDynamicInput()
	input.Battery3ChargeCurrent = 80 // well clear of CCL
	input.Solar3BatteryCurrent = 30
	input.Solar4BatteryCurrent = 30 // excessA = 60 - 145 = -85 → targetW = 0

	// After one step: max(0, 200-50) = 150
	debug := tickN(throttleRampIntervalS, input, state)
	assert.InDelta(t, 150.0, debug.ThrottleDischargeW, 0.001)

	// After four total steps: 200→150→100→50→0
	debug = tickN(3*throttleRampIntervalS, input, state)
	assert.InDelta(t, 0.0, debug.ThrottleDischargeW, 0.001)
	assert.False(t, state.throttleTracking)
}

func TestThrottle_CarCharging_SuppressesThrottle(t *testing.T) {
	// When car charging is the priority, throttle offset must stay zero even if throttling.
	state := makeTestDynamicState()
	input := throttlingInput()
	input.CarChargingEnabled = true
	input.Battery3SOC = 90
	input.CarBattery3Cutoff = 0 // no SOC cutoff
	input.Solar1Power = 2000    // solarProducing=true so car charging gate passes

	debug := tickN(20, input, state)
	assert.InDelta(t, 0.0, debug.ThrottleDischargeW, 0.001)
}

func TestThrottle_Safety_AntiWindup_NoRamp(t *testing.T) {
	// Safety (high freq) must suppress the throttle ramp even when throttling.
	state := makeTestDynamicState()
	input := throttlingInput()
	input.ACFreqP100_5Min = 53.0

	debug := tickN(20, input, state)
	assert.InDelta(t, 0.0, debug.ThrottleDischargeW, 0.001)
}

func TestThrottle_ZeroHeadroom_AntiWindup_NoRamp(t *testing.T) {
	// Zero headroom must suppress the throttle ramp.
	state := makeTestDynamicState()
	input := throttlingInput()
	input.Solar1Power = 2000
	input.Inverter1to9Power = 2500 // headroom = 0

	debug := tickN(20, input, state)
	assert.InDelta(t, 0.0, debug.ThrottleDischargeW, 0.001)
}

func TestThrottle_OffsetApplied_ReducesChargeIntent(t *testing.T) {
	// Surplus 500W → charge intent +500W. After 200W offset: intent = 500-200 = 300W.
	state := makeTestDynamicState()
	stableThrottle(state, 200)
	input := makeBaseDynamicInput()
	input.HouseLoad = 1000
	input.Solar1Power = 1500 // target = 1000-1500 = -500 → charge 500W

	_, debug := calculateDynamicSetpoint(input, state)
	assert.InDelta(t, 300.0, debug.Setpoint, 0.001)
}

func TestThrottle_OffsetApplied_IncreasesDischargeIntent(t *testing.T) {
	// Supply 2000W discharge intent. After 500W offset: intent = -2000-500 = -2500W.
	state := makeTestDynamicState()
	stableThrottle(state, 500)
	input := makeBaseDynamicInput()
	input.HouseLoad = 2000
	input.Solar1Power = 0

	_, debug := calculateDynamicSetpoint(input, state)
	assert.InDelta(t, -2500.0, debug.Setpoint, 0.001)
}

func TestThrottle_OffsetApplied_Safety_ClampsToZero(t *testing.T) {
	// Even with throttle offset, safety (MaxDischarge=0) prevents discharge.
	state := makeTestDynamicState()
	stableThrottle(state, 1000)
	input := makeBaseDynamicInput()
	input.ACFreqP100_5Min = 53.0

	_, debug := calculateDynamicSetpoint(input, state)
	assert.InDelta(t, 0.0, debug.Setpoint, 0.001)
	assert.True(t, debug.Safety)
}

func TestThrottle_TransferLimitStillAppliesWithOffset(t *testing.T) {
	// stableThrottle 2000W; target=Supply(-1000); after offset=-3000; headroom=500 → setpoint=-500
	state := makeTestDynamicState()
	stableThrottle(state, 2000)
	input := makeBaseDynamicInput()
	input.HouseLoad = 3000
	input.Solar1Power = 2000
	input.Inverter1to9Power = 2000 // headroom = 500W

	_, debug := calculateDynamicSetpoint(input, state)
	assert.InDelta(t, -500.0, debug.Setpoint, 0.001)
}
