package main

import (
	"testing"
	"time"

	"github.com/ryansname/powerctl/src/governor"
	"github.com/stretchr/testify/assert"
)

func makeTestDynamicState() *DynamicInverterState {
	return &DynamicInverterState{
		houseLoadMax:        governor.NewRollingMinMaxSeconds(60),
		houseSideGeneration: governor.NewRollingMinMaxSeconds(60),
		cvlVoltageEMA:       governor.NewEMA(cvlVoltageEMATau),
		mp2PowerMax:         governor.NewRollingMinMaxSeconds(mp2PowerLagWindowSeconds),
	}
}

// noVoltLimit isolates the other constraints from the voltage ceiling, which
// starts bounded to the (idle) Multiplus output and so would cap every
// discharge test at the at-limit margin.
var noVoltLimit = safetyConstraint(false)

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
		Battery3CCL:        150.0,
		Battery3Voltage:    53.0,
		Battery3CapacityWh: 43500.0,
		SolarMultiplier:    3.9,
		// No DetailedForecast by default → solar "ended" → charge limit allows full rate,
		// so charge-path tests behave as before. Forecast-specific tests set it explicitly.
	}
}

// --- transferLimitConstraint + DynamicModeConstraint.Setpoint tests ---

func TestTransferLimit_NoGeneration_DischargePassesThrough(t *testing.T) {
	// No generation: full headroom=4500W → MaxDischarge=3000 → -1000 passes through
	tl := transferLimitConstraint(0)
	got := DynamicModeConstraint{Target: -1000, MaxDischarge: dynamicMaxDischargeW, MaxCharge: dynamicMaxChargeW}.add(tl).Setpoint()
	assert.InDelta(t, -1000.0, got, 0.001)
}

func TestTransferLimit_DischargeCapAtHeadroom(t *testing.T) {
	// solar=1kW + i1-9=3kW → headroom=500W → discharge capped at 500W
	tl := transferLimitConstraint(4000)
	got := DynamicModeConstraint{Target: -2000, MaxDischarge: dynamicMaxDischargeW, MaxCharge: dynamicMaxChargeW}.add(tl).Setpoint()
	assert.InDelta(t, -500.0, got, 0.001)
}

func TestTransferLimit_OverLimit_FloorIsMinCharge(t *testing.T) {
	// solar=2kW + i1-9=3kW → headroom=-500W → MinCharge=500; intent=0 → clamped up to 500
	tl := transferLimitConstraint(5000)
	got := DynamicModeConstraint{MaxDischarge: dynamicMaxDischargeW, MaxCharge: dynamicMaxChargeW}.add(tl).Setpoint()
	assert.InDelta(t, 500.0, got, 0.001)
}

func TestTransferLimit_OverLimit_LargeExcess_CapsAtMaxCharge(t *testing.T) {
	// i1-9=8kW → headroom=-3.5kW → MinCharge capped at 3500W
	tl := transferLimitConstraint(8000)
	got := DynamicModeConstraint{MaxDischarge: dynamicMaxDischargeW, MaxCharge: dynamicMaxChargeW}.add(tl).Setpoint()
	assert.InDelta(t, 3500.0, got, 0.001)
}

func TestTransferLimit_ChargeIntentPassesThrough(t *testing.T) {
	// Desired 1000W charge, plenty of headroom
	tl := transferLimitConstraint(1000)
	got := DynamicModeConstraint{Target: 1000, MaxDischarge: dynamicMaxDischargeW, MaxCharge: dynamicMaxChargeW}.add(tl).Setpoint()
	assert.InDelta(t, 1000.0, got, 0.001)
}

func TestTransferLimit_DischargeCapAt3000(t *testing.T) {
	// No generation, intent -5000W → Multiplus discharge cap at 3000W
	tl := transferLimitConstraint(0)
	got := DynamicModeConstraint{Target: -5000, MaxDischarge: dynamicMaxDischargeW, MaxCharge: dynamicMaxChargeW}.add(tl).Setpoint()
	assert.InDelta(t, -3000.0, got, 0.001)
}

func TestTransferLimit_ChargeCapAt3500(t *testing.T) {
	// Intent way over max charge
	tl := transferLimitConstraint(0)
	got := DynamicModeConstraint{Target: 5000, MaxDischarge: dynamicMaxDischargeW, MaxCharge: dynamicMaxChargeW}.add(tl).Setpoint()
	assert.InDelta(t, 3500.0, got, 0.001)
}

// --- cclOverflowConstraint tests ---

func TestCCLOverflow_NoExcess_NoConstraint(t *testing.T) {
	// Solar=30A, CCL=400A → no overflow → charge intent passes through unchanged
	c := cclOverflowConstraint(15, 15, 400, 53)
	assert.InDelta(t, 0.0, c.MinDischarge, 0.001)
	got := DynamicModeConstraint{Target: 2000, MaxDischarge: dynamicMaxDischargeW, MaxCharge: dynamicMaxChargeW}.add(c).Setpoint()
	assert.InDelta(t, 2000.0, got, 0.001)
}

func TestCCLOverflow_JustOnExcess_SwitchesChargeToDischarge(t *testing.T) {
	// Solar=80A, CCL=80A → overflowA=5A → overflowW=5*53=265W
	// Charge intent +500W must be overridden to -265W discharge
	c := cclOverflowConstraint(40, 40, 80, 53)
	assert.InDelta(t, 265.0, c.MinDischarge, 0.001)
	got := DynamicModeConstraint{Target: 500, MaxDischarge: dynamicMaxDischargeW, MaxCharge: dynamicMaxChargeW}.add(c).Setpoint()
	assert.InDelta(t, -265.0, got, 0.001)
}

func TestCCLOverflow_HigherDemandPassesThrough(t *testing.T) {
	// Solar=80A, CCL=80A → MinDischarge=265W (-265W floor)
	// House supply intent -1000W is more discharge than required → passes through
	c := cclOverflowConstraint(40, 40, 80, 53)
	got := DynamicModeConstraint{Target: -1000, MaxDischarge: dynamicMaxDischargeW, MaxCharge: dynamicMaxChargeW}.add(c).Setpoint()
	assert.InDelta(t, -1000.0, got, 0.001)
}

func TestCCLOverflow_Safety_NoForcedDischarge(t *testing.T) {
	// Safety blocks discharge (MaxDischarge=0). CCL overflow wants discharge.
	// Transfer limit wins: lo=0, hi=-265 → lo>hi → clamp returns lo=0 (charge, not discharge).
	c := cclOverflowConstraint(40, 40, 80, 53) // MinDischarge=265W
	sfty := safetyConstraint(true)             // MaxDischarge=0
	got := DynamicModeConstraint{MaxDischarge: dynamicMaxDischargeW, MaxCharge: dynamicMaxChargeW}.add(sfty).add(c).Setpoint()
	assert.InDelta(t, 0.0, got, 0.001)
}

func TestCCLOverflow_TransferLimitOver_ChargeWins(t *testing.T) {
	// Transfer limit exceeded (MinCharge=500, MaxDischarge=0) AND CCL overflow (MinDischarge=265).
	// lo=500, hi=-265 → lo>hi → clamp returns lo=500 (charge wins, bus safety takes priority).
	tl := transferLimitConstraint(5000)        // headroom=-500 → MinCharge=500
	c := cclOverflowConstraint(40, 40, 80, 53) // MinDischarge=265W
	got := DynamicModeConstraint{MaxDischarge: dynamicMaxDischargeW, MaxCharge: dynamicMaxChargeW}.add(tl).add(c).Setpoint()
	assert.InDelta(t, 500.0, got, 0.001)
}

