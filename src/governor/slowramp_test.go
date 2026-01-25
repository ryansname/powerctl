package governor

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// testConfig returns a simple config for testing updatePressure behavior.
// Deadband=100, DoublePressureDiff=1000, DecayMultiplier=4, PressureCapSeconds=100
func testConfig() SlowRampConfig {
	return SlowRampConfig{
		ThresholdSeconds:   30,
		PressureCapSeconds: 100,
		RateAccel:          1.0,
		DecayMultiplier:    4.0,
		DoublePressureDiff: 1000,
		Deadband:           100,
	}
}

func TestUpdatePressure(t *testing.T) {
	config := testConfig()

	tests := []struct {
		name         string
		pressure     float64 // Initial pressure
		diff         float64 // Target - Current
		wantPressure float64 // Expected pressure after update
	}{
		// Building pressure (diff and pressure same direction or pressure=0)
		{"build_pos_normal", 0, 200, 1},
		{"build_neg_normal", 0, -200, -1},
		{"build_pos_double", 0, 1500, 2},   // > DoublePressureDiff
		{"build_neg_double", 0, -1500, -2}, // > DoublePressureDiff
		{"build_pos_continues", 10, 200, 11},
		{"build_neg_continues", -10, -200, -11},

		// Draining pressure (diff and pressure opposite directions)
		{"drain_pos_pressure", 20, -200, 16},  // 20 - 4 = 16
		{"drain_neg_pressure", -20, 200, -16}, // -20 + 4 = -16

		// Inner deadband (diff <= Deadband/2 = 50) - drains
		{"inner_deadband_pos", 20, 25, 16},   // drains at 4x
		{"inner_deadband_neg", -20, -25, -16},
		{"inner_deadband_zero", 20, 0, 16},   // diff=0 is inner deadband
		{"inner_deadband_exact", 20, 50, 16}, // exactly at boundary

		// Outer deadband (Deadband/2 < diff <= Deadband)
		{"outer_deadband_building_pos", 20, 75, 20}, // Neutral when building
		{"outer_deadband_building_neg", -20, -75, -20},
		{"outer_deadband_draining", 20, -75, 16}, // Drains when wrong dir (bug fix!)
		{"outer_deadband_exact", 20, 100, 20},    // exactly at boundary

		// Pressure cap
		{"cap_positive", 99, 200, 100},
		{"cap_negative", -99, -200, -100},

		// Drain clamp (don't overshoot zero)
		{"drain_clamp_small", 2, -200, 0},   // 2-4 would be -2, clamp to 0
		{"drain_clamp_neg", -2, 200, 0},     // -2+4 would be 2, clamp to 0
		{"drain_exact", 4, -200, 0},         // exactly drains to 0
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := &SlowRampState{Pressure: tt.pressure, initialized: true}
			state.updatePressure(tt.diff, 1.0, config)
			assert.Equal(t, tt.wantPressure, state.Pressure)
		})
	}
}

func TestUpdate(t *testing.T) {
	config := SlowRampConfig{
		ThresholdSeconds:   30,
		PressureCapSeconds: 60,
		RateAccel:          1.0,
		DecayMultiplier:    4.0,
		DoublePressureDiff: 1e9, // disabled
	}

	tests := []struct {
		name        string
		current     float64
		pressure    float64
		initialized bool
		target      float64
		wantCurrent float64
	}{
		// First call initialization
		{"init_first_call", 0, 0, false, 1000, 1000},

		// Below threshold - no movement
		{"below_threshold", 500, 20, true, 1000, 500},

		// Above threshold with same direction - moves toward target
		// updatePressure runs first: 40+1=41, then progress = 41-30 = 11, maxRate = 11² = 121
		{"above_threshold_same_dir", 500, 40, true, 1000, 621},

		// Above threshold but opposite direction - no movement
		{"above_threshold_opp_dir", 500, 40, true, 0, 500},

		// Step capped to remaining diff (no overshoot)
		// progress = 40-30 = 10, maxRate = 100, but diff = 50
		{"step_capped_to_diff", 950, 40, true, 1000, 1000},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := &SlowRampState{
				Current:     tt.current,
				Pressure:    tt.pressure,
				initialized: tt.initialized,
			}
			result := state.Update(tt.target, config)
			assert.Equal(t, tt.wantCurrent, result)
		})
	}
}

// Helper to repeat a value n times
func repeat(value float64, n int) []float64 {
	result := make([]float64, n)
	for i := range result {
		result[i] = value
	}
	return result
}

