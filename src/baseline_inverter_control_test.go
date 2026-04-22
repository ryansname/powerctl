package main

import (
	"testing"

	"github.com/ryansname/powerctl/src/governor"
	"github.com/stretchr/testify/assert"
)

func makeTestBaselineConfig() BaselineInverterConfig {
	inverters := []InverterInfo{
		{EntityID: "switch.inv1", StateTopic: "ha/switch/inv1/state"},
		{EntityID: "switch.inv2", StateTopic: "ha/switch/inv2/state"},
		{EntityID: "switch.inv3", StateTopic: "ha/switch/inv3/state"},
	}
	group := BatteryInverterGroup{
		Name:                 "Battery 2",
		Inverters:            inverters,
		ChargeStateTopic:     "b2charge",
		SOCTopic:             "b2soc",
		BatteryVoltageTopic:  "b2volt",
		CapacityWh:           9500,
		SolarMultiplier:      3.9,
		AvailableEnergyTopic: "b2energy",
	}
	return BaselineInverterConfig{
		Battery2:                 group,
		WattsPerInverter:         255.0,
		MaxTransferPower:         5000.0,
		MaxBaselineWatts:         500.0,
		OverflowSOCTurnOffStart:  98.5,
		OverflowSOCTurnOffEnd:    95.0,
		OverflowSOCTurnOnStart:   95.75,
		OverflowSOCTurnOnEnd:     99.5,
		LowVoltageTurnOnStart:  52.0,
		LowVoltageTurnOnEnd:    53.0,
		LowVoltageTurnOffStart: 50.75,
		LowVoltageTurnOffEnd:   52.0,
	}
}

func makeBlankBaselineState(config BaselineInverterConfig) *BaselineInverterState {
	b2Count := len(config.Battery2.Inverters)
	state := &BaselineInverterState{
		overflow2: BatteryOverflowState{
			Hysteresis: governor.NewSteppedHysteresis(
				b2Count, true,
				config.OverflowSOCTurnOnStart, config.OverflowSOCTurnOnEnd,
				config.OverflowSOCTurnOffStart, config.OverflowSOCTurnOffEnd,
			),
		},
		gridOffSolarMax:    governor.NewRollingMinMax(60),
		battery2VoltageMin: governor.NewRollingMinMax(15),
		houseLoadHourly:    governor.NewRollingMinMaxHours(168),
		targetMinusSolar:   governor.NewRollingMinMax(60),
		socLimit2:          governor.NewSteppedHysteresis(b2Count, true, 15, 25, 12.5, 22.5),
		powerCutAllow2:     governor.NewSteppedHysteresis(1, true, 53, 53, 47, 47),
		lowVoltage2: governor.NewSteppedHysteresis(
			b2Count, true,
			config.LowVoltageTurnOnStart, config.LowVoltageTurnOnEnd,
			config.LowVoltageTurnOffStart, config.LowVoltageTurnOffEnd,
		),
	}
	state.socLimit2.Current = b2Count
	state.lowVoltage2.Current = b2Count
	return state
}

// makeBaselineInput returns a sensible default BaselineInput for tests.
// Individual fields can be overridden per-test.
func makeBaselineInput() BaselineInput {
	return BaselineInput{
		Battery2SOC:         80.0,
		Battery2ChargeState: "Bulk Charging",
		Battery2Voltage:     51.5,
		Battery2EnergyWh:    8000,
		Solar1Power:         0,
		Solar1P90_15Min:     0,
		Solar2Power:         0,
		HouseLoad:           0,
		GridAvailable:       true,
		ACFrequency:         50.0,
		ACFreqP100_5Min:     50.0,
		ForecastRemainingWh: 0,
		DetailedForecast:    governor.ForecastPeriods{},
		InverterStates:      []bool{false, false, false},
		Battery3SOC:         90.0,
		PowerwallSOC:        50.0,
		ExpectingPowerCuts:  false,
	}
}

// findMode returns the ModeState with the given name, or nil if not found.
func findMode(modes []ModeState, name string) *ModeState {
	for i := range modes {
		if modes[i].Name == name {
			return &modes[i]
		}
	}
	return nil
}

// --- calculateBaseline tests ---

func TestCalculateBaseline_ZeroHouseLoad(t *testing.T) {
	config := makeTestBaselineConfig()
	state := makeBlankBaselineState(config)

	req := calculateBaseline(0, 0, 0, 500, state)
	assert.Equal(t, "Baseline", req.Name)
	assert.InDelta(t, 0.0, req.Watts, 0.001)
}