func TestCCLOverflow_LargeOverflow_CapsAtMaxDischarge(t *testing.T) {
	// Solar=200A, CCL=10A → overflowA=195A → overflowW=10335W → capped at 3000W
	c := cclOverflowConstraint(100, 100, 10, 53)
	assert.InDelta(t, dynamicMaxDischargeW, c.MinDischarge, 0.001)
	got := DynamicModeConstraint{Target: 0, MaxDischarge: dynamicMaxDischargeW, MaxCharge: dynamicMaxChargeW}.add(c).Setpoint()
	assert.InDelta(t, -3000.0, got, 0.001)
}

func TestCCLOverflow_ZeroVoltage_NoConstraint(t *testing.T) {
	// Voltage not yet available (0V at startup) → overflowW=0 → no constraint
	c := cclOverflowConstraint(40, 40, 80, 0)
	assert.InDelta(t, 0.0, c.MinDischarge, 0.001)
}

func TestCCLOverflow_ExactlyAtTarget_NoChargeNoDischarge(t *testing.T) {
	// Solar = CCL - headroom exactly → headroomA=0: no forced discharge AND no charge room.
	// CCL=80A, headroom=5A → target=75A; solar3=37.5A+solar4=37.5A=75A
	c := cclOverflowConstraint(37.5, 37.5, 80, 53)
	assert.InDelta(t, 0.0, c.MinDischarge, 0.001)
	assert.InDelta(t, 0.0, c.MaxCharge, 0.001)
}

func TestCCLOverflow_ChargeHeadroom_CapsCharge(t *testing.T) {
	// Solar=40A, CCL=80A, headroom=5A → 35A of charge room → 35*53=1855W cap.
	// A 3000W charge intent is capped to 1855W; no forced discharge.
	c := cclOverflowConstraint(20, 20, 80, 53)
	assert.InDelta(t, 0.0, c.MinDischarge, 0.001)
	assert.InDelta(t, 1855.0, c.MaxCharge, 0.001)
	got := DynamicModeConstraint{Target: 3000, MaxDischarge: dynamicMaxDischargeW, MaxCharge: dynamicMaxChargeW}.add(c).Setpoint()
	assert.InDelta(t, 1855.0, got, 0.001)
}

func TestCCLOverflow_AmpleHeadroom_NoChargeCap(t *testing.T) {
	// Solar=30A, CCL=400A → huge charge room → MaxCharge clamps to dynamicMaxChargeW (no effect).
	c := cclOverflowConstraint(15, 15, 400, 53)
	assert.InDelta(t, dynamicMaxChargeW, c.MaxCharge, 0.001)
}

// --- cvlOverflowConstraint tests ---

// cvlOverCorrectFraction is the fraction value at V = CVL (above the targetOffset point),
// = rampStart / (rampStart - targetOffset). Used to verify over-correction tests.
func cvlOverCorrectFraction() float64 {
	return cvlOverflowRampStartV / (cvlOverflowRampStartV - cvlOverflowTargetOffsetV)
}

func TestCVLOverflow_UnknownCVL_NoConstraint(t *testing.T) {
	c := cvlOverflowConstraint(55.5, 0, 500)
	assert.InDelta(t, 0.0, c.MinDischarge, 0.001)
}

func TestCVLOverflow_WellBelowWindow_NoConstraint(t *testing.T) {
	c := cvlOverflowConstraint(53.0, 55.2, 500)
	assert.InDelta(t, 0.0, c.MinDischarge, 0.001)
}

func TestCVLOverflow_AtRampStart_NoConstraint(t *testing.T) {
	c := cvlOverflowConstraint(55.2-cvlOverflowRampStartV, 55.2, 500)
	assert.InDelta(t, 0.0, c.MinDischarge, 0.001)
}

func TestCVLOverflow_AtCVL_NoSolar_NoFloor(t *testing.T) {
	// Solar throttled to 0 + voltage at CVL → fraction>0 but floor = fraction*0 = 0.
	// Loop is "stuck" but harmless: no curtailment to relieve when MPPTs aren't producing.
	c := cvlOverflowConstraint(55.2, 55.2, 0)
	assert.InDelta(t, 0.0, c.MinDischarge, 0.001)
}

func TestCVLOverflow_AtTargetVoltage_FractionOne(t *testing.T) {
	// V = CVL - targetOffset → fraction = 1.0 → floor matches solar exactly.
	c := cvlOverflowConstraint(55.2-cvlOverflowTargetOffsetV, 55.2, 500)
	assert.InDelta(t, 500.0, c.MinDischarge, 0.001)
}

func TestCVLOverflow_AtCVL_OverCorrects(t *testing.T) {
	// V = CVL → fraction = rampStart/(rampStart-targetOffset) ≈ 1.111 → floor > solar.
	c := cvlOverflowConstraint(55.2, 55.2, 500)
	assert.InDelta(t, cvlOverCorrectFraction()*500, c.MinDischarge, 0.001)
}

func TestCVLOverflow_Midpoint_HalfOfSolar(t *testing.T) {
	// Midpoint of ramp: V = CVL - (rampStart+targetOffset)/2 → fraction = 0.5.
	midOffset := (cvlOverflowRampStartV + cvlOverflowTargetOffsetV) / 2
	c := cvlOverflowConstraint(55.2-midOffset, 55.2, 500)
	assert.InDelta(t, 250.0, c.MinDischarge, 0.001)
}

func TestCVLOverflow_OverCVL_CappedAtMaxDischarge(t *testing.T) {
	// V above CVL with high solar → fraction>>1, would compute >3000W → capped.
	c := cvlOverflowConstraint(55.5, 55.2, 4000)
	assert.InDelta(t, dynamicMaxDischargeW, c.MinDischarge, 0.001)
}

func TestCVLOverflow_OverridesChargeIntent(t *testing.T) {
	// At target voltage with solar=1000W → floor=1000W overrides +500W charge intent.
	c := cvlOverflowConstraint(55.2-cvlOverflowTargetOffsetV, 55.2, 1000)
	got := DynamicModeConstraint{Target: 500, MaxDischarge: dynamicMaxDischargeW, MaxCharge: dynamicMaxChargeW}.add(c).Setpoint()
	assert.InDelta(t, -1000.0, got, 0.001)
}

func TestCVLOverflow_Safety_NoForcedDischarge(t *testing.T) {
	// Safety (MaxDischarge=0) wins over CVL MinDischarge via lo>hi tie-break.
	c := cvlOverflowConstraint(55.2, 55.2, 1000)
	sfty := safetyConstraint(true)
	got := DynamicModeConstraint{MaxDischarge: dynamicMaxDischargeW, MaxCharge: dynamicMaxChargeW}.add(sfty).add(c).Setpoint()
	assert.InDelta(t, 0.0, got, 0.001)
}

// --- cvlCompensatedVoltage tests ---

func TestCVLCompensatedVoltage_Charging_LowersVAdj(t *testing.T) {
	// Charging (+1500W) lifts terminal voltage via IR; V_adj strips it back out.
	got := cvlCompensatedVoltage(55.0, 1500)
	assert.InDelta(t, 55.0-(1500.0/55.0)*cvlIRCompOhms, got, 1e-9)
	assert.Less(t, got, 55.0)
}

func TestCVLCompensatedVoltage_Discharging_RaisesVAdj(t *testing.T) {
	// Discharging (-1500W) sags terminal voltage; V_adj corrects it upward (symmetry).
	got := cvlCompensatedVoltage(55.0, -1500)
	assert.InDelta(t, 55.0+(1500.0/55.0)*cvlIRCompOhms, got, 1e-9)
	assert.Greater(t, got, 55.0)
}

func TestCVLCompensatedVoltage_ZeroVolt_NoOp(t *testing.T) {
	assert.InDelta(t, 0.0, cvlCompensatedVoltage(0, 1500), 1e-9)
}