// Helper to create oscillating sequence: n up, n down
func oscillate(up, down float64, n int) []float64 {
	result := make([]float64, n*2)
	for i := range n {
		result[i] = up
	}
	for i := range n {
		result[n+i] = down
	}
	return result
}

func TestPressureSequences(t *testing.T) {
	config := SlowRampConfig{
		ThresholdSeconds:   30,
		PressureCapSeconds: 60,
		RateAccel:          1.0,
		DecayMultiplier:    4.0,
		DoublePressureDiff: 1e9, // disabled
	}

	tests := []struct {
		name         string
		diffs        []float64
		wantPressure float64
	}{
		{"build_10_steps", repeat(200, 10), 10},
		{"build_to_cap", repeat(200, 100), 60}, // caps at 60
		// 10 up at 1x = 10, then 10 down: drain 4x (10→6→2→0 in 3 ticks), then build -1 for 7 ticks = -7
		{"oscillation_drains_then_builds", oscillate(500, -500, 10), -7},
		// After 20 up: pressure = 20
		// Then 10 down: drain 4x (20→16→12→8→4→0 in 5 ticks), then build -1 for 5 ticks = -5
		{"build_then_reverse", append(repeat(200, 20), repeat(-200, 10)...), -5},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := &SlowRampState{initialized: true}
			for _, diff := range tt.diffs {
				state.updatePressure(diff, 1.0, config)
			}
			assert.Equal(t, tt.wantPressure, state.Pressure)
		})
	}
}

func TestSlowRamp_NeverOvershoots(t *testing.T) {
	config := SlowRampConfig{
		ThresholdSeconds:   30,
		PressureCapSeconds: 60,
		RateAccel:          1.0,
		DecayMultiplier:    4.0,
		DoublePressureDiff: 1e9,
	}

	state := SlowRampState{}
	state.Update(0, config) // initialize

	target := 100.0
	for i := range 200 {
		result := state.Update(target, config)
		assert.LessOrEqual(t, result, target, "Should never overshoot target at t=%d", i)
	}
	assert.InDelta(t, target, state.Current, 0.1, "Should reach target")
}

func TestSlowRamp_AcceleratesOverTime(t *testing.T) {
	config := SlowRampConfig{
		ThresholdSeconds:   30,
		PressureCapSeconds: 60,
		RateAccel:          1.0,
		DecayMultiplier:    4.0,
		DoublePressureDiff: 1e9,
	}

	state := SlowRampState{}
	state.Update(0, config) // initialize

	// Track values over time toward target of 10000
	values := make([]float64, 90)
	for i := range 90 {
		values[i] = state.Update(10000, config)
	}

	// First 30 seconds: no movement (below threshold)
	for i := range 30 {
		assert.Equal(t, 0.0, values[i], "Should not move before threshold at t=%d", i)
	}

	// After threshold: deltas should increase (acceleration)
	earlyDelta := values[35] - values[34]
	lateDelta := values[55] - values[54]
	assert.Greater(t, lateDelta, earlyDelta, "Should accelerate over time")
}

func TestSlowRamp_MaxRateAtCap(t *testing.T) {
	config := SlowRampConfig{
		ThresholdSeconds:   30,
		PressureCapSeconds: 60,
		RateAccel:          1.0,
		DecayMultiplier:    4.0,
		DoublePressureDiff: 1e9,
	}

	state := SlowRampState{}
	state.Update(0, config) // initialize

	// Build to max pressure with large target
	for range 100 {
		state.Update(1e9, config)
	}

	// At max pressure (60), progressSeconds = 60 - 30 = 30
	// maxRate = 1.0 * 30² = 900 W/s
	before := state.Current
	state.Update(1e9, config)
	delta := state.Current - before

	assert.InDelta(t, 900.0, delta, 1.0, "Max rate should be 900 W/s at pressure cap")
}

func TestSlowRamp_DoesNotRampAwayFromTarget(t *testing.T) {
	config := SlowRampConfig{
		ThresholdSeconds:   30,
		PressureCapSeconds: 60,
		RateAccel:          1.0,
		DecayMultiplier:    4.0,
		DoublePressureDiff: 1e9,
	}

	state := SlowRampState{}
	state.Update(500, config) // initialize at 500

	// Build positive pressure past threshold
	for range 35 {
		state.Update(1000, config)
	}
	currentBeforeDrop := state.Current

	// Drop target below current - should not decrease
	for range 10 {
		result := state.Update(0, config)
		assert.GreaterOrEqual(t, result, currentBeforeDrop,
			"Should not ramp away from target when pressure/diff disagree")
	}
}