func TestCalculateBaseline_CapAt500W(t *testing.T) {
	config := makeTestBaselineConfig()
	state := makeBlankBaselineState(config)

	req := calculateBaseline(2000, 0, 0, 500, state)
	assert.InDelta(t, 500.0, req.Watts, 0.001)
}

func TestCalculateBaseline_SolarOffsets(t *testing.T) {
	config := makeTestBaselineConfig()
	state := makeBlankBaselineState(config)

	// House load 800W, solar covers 600W → baseline needed = 200W
	req := calculateBaseline(800, 400, 200, 500, state)
	assert.InDelta(t, 200.0, req.Watts, 0.001)
}

func TestCalculateBaseline_SolarExceedsLoad(t *testing.T) {
	config := makeTestBaselineConfig()
	state := makeBlankBaselineState(config)

	req := calculateBaseline(500, 400, 200, 500, state)
	assert.InDelta(t, 0.0, req.Watts, 0.001)
}

// --- selectBaselineMode tests ---

func TestSelectBaselineMode_HighFrequencySafety(t *testing.T) {
	config := makeTestBaselineConfig()
	state := makeBlankBaselineState(config)
	input := makeBaselineInput()
	input.ACFreqP100_5Min = 53.0

	count, debug := selectBaselineMode(input, config, state)
	assert.Equal(t, 0, count)
	assert.Equal(t, "High frequency", debug.SafetyReason)
}

func TestSelectBaselineMode_GridOffHighPowerwall(t *testing.T) {
	config := makeTestBaselineConfig()
	state := makeBlankBaselineState(config)
	input := makeBaselineInput()
	input.GridAvailable = false
	input.PowerwallSOC = 91.0

	count, debug := selectBaselineMode(input, config, state)
	assert.Equal(t, 0, count)
	assert.Equal(t, "Grid off + high Powerwall", debug.SafetyReason)
}

func TestSelectBaselineMode_BaselineContributes(t *testing.T) {
	config := makeTestBaselineConfig()
	state := makeBlankBaselineState(config)
	input := makeBaselineInput()
	input.HouseLoad = 1000 // 1000W capped at 500W → ceil(500/255) = 2 inverters

	count, debug := selectBaselineMode(input, config, state)
	assert.Equal(t, 2, count)
	assert.Empty(t, debug.SafetyReason)

	baselineMode := findMode(debug.Modes, "Baseline")
	assert.NotNil(t, baselineMode)
	assert.InDelta(t, 500.0, baselineMode.Watts, 0.001)
	assert.True(t, baselineMode.Contributing)
}

func TestSelectBaselineMode_OverflowWins(t *testing.T) {
	config := makeTestBaselineConfig()
	state := makeBlankBaselineState(config)
	input := makeBaselineInput()
	input.Battery2ChargeState = floatChargingState
	input.Battery2SOC = 100.0
	input.HouseLoad = 200 // Low house load → baseline < overflow

	count, debug := selectBaselineMode(input, config, state)

	// Overflow at 100% SOC → all 3 inverters = 765W
	assert.Equal(t, 3, count)

	overflowMode := findMode(debug.Modes, "Overflow")
	assert.NotNil(t, overflowMode)
	assert.True(t, overflowMode.Contributing)

	baselineMode := findMode(debug.Modes, "Baseline")
	assert.NotNil(t, baselineMode)
	assert.False(t, baselineMode.Contributing)
}

func TestSelectBaselineMode_TransferLimitApplied(t *testing.T) {
	config := makeTestBaselineConfig()
	state := makeBlankBaselineState(config)
	input := makeBaselineInput()
	input.Battery2ChargeState = floatChargingState
	input.Battery2SOC = 100.0      // Overflow → 3 inverters desired
	input.Battery3SOC = 90.0       // ≥85% → transfer limit applies
	input.Solar1P90_15Min = 4500.0 // limit = 5000-4500 = 500W → int(500/255) = 1

	count, _ := selectBaselineMode(input, config, state)
	assert.Equal(t, 1, count)
}

func TestSelectBaselineMode_TransferLimitSkipped(t *testing.T) {
	config := makeTestBaselineConfig()
	state := makeBlankBaselineState(config)
	input := makeBaselineInput()
	input.Battery2ChargeState = floatChargingState
	input.Battery2SOC = 100.0      // Overflow → 3 inverters desired
	input.Battery3SOC = 80.0       // <85% → transfer limit skipped
	input.Solar1P90_15Min = 4500.0 // Would normally cap to 1, but skipped

	count, _ := selectBaselineMode(input, config, state)
	assert.Equal(t, 3, count)
}