// --- cvlOverflowConstraint MaxCharge taper tests ---

func TestCVLOverflow_ChargeTaper_FullBelowBand(t *testing.T) {
	// Below the taper band (rampStart - band): charging unrestricted.
	c := cvlOverflowConstraint(55.2-cvlOverflowRampStartV-cvlChargeTaperBandV-0.01, 55.2, 500)
	assert.InDelta(t, dynamicMaxChargeW, c.MaxCharge, 0.001)
}

func TestCVLOverflow_ChargeTaper_HalfMidBand(t *testing.T) {
	// Halfway through the taper band: MaxCharge at half.
	v := 55.2 - cvlOverflowRampStartV - cvlChargeTaperBandV/2
	c := cvlOverflowConstraint(v, 55.2, 500)
	assert.InDelta(t, 0.5*dynamicMaxChargeW, c.MaxCharge, 0.001)
	assert.InDelta(t, 0.0, c.MinDischarge, 0.001) // no discharge floor yet
}

func TestCVLOverflow_ChargeTaper_ZeroAtRampStart(t *testing.T) {
	// At the discharge ramp start, voluntary charging is fully tapered off.
	c := cvlOverflowConstraint(55.2-cvlOverflowRampStartV, 55.2, 500)
	assert.InDelta(t, 0.0, c.MaxCharge, 0.001)
	assert.InDelta(t, 0.0, c.MinDischarge, 0.001)
}

// --- calculateDynamicSetpoint tests ---

func TestCalculateDynamic_Safety_HighFreq_PreventsDischarge(t *testing.T) {
	// High freq: safety clamps discharge to 0; house load would normally request discharge
	state := makeTestDynamicState()
	input := makeBaseDynamicInput()
	input.ACFreqP100_5Min = 53.0
	input.HouseLoad = 1000 // would normally discharge 1000W

	setpoint, debug := calculateDynamicSetpoint(input, state, noVoltLimit)
	assert.InDelta(t, 0.0, setpoint, 0.001)
	assert.Equal(t, prioritySafety, debug.Priority)
	assert.True(t, debug.Safety)
}

func TestCalculateDynamic_Safety_GridOffHighPowerwall(t *testing.T) {
	state := makeTestDynamicState()
	input := makeBaseDynamicInput()
	input.GridAvailable = false
	input.PowerwallSOC = 91.0

	setpoint, debug := calculateDynamicSetpoint(input, state, noVoltLimit)
	assert.InDelta(t, 0.0, setpoint, 0.001)
	assert.Equal(t, prioritySafety, debug.Priority)
	assert.True(t, debug.Safety)
}

func TestCalculateDynamic_Safety_HighFreq_AllowsForcedCharge(t *testing.T) {
	// High freq + over transfer limit: safety blocks discharge but charge intent is preserved.
	// Surplus=(2000+3000-1000)=4000 → desired=min(4000,3500)=3500; floor=500 → 3500 wins.
	// SOC=50% (below charge limit) so the SOC limit doesn't interfere.
	state := makeTestDynamicState()
	input := makeBaseDynamicInput()
	input.Battery3SOC = 50
	input.ACFreqP100_5Min = 53.0
	input.Solar1Power = 2000
	input.Inverter1to9Power = 3000
	input.PowerhouseNetPower = 5000

	setpoint, debug := calculateDynamicSetpoint(input, state, noVoltLimit)
	assert.InDelta(t, 3500.0, setpoint, 0.001)
	assert.True(t, debug.Safety)
}

func TestCalculateDynamic_DefaultSupply_NoGeneration(t *testing.T) {
	state := makeTestDynamicState()
	input := makeBaseDynamicInput()
	input.HouseLoad = 2000 // 2kW needed, no generation

	setpoint, debug := calculateDynamicSetpoint(input, state, noVoltLimit)
	assert.Equal(t, "Supply", debug.Priority)
	// target = 2000 - 0 = 2000, desired = -2000, headroom = 4500 → setpoint = -2000
	assert.InDelta(t, -2000.0, setpoint, 0.001)
}

func TestCalculateDynamic_DefaultSupply_CappedByHeadroom(t *testing.T) {
	state := makeTestDynamicState()
	input := makeBaseDynamicInput()
	// target = 5000 - (1000+0+3000) = 1000, desired = -1000, headroom = 4500-4000 = 500 → -500
	input.HouseLoad = 5000
	input.Solar1Power = 1000
	input.Inverter1to9Power = 3000
	input.Solar2Power = 0
	input.PowerhouseNetPower = 4000

	setpoint, debug := calculateDynamicSetpoint(input, state, noVoltLimit)
	assert.Equal(t, "Supply", debug.Priority)
	assert.InDelta(t, -500.0, setpoint, 0.001)
}

func TestCalculateDynamic_DefaultSupply_CappedAt3000(t *testing.T) {
	state := makeTestDynamicState()
	input := makeBaseDynamicInput()
	input.HouseLoad = 5000 // large load, no generation → discharge cap at 3000W

	setpoint, debug := calculateDynamicSetpoint(input, state, noVoltLimit)
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

	setpoint, debug := calculateDynamicSetpoint(input, state, noVoltLimit)
	assert.Equal(t, priorityCharge, debug.Priority)
	assert.InDelta(t, 2000.0, setpoint, 0.001)
}

func TestCalculateDynamic_ChargeFromSurplus_SmallSurplus(t *testing.T) {
	// Solar barely exceeds house load — should charge only the 100W surplus, not all solar
	state := makeTestDynamicState()
	input := makeBaseDynamicInput()
	input.HouseLoad = 1000
	input.Solar1Power = 1100
	// target = 1000 - 1100 = -100 → surplus = 100W → charge 100W

	setpoint, debug := calculateDynamicSetpoint(input, state, noVoltLimit)
	assert.Equal(t, priorityCharge, debug.Priority)
	assert.InDelta(t, 100.0, setpoint, 0.001)
}

func TestCalculateDynamic_ChargeFromSurplus_ForcedChargeByTransferLimit(t *testing.T) {
	// headroom=-1500 → MinCharge=1500 (floor); surplus=6000 → desired=3500.
	// desired(3500) ≥ floor(1500) → 3500 is preserved.
	// SOC=50% (below charge limit) so the SOC limit doesn't interfere.
	state := makeTestDynamicState()
	input := makeBaseDynamicInput()
	input.Battery3SOC = 50
	input.HouseLoad = 0
	input.Solar1Power = 2000
	input.Inverter1to9Power = 4000
	input.PowerhouseNetPower = 6000

	setpoint, debug := calculateDynamicSetpoint(input, state, noVoltLimit)
	assert.Equal(t, priorityCharge, debug.Priority)
	assert.InDelta(t, 3500.0, setpoint, 0.001)
}

func TestCalculateDynamic_ChargeFromSurplus_CapAt3500(t *testing.T) {
	// SOC=50% (below charge limit) so the 3500W cap from dynamicMaxChargeW is the active limit.
	state := makeTestDynamicState()
	input := makeBaseDynamicInput()
	input.Battery3SOC = 50
	input.HouseLoad = 0
	input.Solar1Power = 2000
	input.Inverter1to9Power = 2000 // headroom = 500W
	// P3: desired = min(4000, 3500) = 3500, clamped to 500W headroom → wait no
	// headroom = 4500-4000 = 500W, so max discharge is -500W (or no discharge)
	// desired = 3500 (charge), 3500 > 0 so no clamping needed, just cap at 3500

	setpoint, debug := calculateDynamicSetpoint(input, state, noVoltLimit)
	assert.Equal(t, priorityCharge, debug.Priority)
	assert.InDelta(t, 3500.0, setpoint, 0.001)
}

// --- CCL overflow integration in calculateDynamicSetpoint ---

