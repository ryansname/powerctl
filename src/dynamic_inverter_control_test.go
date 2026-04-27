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
	}
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

// --- MPPT boost tests ---

// stableBoost pins the boost offset at the given value for the duration of a test
// call by setting the deadband so the ramp switch takes the "hold" branch.
func stableBoost(state *DynamicInverterState, offset float64) {
	state.mpptBoostOffset = offset
	state.mpptDeadbandTicks = mpptBoostDeadbandTicks
}

func TestMpptBoost_NoThrottling_OffsetStaysZero(t *testing.T) {
	state := makeTestDynamicState()
	input := makeBaseDynamicInput()
	input.MpptThrottling = false

	_, debug := calculateDynamicSetpoint(input, state)
	assert.InDelta(t, 0.0, debug.MpptBoost, 0.001)
	assert.False(t, debug.MpptThrottling)
}

func TestMpptBoost_Throttling_RampsUpEachTick(t *testing.T) {
	state := makeTestDynamicState()
	input := makeBaseDynamicInput()
	input.MpptThrottling = true

	_, debug1 := calculateDynamicSetpoint(input, state)
	assert.InDelta(t, mpptBoostRampW, debug1.MpptBoost, 0.001)

	_, debug2 := calculateDynamicSetpoint(input, state)
	assert.InDelta(t, 2*mpptBoostRampW, debug2.MpptBoost, 0.001)
}

func TestMpptBoost_OffsetClampsAtMaxDischarge(t *testing.T) {
	state := makeTestDynamicState()
	state.mpptBoostOffset = dynamicMaxDischargeW - mpptBoostRampW + 0.1
	input := makeBaseDynamicInput()
	input.MpptThrottling = true

	_, debug := calculateDynamicSetpoint(input, state)
	assert.InDelta(t, dynamicMaxDischargeW, debug.MpptBoost, 0.001)
}

func TestMpptBoost_DeadbandHoldsOffsetAfterThrottleClears(t *testing.T) {
	state := makeTestDynamicState()
	state.mpptBoostOffset = 500
	state.mpptDeadbandTicks = 3
	input := makeBaseDynamicInput()
	input.MpptThrottling = false

	_, debug := calculateDynamicSetpoint(input, state)
	assert.InDelta(t, 500.0, debug.MpptBoost, 0.001) // held, deadband decremented to 2
	assert.Equal(t, 2, state.mpptDeadbandTicks)
}

func TestMpptBoost_RampsDownAfterDeadbandExpires(t *testing.T) {
	state := makeTestDynamicState()
	state.mpptBoostOffset = 300
	state.mpptDeadbandTicks = 0
	input := makeBaseDynamicInput()
	input.MpptThrottling = false

	_, debug := calculateDynamicSetpoint(input, state)
	assert.InDelta(t, 300-mpptBoostRampW, debug.MpptBoost, 0.001)
}

func TestMpptBoost_ChargeIntent_PushedToDischarge(t *testing.T) {
	// target = houseLoadMax(500) - solar(1000) = -500 → charge intent = min(500,3500) = 500
	// After boost 500: intent.Target = 500 - 500 = 0 → setpoint = 0
	state := makeTestDynamicState()
	stableBoost(state, 500)
	input := makeBaseDynamicInput()
	input.HouseLoad = 500
	input.Solar1Power = 1000
	input.MpptThrottling = false

	_, debug := calculateDynamicSetpoint(input, state)
	assert.InDelta(t, 0.0, debug.Setpoint, 0.001)
}

func TestMpptBoost_Safety_ClampsToZero(t *testing.T) {
	// Even with MPPT boost, safety (MaxDischarge=0) prevents discharge.
	state := makeTestDynamicState()
	stableBoost(state, 1000)
	input := makeBaseDynamicInput()
	input.MpptThrottling = false
	input.ACFreqP100_5Min = 53.0

	_, debug := calculateDynamicSetpoint(input, state)
	assert.InDelta(t, 0.0, debug.Setpoint, 0.001)
	assert.True(t, debug.Safety)
}

func TestMpptBoost_Safety_AntiWindup_NoRampDuringSafety(t *testing.T) {
	// Boost must not ramp up during a safety event (anti-windup).
	state := makeTestDynamicState()
	input := makeBaseDynamicInput()
	input.MpptThrottling = true
	input.ACFreqP100_5Min = 53.0

	_, debug := calculateDynamicSetpoint(input, state)
	assert.InDelta(t, 0.0, debug.MpptBoost, 0.001)
}

func TestMpptBoost_ZeroHeadroom_AntiWindup_NoRamp(t *testing.T) {
	// Boost must not ramp up when headroom is 0.
	state := makeTestDynamicState()
	input := makeBaseDynamicInput()
	input.MpptThrottling = true
	input.Solar1Power = 2000
	input.Inverter1to9Power = 2500 // headroom = 0

	_, debug := calculateDynamicSetpoint(input, state)
	assert.InDelta(t, 0.0, debug.MpptBoost, 0.001)
}

func TestMpptBoost_TransferLimitHeadroom_CapsDischarge(t *testing.T) {
	// target = houseLoadMax(0) - (2000+0+2000) = -4000 → charge intent = min(4000,3500) = 3500
	// After boost 2000: intent.Target = 3500 - 2000 = 1500 → charge 1500W
	state := makeTestDynamicState()
	stableBoost(state, 2000)
	input := makeBaseDynamicInput()
	input.MpptThrottling = false
	input.HouseLoad = 0
	input.Solar1Power = 2000
	input.Inverter1to9Power = 2000 // headroom = 500W

	_, debug := calculateDynamicSetpoint(input, state)
	assert.InDelta(t, 1500.0, debug.Setpoint, 0.001)
}

func TestMpptBoost_TransferLimitHeadroom_CapsDischargeBelowBoost(t *testing.T) {
	// target = houseLoadMax(3000) - (2000+0+2000) = -1000 → Supply -1000
	// After boost 2000: intent.Target = -3000; headroom=500 → setpoint=-500
	state := makeTestDynamicState()
	stableBoost(state, 2000)
	input := makeBaseDynamicInput()
	input.MpptThrottling = false
	input.HouseLoad = 3000
	input.Solar1Power = 2000
	input.Inverter1to9Power = 2000 // headroom = 500W

	_, debug := calculateDynamicSetpoint(input, state)
	assert.InDelta(t, -500.0, debug.Setpoint, 0.001)
}
