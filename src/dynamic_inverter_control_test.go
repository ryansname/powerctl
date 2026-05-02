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
		Battery3CCL:        150.0,
		Battery3Voltage:    53.0,
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
	sfty := safetyConstraint(true)              // MaxDischarge=0
	got := DynamicModeConstraint{MaxDischarge: dynamicMaxDischargeW, MaxCharge: dynamicMaxChargeW}.add(sfty).add(c).Setpoint()
	assert.InDelta(t, 0.0, got, 0.001)
}

func TestCCLOverflow_TransferLimitOver_ChargeWins(t *testing.T) {
	// Transfer limit exceeded (MinCharge=500, MaxDischarge=0) AND CCL overflow (MinDischarge=265).
	// lo=500, hi=-265 → lo>hi → clamp returns lo=500 (charge wins, bus safety takes priority).
	tl := transferLimitConstraint(5000) // headroom=-500 → MinCharge=500
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

func TestCCLOverflow_ExactlyAtTarget_NoConstraint(t *testing.T) {
	// Solar = CCL - headroomA exactly → overflowA=0 → no constraint
	// CCL=80A, headroom=5A → target=75A; solar3=37.5A+solar4=37.5A=75A
	c := cclOverflowConstraint(37.5, 37.5, 80, 53)
	assert.InDelta(t, 0.0, c.MinDischarge, 0.001)
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
	input.Inverter1to9Power = 3000
	input.PowerhouseNetPower = 5000

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
	// target = 5000 - (1000+0+3000) = 1000, desired = -1000, headroom = 4500-4000 = 500 → -500
	input.HouseLoad = 5000
	input.Solar1Power = 1000
	input.Inverter1to9Power = 3000
	input.Solar2Power = 0
	input.PowerhouseNetPower = 4000

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
	input.Inverter1to9Power = 4000
	input.PowerhouseNetPower = 6000

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
	// headroom = 4500-4000 = 500W, so max discharge is -500W (or no discharge)
	// desired = 3500 (charge), 3500 > 0 so no clamping needed, just cap at 3500

	setpoint, debug := calculateDynamicSetpoint(input, state)
	assert.Equal(t, "Charge", debug.Priority)
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

	setpoint, debug := calculateDynamicSetpoint(input, state)
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

	setpoint, _ := calculateDynamicSetpoint(input, state)
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

	setpoint, _ := calculateDynamicSetpoint(input, state)
	assert.InDelta(t, -2000.0, setpoint, 0.001)
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
	state := makeTestDynamicState()
	input := makeBaseDynamicInput()
	input.HouseLoad = 0
	input.Solar1Power = 2000
	input.Inverter1to9Power = 3000
	input.PowerhouseNetPower = 5000

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
	input.PowerhouseNetPower = 1500

	_, debug := calculateDynamicSetpoint(input, state)
	// headroom = 4500 - 1500 = 3000
	assert.InDelta(t, 3000.0, debug.Headroom, 0.001)
	assert.InDelta(t, input.Battery3SOC, debug.Battery3SOC, 0.001)
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
	battery3CCL    := 80.0  // A  (BMS charge current limit)
	battery3Voltage := 53.0 // V
	houseLoad      := 500.0 // W  (house consumption)
	solar1Power    := 1000.0 // W  (solar 1, house-side)
	inverter1to9   := 0.0   // W  (inverters 1–9 output, house-side)
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

	setpoint, debug := calculateDynamicSetpoint(input, state)
	t.Logf("%-10.0f  %-10.0f  %-12s", setpoint, debug.CCLOverflowW, debug.Priority)
}