func TestCalculateDynamic_CCLOverflow_SwitchesChargeToDischarge(t *testing.T) {
	// Battery-side solar at CCL → must discharge to create headroom.
	// Solar3=40A, Solar4=40A=80A total; CCL=80A → overflowA=5A → 265W discharge floor.
	// House surplus intent would be +500W charge → overridden to -265W.
	state := makeTestDynamicState()
	input := makeBaseDynamicInput()
	input.HouseLoad = 500
	input.Solar1Power = 1000 // surplus → Charge intent
	input.Solar3BatteryCurrent = 40
	input.Solar4BatteryCurrent = 40
	input.Battery3CCL = 80
	input.Battery3Voltage = 53

	setpoint, debug := calculateDynamicSetpoint(input, state, noVoltLimit)
	assert.InDelta(t, -265.0, setpoint, 0.001)
	assert.InDelta(t, 265.0, debug.CCLOverflowW, 0.001)
}

func TestCalculateDynamic_CCLOverflow_Safety_NoForcedDischarge(t *testing.T) {
	// Safety active + CCL overflow: safety (MaxDischarge=0) wins → setpoint=0.
	state := makeTestDynamicState()
	input := makeBaseDynamicInput()
	input.ACFreqP100_5Min = 53.0
	input.Solar3BatteryCurrent = 40
	input.Solar4BatteryCurrent = 40
	input.Battery3CCL = 80
	input.Battery3Voltage = 53

	setpoint, _ := calculateDynamicSetpoint(input, state, noVoltLimit)
	assert.InDelta(t, 0.0, setpoint, 0.001)
}

func TestCalculateDynamic_CCLOverflow_HigherDemandWins(t *testing.T) {
	// House needs 2000W, CCL overflow floor is only 265W → demand (-2000W) wins.
	state := makeTestDynamicState()
	input := makeBaseDynamicInput()
	input.HouseLoad = 2000
	input.Solar3BatteryCurrent = 40
	input.Solar4BatteryCurrent = 40
	input.Battery3CCL = 80
	input.Battery3Voltage = 53

	setpoint, _ := calculateDynamicSetpoint(input, state, noVoltLimit)
	assert.InDelta(t, -2000.0, setpoint, 0.001)
}

func TestCalculateDynamic_CCLChargeCap_LimitsCharge(t *testing.T) {
	// Solar=40A (battery-side), CCL=80A, headroom=5A → 35A charge room → 1855W cap.
	// Powerhouse surplus would charge at 3000W → capped to 1855W by the CCL charge headroom.
	state := makeTestDynamicState()
	input := makeBaseDynamicInput()
	input.HouseLoad = 0
	input.Solar1Power = 3000 // surplus → Charge intent, also house-side charge headroom
	input.Solar3BatteryCurrent = 20
	input.Solar4BatteryCurrent = 20
	input.Battery3CCL = 80
	input.Battery3Voltage = 53

	setpoint, debug := calculateDynamicSetpoint(input, state, noVoltLimit)
	assert.Equal(t, priorityCharge, debug.Priority)
	assert.InDelta(t, 1855.0, setpoint, 0.001)
	assert.InDelta(t, 1855.0, debug.CCLChargeMaxW, 0.001)
}

func TestCalculateDynamic_CCLChargeCap_HiddenWhenNotBinding(t *testing.T) {
	// CCL charge headroom caps MaxCharge at 1855W, but the house is discharging (supply intent),
	// so the cap isn't holding the setpoint → debug reports the inactive sentinel (hidden).
	state := makeTestDynamicState()
	input := makeBaseDynamicInput()
	input.HouseLoad = 2000
	input.Solar1Power = 0
	input.Solar3BatteryCurrent = 20
	input.Solar4BatteryCurrent = 20
	input.Battery3CCL = 80
	input.Battery3Voltage = 53

	setpoint, debug := calculateDynamicSetpoint(input, state, noVoltLimit)
	assert.Equal(t, "Supply", debug.Priority)
	assert.InDelta(t, -2000.0, setpoint, 0.001)
	assert.InDelta(t, dynamicMaxChargeW, debug.CCLChargeMaxW, 0.001) // not binding → hidden
}

func TestCalculateDynamic_CCLChargeCap_ShownWhenNear(t *testing.T) {
	// CCL charge cap = 1855W. Charge intent only 1600W (set by house-side headroom), so the cap
	// isn't binding — but the setpoint is within 25% of it (255W < 463W) → still surfaced.
	state := makeTestDynamicState()
	input := makeBaseDynamicInput()
	input.HouseLoad = 0
	input.Solar1Power = 1600 // surplus charge intent + house-side charge headroom
	input.Solar3BatteryCurrent = 20
	input.Solar4BatteryCurrent = 20
	input.Battery3CCL = 80
	input.Battery3Voltage = 53

	setpoint, debug := calculateDynamicSetpoint(input, state, noVoltLimit)
	assert.InDelta(t, 1600.0, setpoint, 0.001)
	assert.InDelta(t, 1855.0, debug.CCLChargeMaxW, 0.001) // within 25% → shown
}

func TestCalculateDynamic_Headroom_HiddenWhenAmple(t *testing.T) {
	// Plenty of transfer headroom and not discharging near it → Headroom row hidden.
	state := makeTestDynamicState()
	input := makeBaseDynamicInput()

	_, debug := calculateDynamicSetpoint(input, state, noVoltLimit)
	assert.False(t, debug.HeadroomActive)
}

func TestCalculateDynamic_Headroom_ShownWhenDischargeNearCap(t *testing.T) {
	// busLoad=4000 → headroom=500, capping discharge at 500W. Supply wants more → setpoint=-500,
	// sitting on the headroom cap → Headroom row active.
	state := makeTestDynamicState()
	input := makeBaseDynamicInput()
	input.HouseLoad = 3000
	input.Solar1Power = 0
	input.PowerhouseNetPower = 4000

	setpoint, debug := calculateDynamicSetpoint(input, state, noVoltLimit)
	assert.InDelta(t, -500.0, setpoint, 0.001)
	assert.True(t, debug.HeadroomActive)
}

// --- CVL overflow integration in calculateDynamicSetpoint ---

func TestCalculateDynamic_CVLOverflow_SwitchesChargeToDischarge(t *testing.T) {
	// V=CVL with Solar34=500W → over-correct fraction (≈1.111) → floor ≈ 555.6W discharge.
	state := makeTestDynamicState()
	input := makeBaseDynamicInput()
	input.HouseLoad = 500
	input.Solar1Power = 1000 // surplus → Charge intent
	input.Solar34Power = 500
	input.Battery3CVL = 55.2
	input.Battery3Voltage = 55.2

	expected := cvlOverCorrectFraction() * 500
	setpoint, debug := calculateDynamicSetpoint(input, state, noVoltLimit)
	assert.InDelta(t, -expected, setpoint, 0.001)
	assert.InDelta(t, expected, debug.CVLOverflowW, 0.001)
}

func TestCalculateDynamic_CVLOverflow_HigherSupplyWins(t *testing.T) {
	// Mid-ramp CVL floor (250W) is less than 2000W supply intent → supply wins.
	state := makeTestDynamicState()
	input := makeBaseDynamicInput()
	input.HouseLoad = 2000
	input.Solar34Power = 500
	input.Battery3CVL = 55.2
	midOffset := (cvlOverflowRampStartV + cvlOverflowTargetOffsetV) / 2
	input.Battery3Voltage = 55.2 - midOffset

	setpoint, _ := calculateDynamicSetpoint(input, state, noVoltLimit)
	assert.InDelta(t, -2000.0, setpoint, 0.001)
}

