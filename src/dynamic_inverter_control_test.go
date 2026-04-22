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

// --- applyTransferLimit tests ---

func TestApplyTransferLimit_NoGeneration(t *testing.T) {
	// No solar/inverters: full headroom = 4500W, discharge freely up to 3000W
	setpoint := applyTransferLimit(-1000, 0, 0)
	assert.InDelta(t, -1000.0, setpoint, 0.001)
}

func TestApplyTransferLimit_DischargeCapAtHeadroom(t *testing.T) {
	// solar=1kW + i1-9=3kW = 4kW → headroom=500W → discharge capped at 500W
	setpoint := applyTransferLimit(-2000, 1000, 3000)
	assert.InDelta(t, -500.0, setpoint, 0.001)
}

func TestApplyTransferLimit_OverLimit_ForcesCharge(t *testing.T) {
	// solar=2kW + i1-9=3kW = 5kW → headroom=-500W → must charge 500W
	setpoint := applyTransferLimit(0, 2000, 3000)
	assert.InDelta(t, 500.0, setpoint, 0.001)
}

func TestApplyTransferLimit_OverLimit_LargeExcess(t *testing.T) {
	// solar=0 + i1-9=8kW → headroom=-3.5kW → force charge capped at 3500W
	setpoint := applyTransferLimit(0, 0, 8000)
	assert.InDelta(t, 3500.0, setpoint, 0.001)
}

func TestApplyTransferLimit_ChargeAllowed(t *testing.T) {
	// Desired charge of 1000W with plenty of headroom
	setpoint := applyTransferLimit(1000, 500, 500)
	assert.InDelta(t, 1000.0, setpoint, 0.001)
}

func TestApplyTransferLimit_DischargeCapAt3000(t *testing.T) {
	// No generation, desired -5000W → capped at -3000W (max discharge)
	setpoint := applyTransferLimit(-5000, 0, 0)
	assert.InDelta(t, -3000.0, setpoint, 0.001)
}

func TestApplyTransferLimit_ChargeCapAt3500(t *testing.T) {
	// Desired charge way over max
	setpoint := applyTransferLimit(5000, 0, 0)
	assert.InDelta(t, 3500.0, setpoint, 0.001)
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
	// High freq + over transfer limit: forced charge still applies (absorbing excess is ideal)
	state := makeTestDynamicState()
	input := makeBaseDynamicInput()
	input.ACFreqP100_5Min = 53.0
	input.Solar1Power = 2000
	input.Inverter1to9Power = 3000 // headroom = -500 → forced charge 500W; safety keeps it

	setpoint, debug := calculateDynamicSetpoint(input, state)
	assert.InDelta(t, 500.0, setpoint, 0.001)
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
	state := makeTestDynamicState()
	input := makeBaseDynamicInput()
	input.HouseLoad = 0
	input.Solar1Power = 2000
	input.Inverter1to9Power = 4000 // total on bus = 6kW > 4500W → headroom = -1500
	// P3: desired = min(2000+4000, 3500) = 3500, but headroom < 0 → force charge 1500W

	setpoint, debug := calculateDynamicSetpoint(input, state)
	assert.Equal(t, "Charge", debug.Priority)
	assert.InDelta(t, 1500.0, setpoint, 0.001)
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