func TestCalculateDynamic_CVLOverflow_SOCFull_DischargesAnyway(t *testing.T) {
	// SOC at 98% (MaxCharge=0) + V=CVL + Solar34=500W → CVL discharge floor still wins.
	state := makeTestDynamicState()
	input := makeBaseDynamicInput()
	input.Battery3SOC = 98
	input.HouseLoad = 500
	input.Solar1Power = 1000
	input.Solar34Power = 500
	input.Battery3CVL = 55.2
	input.Battery3Voltage = 55.2

	expected := cvlOverCorrectFraction() * 500
	setpoint, _ := calculateDynamicSetpoint(input, state, noVoltLimit)
	assert.InDelta(t, -expected, setpoint, 0.001)
}

// --- regression tests ---

func TestConstraint_OverLimit_DesiredChargeTakesPrecedence(t *testing.T) {
	// transferLimitConstraint(5000): headroom=-500 → MinCharge=500, MaxDischarge=0
	// An intent of 3500W charge should be preserved — 3500 ≥ floor(500).
	tl := transferLimitConstraint(5000)
	got := DynamicModeConstraint{Target: 3500,
		MaxDischarge: dynamicMaxDischargeW, MaxCharge: dynamicMaxChargeW}.add(tl).Setpoint()
	assert.InDelta(t, 3500.0, got, 0.001)
}

func TestCalculateDynamic_ChargeFromSurplus_OverLimitPreservesHigherCharge(t *testing.T) {
	// Solar1=2000, Inverter1to9=3000 → headroom=-500 → floor=500W
	// Surplus=5000W → desired=min(5000, 3500)=3500W → should not be replaced by floor.
	// SOC=50% (below charge limit) so the SOC limit doesn't interfere.
	state := makeTestDynamicState()
	input := makeBaseDynamicInput()
	input.Battery3SOC = 50
	input.HouseLoad = 0
	input.Solar1Power = 2000
	input.Inverter1to9Power = 3000
	input.PowerhouseNetPower = 5000

	setpoint, debug := calculateDynamicSetpoint(input, state, noVoltLimit)
	assert.Equal(t, priorityCharge, debug.Priority)
	assert.InDelta(t, 3500.0, setpoint, 0.001)
}

func TestCalculateDynamic_Headroom(t *testing.T) {
	state := makeTestDynamicState()
	input := makeBaseDynamicInput()
	input.HouseLoad = 3000
	input.Solar1Power = 500
	input.Inverter1to9Power = 1000
	input.PowerhouseNetPower = 1500

	_, debug := calculateDynamicSetpoint(input, state, noVoltLimit)
	// headroom = 4500 - 1500 = 3000
	assert.InDelta(t, 3000.0, debug.Headroom, 0.001)
	assert.InDelta(t, input.Battery3SOC, debug.Battery3SOC, 0.001)
}

// --- b3ForecastChargeLimit tests ---

// makeForecast builds 30-minute forecast periods of constant generation starting at `start`,
// spanning `hours`. Generation is fixed at 1kW, comfortably above b3SolarEndThresholdKw, so
// solar end time resolves to start+hours.
func makeForecast(start time.Time, hours float64) governor.ForecastPeriods {
	const pvKw = 1.0
	var periods governor.ForecastPeriods
	for i := 0; i < int(hours*2); i++ {
		periods = append(periods, governor.ForecastPeriod{
			PeriodStart: start.Add(time.Duration(i) * 30 * time.Minute),
			PvEstimate:  pvKw,
		})
	}
	return periods
}

func TestB3ForecastChargeLimit_AtTarget_NoCharge(t *testing.T) {
	c := b3ForecastChargeLimit(98, 43500, 0, 3.9, nil, time.Now())
	assert.InDelta(t, 0.0, c.MaxCharge, 0.001)
}

func TestB3ForecastChargeLimit_SolarCoversGap_NoCharge(t *testing.T) {
	// SOC=80 → gap=18%*43500=7830Wh. 3.9*2100=8190Wh ≥ gap → deficit≤0 → no charge.
	c := b3ForecastChargeLimit(80, 43500, 2100, 3.9, nil, time.Now())
	assert.InDelta(t, 0.0, c.MaxCharge, 0.001)
}

func TestB3ForecastChargeLimit_DeficitSpreadOverHours(t *testing.T) {
	// gap=7830, expected solar=3.9*500=1950 → deficit=5880 over 5h → 1176W.
	now := time.Date(2026, 6, 13, 12, 0, 0, 0, time.Local)
	c := b3ForecastChargeLimit(80, 43500, 500, 3.9, makeForecast(now, 5), now)
	assert.InDelta(t, 1176.0, c.MaxCharge, 0.001)
}

func TestB3ForecastChargeLimit_RateClampedToMax(t *testing.T) {
	// gap=78%*43500=33930, no solar, only 2h → 16965W → clamped to 3500W.
	now := time.Date(2026, 6, 13, 12, 0, 0, 0, time.Local)
	c := b3ForecastChargeLimit(20, 43500, 0, 3.9, makeForecast(now, 2), now)
	assert.InDelta(t, dynamicMaxChargeW, c.MaxCharge, 0.001)
}

func TestB3ForecastChargeLimit_SolarEnded_FullCharge(t *testing.T) {
	// Deficit remains but no forecast → solar end in the past → allow full charge.
	c := b3ForecastChargeLimit(80, 43500, 0, 3.9, nil, time.Now())
	assert.InDelta(t, dynamicMaxChargeW, c.MaxCharge, 0.001)
}

func TestCalculateDynamic_ChargeLimit_DeficitCaps(t *testing.T) {
	// SOC=80 → gap=7830; forecast remaining 500Wh → expected solar 1950 → deficit 5880 over
	// ~5h → ~1176W cap. Surplus=2000W charge intent is capped to the deficit rate.
	state := makeTestDynamicState()
	input := makeBaseDynamicInput()
	input.Battery3SOC = 80
	input.HouseLoad = 0
	input.Solar1Power = 2000
	input.ForecastRemainingWh = 500
	input.DetailedForecast = makeForecast(time.Now(), 5)

	setpoint, debug := calculateDynamicSetpoint(input, state, noVoltLimit)
	assert.Equal(t, priorityCharge, debug.Priority)
	assert.InDelta(t, 1176.0, setpoint, 2.0) // small tolerance for wall-clock between calls
	assert.InDelta(t, 1176.0, debug.B3ChargeMaxW, 2.0)
}

func TestCalculateDynamic_ChargeLimit_AtTarget_NoVoluntaryCharge(t *testing.T) {
	// SOC≥98%: MaxCharge=0. Surplus=2000W → charge intent blocked → 0W.
	state := makeTestDynamicState()
	input := makeBaseDynamicInput()
	input.Battery3SOC = 98
	input.HouseLoad = 0
	input.Solar1Power = 2000

	setpoint, _ := calculateDynamicSetpoint(input, state, noVoltLimit)
	assert.InDelta(t, 0.0, setpoint, 0.001)
}

func TestCalculateDynamic_ChargeLimit_AtTarget_TransferLimitStillCharges(t *testing.T) {
	// SOC=98% (MaxCharge=0) + over transfer limit (MinCharge=500, MaxDischarge=0).
	// lo=MinCharge(500)-MaxDischarge(0)=500, hi=MaxCharge(0) → lo>hi → 500W charge.
	state := makeTestDynamicState()
	input := makeBaseDynamicInput()
	input.Battery3SOC = 98
	input.HouseLoad = 0
	input.Solar1Power = 2000
	input.Inverter1to9Power = 3000
	input.PowerhouseNetPower = 5000 // over 4500W limit by 500W

	setpoint, _ := calculateDynamicSetpoint(input, state, noVoltLimit)
	assert.InDelta(t, 500.0, setpoint, 0.001)
}

func TestCalculateDynamic_ChargeLimit_AtTarget_SupplyUnaffected(t *testing.T) {
	// SOC=98% but house load > generation → supply intent → discharge unaffected by MaxCharge cap.
	state := makeTestDynamicState()
	input := makeBaseDynamicInput()
	input.Battery3SOC = 98
	input.HouseLoad = 2000
	input.Solar1Power = 0

	setpoint, debug := calculateDynamicSetpoint(input, state, noVoltLimit)
	assert.Equal(t, "Supply", debug.Priority)
	assert.InDelta(t, -2000.0, setpoint, 0.001)
}

func TestCalculateDynamic_ChargeLimit_AtTarget_CCLOverflowUnaffected(t *testing.T) {
	// SOC=98% + CCL overflow (MinDischarge=265W). MaxCharge=0 but discharge must still occur.
	state := makeTestDynamicState()
	input := makeBaseDynamicInput()
	input.Battery3SOC = 98
	input.HouseLoad = 500
	input.Solar1Power = 1000 // surplus → Charge intent (blocked by charge limit)
	input.Solar3BatteryCurrent = 40
	input.Solar4BatteryCurrent = 40
	input.Battery3CCL = 80
	input.Battery3Voltage = 53

	setpoint, _ := calculateDynamicSetpoint(input, state, noVoltLimit)
	assert.InDelta(t, -265.0, setpoint, 0.001)
}

// --- b3SOCDischargeLimit tests ---

func TestB3SOCDischargeLimit_AtZero_NoDischarge(t *testing.T) {
	c := b3SOCDischargeLimit(10)
	assert.InDelta(t, 0.0, c.MaxDischarge, 0.001)
}

func TestB3SOCDischargeLimit_BelowZero_NoDischarge(t *testing.T) {
	c := b3SOCDischargeLimit(5)
	assert.InDelta(t, 0.0, c.MaxDischarge, 0.001)
}

func TestB3SOCDischargeLimit_Midpoint_HalfRate(t *testing.T) {
	// SOC=15%: midpoint of 10→20 → fraction=0.5 → 1500W
	c := b3SOCDischargeLimit(15)
	assert.InDelta(t, dynamicMaxDischargeW*0.5, c.MaxDischarge, 0.001)
}

func TestB3SOCDischargeLimit_AtFull_NoLimit(t *testing.T) {
	c := b3SOCDischargeLimit(20)
	assert.InDelta(t, dynamicMaxDischargeW, c.MaxDischarge, 0.001)
}

func TestB3SOCDischargeLimit_AboveFull_NoLimit(t *testing.T) {
	c := b3SOCDischargeLimit(50)
	assert.InDelta(t, dynamicMaxDischargeW, c.MaxDischarge, 0.001)
}

func TestCalculateDynamic_B3DischargeLimit_SupplyCapped(t *testing.T) {
	// SOC=15% → MaxDischarge=1500. Supply wants 2000W discharge → capped to 1500W.
	state := makeTestDynamicState()
	input := makeBaseDynamicInput()
	input.Battery3SOC = 15
	input.HouseLoad = 2000
	input.Solar1Power = 0

	setpoint, debug := calculateDynamicSetpoint(input, state, noVoltLimit)
	assert.Equal(t, "Supply", debug.Priority)
	assert.InDelta(t, -1500.0, setpoint, 0.001)
	assert.InDelta(t, 1500.0, debug.B3DischargeMaxW, 0.001)
}

func TestCalculateDynamic_B3DischargeLimit_AtZero_NoDischarge(t *testing.T) {
	// SOC=10% → MaxDischarge=0. Supply intent fully suppressed.
	state := makeTestDynamicState()
	input := makeBaseDynamicInput()
	input.Battery3SOC = 10
	input.HouseLoad = 2000
	input.Solar1Power = 0

	setpoint, _ := calculateDynamicSetpoint(input, state, noVoltLimit)
	assert.InDelta(t, 0.0, setpoint, 0.001)
}

// --- powerwallLowOffset tests ---

func TestPowerwallLowOffset_AtFull_MaxOffset(t *testing.T) {
	assert.InDelta(t, pwOffsetMaxW, powerwallLowOffset(10), 0.001)
}

func TestPowerwallLowOffset_BelowFull_MaxOffset(t *testing.T) {
	assert.InDelta(t, pwOffsetMaxW, powerwallLowOffset(5), 0.001)
}

func TestPowerwallLowOffset_Midpoint_HalfOffset(t *testing.T) {
	assert.InDelta(t, pwOffsetMaxW*0.5, powerwallLowOffset(15), 0.001)
}

func TestPowerwallLowOffset_AtZero_NoOffset(t *testing.T) {
	assert.InDelta(t, 0.0, powerwallLowOffset(20), 0.001)
}

func TestPowerwallLowOffset_AboveZero_NoOffset(t *testing.T) {
	assert.InDelta(t, 0.0, powerwallLowOffset(30), 0.001)
}

func TestCalculateDynamic_PWOffset_AddsToSupplyDischarge(t *testing.T) {
	// PW=15% → +125W. Supply wants 2000W discharge → total 2125W discharge.
	state := makeTestDynamicState()
	input := makeBaseDynamicInput()
	input.PowerwallSOC = 15
	input.HouseLoad = 2000
	input.Solar1Power = 0

	setpoint, debug := calculateDynamicSetpoint(input, state, noVoltLimit)
	assert.Equal(t, "Supply", debug.Priority)
	assert.InDelta(t, -2125.0, setpoint, 0.001)
	assert.InDelta(t, 125.0, debug.PWOffsetW, 0.001)
}

func TestCalculateDynamic_PWOffset_ReducesChargeNotForcesDischarge(t *testing.T) {
	// PW=10% → +250W offset. Charge surplus only 100W → reduced to 0, not flipped to discharge.
	state := makeTestDynamicState()
	input := makeBaseDynamicInput()
	input.PowerwallSOC = 10
	input.HouseLoad = 0
	input.Solar1Power = 100 // 100W surplus → small charge intent

	setpoint, _ := calculateDynamicSetpoint(input, state, noVoltLimit)
	assert.InDelta(t, 0.0, setpoint, 0.001)
}

func TestCalculateDynamic_PWOffset_ReducesLargeCharge(t *testing.T) {
	// PW=10% → +250W. Charge surplus 1000W → reduced to 750W charge (still charging).
	state := makeTestDynamicState()
	input := makeBaseDynamicInput()
	input.PowerwallSOC = 10
	input.HouseLoad = 0
	input.Solar1Power = 1000

	setpoint, debug := calculateDynamicSetpoint(input, state, noVoltLimit)
	assert.Equal(t, priorityCharge, debug.Priority)
	assert.InDelta(t, 750.0, setpoint, 0.001)
}

func TestCalculateDynamic_PWOffset_CappedByB3DischargeLimit(t *testing.T) {
	// PW=10% (+250W discharge wanted) but B3 SOC=10% → MaxDischarge=0 → no discharge.
	state := makeTestDynamicState()
	input := makeBaseDynamicInput()
	input.PowerwallSOC = 10
	input.Battery3SOC = 10
	input.HouseLoad = 2000
	input.Solar1Power = 0

	setpoint, _ := calculateDynamicSetpoint(input, state, noVoltLimit)
	assert.InDelta(t, 0.0, setpoint, 0.001)
}

// TestDynamicOverflowScenario is a scratch test for manual exploration.
// Edit the values under "Scenario inputs" and run:
//
//	nix-shell --run 'go test ./src/... -run TestDynamicOverflowScenario -v'
//
// Each row shows one tick (≈1 second of real time). No ramp — the CCL overflow
// constraint is instantaneous and stateless, so convergence happens immediately.
func TestDynamicOverflowScenario(t *testing.T) {
	// --- Scenario inputs ---
	solar3CurrentA := 40.0  // A  (MPPT 3 battery-side current)
	solar4CurrentA := 40.0  // A  (MPPT 4 battery-side current)
	battery3CCL := 80.0     // A  (BMS charge current limit)
	battery3Voltage := 53.0 // V
	houseLoad := 500.0      // W  (house consumption)
	solar1Power := 1000.0   // W  (solar 1, house-side)
	inverter1to9 := 0.0     // W  (inverters 1–9 output, house-side)
	// ----------------------

	state := makeTestDynamicState()
	input := makeBaseDynamicInput()
	input.Solar3BatteryCurrent = solar3CurrentA
	input.Solar4BatteryCurrent = solar4CurrentA
	input.Battery3CCL = battery3CCL
	input.Battery3Voltage = battery3Voltage
	input.HouseLoad = houseLoad
	input.Solar1Power = solar1Power
	input.Inverter1to9Power = inverter1to9

	overflowA := (solar3CurrentA + solar4CurrentA) - (battery3CCL - cclOverflowHeadroomA)
	overflowW := max(0, overflowA) * battery3Voltage

	t.Logf("Inputs: solar3=%.0fA  solar4=%.0fA  CCL=%.0fA  V=%.1fV", solar3CurrentA, solar4CurrentA, battery3CCL, battery3Voltage)
	t.Logf("        house=%.0fW  solar1=%.0fW  inv1-9=%.0fW", houseLoad, solar1Power, inverter1to9)
	t.Logf("overflowA=%.1fA  overflowW=%.0fW (instantaneous, no ramp)", overflowA, overflowW)
	t.Logf("%-10s  %-10s  %-12s", "setpoint W", "cclOverflW", "priority")

	setpoint, debug := calculateDynamicSetpoint(input, state, noVoltLimit)
	t.Logf("%-10.0f  %-10.0f  %-12s", setpoint, debug.CCLOverflowW, debug.Priority)
}

// --- exportLimitConstraint tests ---
//
// Inputs are stated as the raw sensor pair (SitePower, MultiplusACPower) and
// converted with the same expression the controller uses, so these tests would
// catch a sign-convention regression in either direction.
// Reminder: SitePower +ve = importing, -ve = exporting; MultiplusACPower -ve = inverting.

func exportExcludingMP2(sitePower, multiplusACPower float64) float64 {
	return multiplusACPower - sitePower
}

func TestExportLimit_Importing_FullDischargeAllowed(t *testing.T) {
	// House importing 2kW with MP2 idle: nothing is reaching the grid, so the
	// intent passes through untouched.
	exp := exportLimitConstraint(exportExcludingMP2(2000, 0))
	got := DynamicModeConstraint{Target: -1000, MaxDischarge: dynamicMaxDischargeW, MaxCharge: dynamicMaxChargeW}.add(exp).Setpoint()
	assert.InDelta(t, -1000.0, got, 0.001)
}

func TestExportLimit_MP2OutputDoesNotEatOwnHeadroom(t *testing.T) {
	// The regression this guards: house exporting 3kW, but 3kW of that IS the
	// Multiplus. Excluding its own output the house exports 0, so the full
	// discharge intent must still pass. Were MP2 not stripped out, headroom would
	// read 2500 and the inverter would throttle itself for no reason.
	exp := exportLimitConstraint(exportExcludingMP2(-3000, -3000))
	got := DynamicModeConstraint{Target: -3000, MaxDischarge: dynamicMaxDischargeW, MaxCharge: dynamicMaxChargeW}.add(exp).Setpoint()
	assert.InDelta(t, -3000.0, got, 0.001)
}

func TestExportLimit_DischargeCapAtHeadroom(t *testing.T) {
	// House exporting 5kW with MP2 idle → headroom = 5500-5000 = 500W.
	exp := exportLimitConstraint(exportExcludingMP2(-5000, 0))
	got := DynamicModeConstraint{Target: -2000, MaxDischarge: dynamicMaxDischargeW, MaxCharge: dynamicMaxChargeW}.add(exp).Setpoint()
	assert.InDelta(t, -500.0, got, 0.001)
}

func TestExportLimit_OverLimit_ForcesAbsorption(t *testing.T) {
	// Exporting 6kW from solar/Powerwall alone → 500W over. MP2 must charge to
	// soak it up even with no charge intent.
	exp := exportLimitConstraint(exportExcludingMP2(-6000, 0))
	got := DynamicModeConstraint{MaxDischarge: dynamicMaxDischargeW, MaxCharge: dynamicMaxChargeW}.add(exp).Setpoint()
	assert.InDelta(t, 500.0, got, 0.001)
}

func TestExportLimit_OverLimit_LargeExcess_CapsAtMaxCharge(t *testing.T) {
	// Wildly over: forced absorption is still bounded by the charge rating.
	exp := exportLimitConstraint(exportExcludingMP2(-12000, 0))
	got := DynamicModeConstraint{MaxDischarge: dynamicMaxDischargeW, MaxCharge: dynamicMaxChargeW}.add(exp).Setpoint()
	assert.InDelta(t, dynamicMaxChargeW, got, 0.001)
}

func TestExportLimit_ImportingAllowsMoreThanTheLimit(t *testing.T) {
	// Importing 4kW: MP2 could discharge 9.5kW before export reached 5.5kW, so the
	// constraint must not bind at all — the discharge rating is the only cap.
	exp := exportLimitConstraint(exportExcludingMP2(4000, 0))
	assert.InDelta(t, dynamicMaxDischargeW, exp.MaxDischarge, 0.001)
}

func TestExportLimit_ChargeIntentAlwaysPassesThrough(t *testing.T) {
	// Charging reduces export, so it is never restricted by this constraint.
	exp := exportLimitConstraint(exportExcludingMP2(-6000, 0))
	got := DynamicModeConstraint{Target: 1000, MaxDischarge: dynamicMaxDischargeW, MaxCharge: dynamicMaxChargeW}.add(exp).Setpoint()
	assert.InDelta(t, 1000.0, got, 0.001)
}

// --- voltage discharge limit tests ---

func newVoltageState() *DynamicInverterState {
	return &DynamicInverterState{}
}

// The ceiling steps by measured time, not by the keepalive tick: the controller
// recalculates on every inbound reading, so assuming dynamicTick would slew the
// ceiling several times faster than the rates allow.
func TestVoltageCapDt_MeasuresElapsed(t *testing.T) {
	state := newVoltageState()
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	// Nothing to measure against yet, so the first step advances by nothing.
	assert.InDelta(t, 0.0, voltageCapDt(state, t0), 0.001)
	assert.InDelta(t, 1.0, voltageCapDt(state, t0.Add(time.Second)), 0.001)
}

// A long gap (dropped inputs, a stalled process) must not cash in as one huge
// step — that would move the ceiling further than the slew rate permits.
func TestVoltageCapDt_ClampsStallAndBackwardsClock(t *testing.T) {
	state := newVoltageState()
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	voltageCapDt(state, t0)

	assert.InDelta(t, voltageCapMaxDtSeconds, voltageCapDt(state, t0.Add(10*time.Minute)), 0.001)

	// A clock that jumps backwards must not integrate in reverse.
	voltageCapDt(state, t0.Add(time.Hour))
	assert.InDelta(t, 0.0, voltageCapDt(state, t0), 0.001)
}

// The incident case: Multiplus idle, voltage fine, so the old ceiling recovered
// to 3kW and let a CCL-overflow step jump straight there. The ceiling must sit
// just above what is actually running instead.
func TestVoltageCap_NeverWindsUpAboveActualOutput(t *testing.T) {
	state := newVoltageState()
	for i := 0; i < 100; i++ {
		updateVoltageCap(state, 245, 0, 1)
	}
	// Idle output: the ceiling sits at the margin plus at most one relax step.
	assert.InDelta(t, voltageAtLimitMarginW+voltageRelaxRateWps, state.voltageCapW, 0.001)

	cclStep := DynamicModeConstraint{MinDischarge: 2500, MaxDischarge: dynamicMaxDischargeW, MaxCharge: dynamicMaxChargeW}
	c := updateVoltageCap(state, 245, 0, 1)
	assert.InDelta(t, -c.MaxDischarge, cclStep.add(c).Setpoint(), 0.001)
	assert.LessOrEqual(t, c.MaxDischarge, voltageAtLimitMarginW+voltageRelaxRateWps)
}

// Through the whole chain: Multiplus idle, solar 3+4 well over a dropped CCL, so
// CCL overflow demands ~2.6kW. The setpoint must start from the idle output, not
// jump to the demand.
func TestDynamicSetpoint_CCLOverflowStepHeldByVoltageCeiling(t *testing.T) {
	state := makeTestDynamicState()
	input := makeBaseDynamicInput()
	input.HouseLoad = 0
	input.MultiplusACInVoltage = 248.6
	input.Battery3CCL = 50
	input.Solar3BatteryCurrent = 50
	input.Solar4BatteryCurrent = 50

	setpoint, _ := calculateDynamicSetpoint(input, state, stepVoltageCap(input, state))
	assert.InDelta(t, -voltageAtLimitMarginW, setpoint, 0.001)
}

// Pressed against the ceiling with voltage low, it relaxes at the relax rate.
func TestVoltageCap_RelaxesWhenAtLimitAndLow(t *testing.T) {
	state := newVoltageState()
	state.voltageCapInit = true
	state.voltageCapW = 1000

	updateVoltageCap(state, 248.6, 1000, 1)
	assert.InDelta(t, 1000+voltageRelaxRateWps, state.voltageCapW, 0.001)
}

// Voltage low but the output isn't using the ceiling: no relax, and the ceiling
// is pulled down to the output plus margin.
func TestVoltageCap_NoRelaxWhenNotAtLimit(t *testing.T) {
	state := newVoltageState()
	state.voltageCapInit = true
	state.voltageCapW = 1000

	updateVoltageCap(state, 248.6, 400, 1)
	assert.InDelta(t, 400+voltageAtLimitMarginW, state.voltageCapW, 0.001)
}

// Between the relax and throttle thresholds the ceiling holds.
func TestVoltageCap_HoldsInBand(t *testing.T) {
	state := newVoltageState()
	state.voltageCapInit = true
	state.voltageCapW = 1000

	for _, v := range []float64{249.0, 249.6, 250.6, 251.6, 252.0} {
		updateVoltageCap(state, v, 1000, 1)
		assert.InDelta(t, 1000.0, state.voltageCapW, 0.001, "moved at %.1fV", v)
	}
}

// Over the threshold it sheds from the current output, not from a ceiling
// that may be well above it.
func TestVoltageCap_ThrottlesFromActualOutput(t *testing.T) {
	state := newVoltageState()
	state.voltageCapInit = true
	state.voltageCapW = 1100 // within margin of the output
	updateVoltageCap(state, 252.6, 1000, 1)
	assert.InDelta(t, 1000-voltageShedRateWps, state.voltageCapW, 0.001)
}

// Sustained over-voltage bottoms out at 0 rather than banking curtailment.
func TestVoltageCap_ClampsAtZero(t *testing.T) {
	state := newVoltageState()
	for i := 0; i < 100; i++ {
		updateVoltageCap(state, 255, 0, 1)
	}
	assert.InDelta(t, 0.0, state.voltageCapW, 0.001)
}

// No reading holds the ceiling (still bounded to the output). Relaxing on
// absent data would silently disable the limit.
func TestVoltageCap_NoReadingHolds(t *testing.T) {
	state := newVoltageState()
	state.voltageCapInit = true
	state.voltageCapW = 800

	updateVoltageCap(state, 0, 800, 1)
	assert.InDelta(t, 800.0, state.voltageCapW, 0.001)
}

// Charging is never restricted: it pulls voltage down, so it is the remedy.
func TestVoltageCap_NeverLimitsCharging(t *testing.T) {
	state := newVoltageState()
	for i := 0; i < 100; i++ {
		updateVoltageCap(state, 255, 0, 1)
	}
	c := updateVoltageCap(state, 255, 0, 1)
	assert.InDelta(t, dynamicMaxChargeW, c.MaxCharge, 0.001)

	got := DynamicModeConstraint{Target: 1000, MaxDischarge: dynamicMaxDischargeW, MaxCharge: dynamicMaxChargeW}.add(c).Setpoint()
	assert.InDelta(t, 1000.0, got, 0.001)
}

// --- MP2 lag compensation in the transfer/export limits ---

// The incident feedback loop: net power is stale (doesn't yet include the
// Multiplus's new output) while the Multiplus reading is fresh. Stripping the
// fresh reading out of the stale total counted the new discharge as headroom.
func TestDynamicSetpoint_HeadroomNotInflatedByMP2Ramp(t *testing.T) {
	state := makeTestDynamicState()
	input := makeBaseDynamicInput()
	input.HouseLoad = 5000 // Supply wants as much as it can get
	input.PowerhouseNetPower = 3000
	input.MultiplusACPower = 0
	_, debug := calculateDynamicSetpoint(input, state, noVoltLimit)
	assert.InDelta(t, 1500.0, debug.Headroom, 0.001)

	// Multiplus now discharging 1500W; net power hasn't caught up.
	input.MultiplusACPower = -1500
	_, debug = calculateDynamicSetpoint(input, state, noVoltLimit)
	assert.InDelta(t, 1500.0, debug.Headroom, 0.001, "fresh MP2 discharge must not count as headroom")
}

func TestDynamicSetpoint_ExportHeadroomNotInflatedByMP2Ramp(t *testing.T) {
	state := makeTestDynamicState()
	input := makeBaseDynamicInput()
	input.HouseLoad = 5000
	input.SitePower = -5000 // exporting 5000 with the Multiplus idle
	input.MultiplusACPower = 0
	calculateDynamicSetpoint(input, state, noVoltLimit)

	// Multiplus discharging 2000W; site power hasn't caught up. Export headroom
	// must still be 500W, not 2500W.
	input.MultiplusACPower = -2000
	setpoint, _ := calculateDynamicSetpoint(input, state, noVoltLimit)
	assert.GreaterOrEqual(t, setpoint, -500.0-0.001)
}

// TestCalculateDynamic_ChargeLimit_OvernightZeroForecast covers the regression where an
// overnight forecast sample of 0 (Solcast reports 0 remaining from sunset until it
// recalculates after midnight) poisoned the rest of the day's charge limit.
func TestCalculateDynamic_ChargeLimit_OvernightZeroForecast(t *testing.T) {
	state := makeTestDynamicState()

	// Overnight tick: Solcast still reporting 0 remaining for the day just ended.
	night := makeBaseDynamicInput()
	night.Battery3SOC = 80
	night.ForecastRemainingWh = 0
	calculateDynamicSetpoint(night, state, noVoltLimit)

	// Morning tick on the same state: Solcast has published the new day's forecast.
	// gap=7830, expected solar=3.9*500=1950 → deficit=5880 over ~5h → ~1176W.
	morning := makeBaseDynamicInput()
	morning.Battery3SOC = 80
	morning.HouseLoad = 0
	morning.Solar1Power = 2000
	morning.ForecastRemainingWh = 500
	morning.DetailedForecast = makeForecast(time.Now(), 5)

	_, debug := calculateDynamicSetpoint(morning, state, noVoltLimit)
	assert.InDelta(t, 1176.0, debug.B3ChargeMaxW, 2.0)
}
