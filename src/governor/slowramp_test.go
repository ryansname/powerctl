package governor

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// testConfig returns a simple config for testing updatePressure behavior.
// FullPressureDiff=100, DecayMultiplier=4, PressureCapSeconds=100
func testConfig() SlowRampConfig {
	return SlowRampConfig{
		ThresholdSeconds:   30,
		PressureCapSeconds: 100,
		RateLinear:         0.0, // Purely quadratic for test compatibility
		RateQuadratic:      1.0, // rate = p²
		DecayMultiplier:    4.0,
		FullPressureDiff:   100,
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
		// Building pressure - rate scales linearly with diff (rate = diff/FullPressureDiff)
		{"build_pos_2x", 0, 200, 2},       // rate = 200/100 = 2.0
		{"build_neg_2x", 0, -200, -2},
		{"build_pos_15x", 0, 1500, 15},    // rate = 1500/100 = 15.0
		{"build_neg_15x", 0, -1500, -15},
		{"build_pos_continues", 10, 200, 12}, // rate = 2.0, so 10 + 2 = 12
		{"build_neg_continues", -10, -200, -12},

		// Draining pressure (diff and pressure opposite directions)
		// drain rate = (diff/FullPressureDiff) * DecayMultiplier = 2 * 4 = 8
		{"drain_pos_pressure", 20, -200, 12},  // 20 - 8 = 12
		{"drain_neg_pressure", -20, 200, -12}, // -20 + 8 = -12

		// Rate scales linearly at all diff values
		{"rate_quarter", 20, 25, 20.25},       // rate = 25/100 = 0.25
		{"rate_quarter_neg", -20, -25, -20.25},
		{"rate_zero", 20, 0, 20},              // rate = 0/100 = 0 (no change)
		{"rate_half", 20, 50, 20.5},           // rate = 50/100 = 0.5
		{"rate_three_quarter", 20, 75, 20.75}, // rate = 75/100 = 0.75
		{"rate_three_quarter_neg", -20, -75, -20.75},
		{"rate_1x", 20, 100, 21},              // rate = 100/100 = 1.0
		{"rate_1_5x", 20, 150, 21.5},          // rate = 150/100 = 1.5

		// Wrong direction still drains (drain rate = 0.75 * 4 = 3)
		{"wrong_dir_drains", 20, -75, 17}, // mPressure > 0 but diff < 0, 20 - 3 = 17

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
		RateLinear:         0.0,
		RateQuadratic:      1.0, // rate = p²
		DecayMultiplier:    4.0,
		FullPressureDiff:   500, // diff=500 gives rate=1.0
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
	// Use FullPressureDiff=200 so diff=200 gives rate=1.0, diff=500 gives rate=2.5
	config := SlowRampConfig{
		ThresholdSeconds:   30,
		PressureCapSeconds: 60,
		RateLinear:         0.0,
		RateQuadratic:      1.0,
		DecayMultiplier:    4.0,
		FullPressureDiff:   200,
	}

	tests := []struct {
		name         string
		diffs        []float64
		wantPressure float64
	}{
		{"build_10_steps", repeat(200, 10), 10},
		{"build_to_cap", repeat(200, 100), 60}, // caps at 60
		// 10 up at rate=2.5 = 25, then 10 down: drain at rate 10 (25→15→5→0 in 3 ticks),
		// then build at rate=-2.5 for 7 ticks = -17.5
		{"oscillation_drains_then_builds", oscillate(500, -500, 10), -17.5},
		// After 20 up: pressure = 20
		// Then 10 down: drain at rate 4 (20→16→12→8→4→0 in 5 ticks), then build -1 for 5 ticks = -5
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
		RateLinear:         0.0,
		RateQuadratic:      1.0,
		DecayMultiplier:    4.0,
		FullPressureDiff:   200, // diff=200 gives rate=1.0
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
		RateLinear:         0.0,
		RateQuadratic:      1.0,
		DecayMultiplier:    4.0,
		FullPressureDiff:   10000, // diff=10000 gives rate=1.0
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
		RateLinear:         0.0,
		RateQuadratic:      1.0, // rate = p², at p=30: rate = 900
		DecayMultiplier:    4.0,
		FullPressureDiff:   200, // diff=200 gives rate=1.0
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

func TestSlowRamp_RespondsToTargetReversal(t *testing.T) {
	config := SlowRampConfig{
		ThresholdSeconds:   30,
		PressureCapSeconds: 60,
		RateLinear:         0.0,
		RateQuadratic:      1.0,
		DecayMultiplier:    4.0,
		FullPressureDiff:   500, // diff=500 gives rate=1.0
	}

	state := SlowRampState{}
	state.Update(500, config) // initialize at 500

	// Build positive pressure past threshold toward 1000
	for range 35 {
		state.Update(1000, config)
	}
	valueAtPeak := state.Current

	// Drop target to 0 - should eventually start moving toward new target
	// (after pressure drains and reverses)
	var finalValue float64
	for range 100 {
		finalValue = state.Update(0, config)
	}

	// Should have moved toward new target (0), not stayed at peak
	assert.Less(t, finalValue, valueAtPeak,
		"Should eventually respond to target reversal")
}

func TestDamping(t *testing.T) {
	// Config with damping=1.0 for easy math, FullPressureDiff=100
	config := SlowRampConfig{
		ThresholdSeconds:   30,
		PressureCapSeconds: 100,
		RateLinear:         0.0,
		RateQuadratic:      1.0,
		DecayMultiplier:    2.0,
		FullPressureDiff:   100,
		Damping:            1.0,
	}

	tests := []struct {
		name         string
		pressure     float64
		diff         float64
		wantPressure float64
	}{
		// Damping pulls pressure toward zero
		{"damping_pos", 10, 0, 9},           // 10 - 1 = 9
		{"damping_neg", -10, 0, -9},         // -10 + 1 = -9
		{"damping_small_pos", 0.5, 0, 0},    // 0.5 < 1.0 → zeroed
		{"damping_small_neg", -0.5, 0, 0},   // -0.5 > -1.0 → zeroed
		{"damping_exact_pos", 1.0, 0, 0},    // exactly at threshold → zeroed
		{"damping_exact_neg", -1.0, 0, 0},

		// Build rate > damping: net positive gain
		// rate = 200/100 = 2.0, then damping subtracts 1.0 → net +1.0
		{"build_minus_damping", 10, 200, 11}, // 10 + 2 - 1 = 11

		// Build rate == damping: no net change
		// rate = 100/100 = 1.0, then damping subtracts 1.0 → net 0
		{"build_equals_damping", 10, 100, 10}, // 10 + 1 - 1 = 10

		// Build rate < damping: net negative (pressure decreases)
		// rate = 50/100 = 0.5, then damping subtracts 1.0 → net -0.5
		{"build_less_than_damping", 10, 50, 9.5}, // 10 + 0.5 - 1 = 9.5

		// From zero with build > damping
		{"from_zero_builds", 0, 200, 1}, // 0 + 2 - 1 = 1

		// From zero with build < damping → stays at zero
		{"from_zero_no_build", 0, 50, 0}, // 0 + 0.5 = 0.5 < 1.0 → zeroed
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := &SlowRampState{Pressure: tt.pressure, initialized: true}
			state.updatePressure(tt.diff, 1.0, config)
			assert.Equal(t, tt.wantPressure, state.Pressure)
		})
	}
}

func TestDampingCreatesDeadZone(t *testing.T) {
	// With FullPressureDiff=100 and Damping=0.5, a diff of 50 exactly balances
	config := SlowRampConfig{
		ThresholdSeconds:   30,
		PressureCapSeconds: 100,
		RateLinear:         0.0,
		RateQuadratic:      1.0,
		DecayMultiplier:    2.0,
		FullPressureDiff:   100,
		Damping:            0.5,
	}

	state := SlowRampState{initialized: true}

	// Apply diff=50 (rate=0.5) for 100 ticks - should stay at zero
	for range 100 {
		state.updatePressure(50, 1.0, config)
	}
	assert.Equal(t, 0.0, state.Pressure, "Diff at damping threshold should not build pressure")

	// Apply diff=100 (rate=1.0) for 10 ticks - should build at net 0.5/tick
	state.Pressure = 0
	for range 10 {
		state.updatePressure(100, 1.0, config)
	}
	assert.Equal(t, 5.0, state.Pressure, "Diff above damping threshold should build pressure")
}

func TestPressureRelease(t *testing.T) {
	// Config with release factor = 0.1 for easy math
	// threshold=30, so at pressure=40 (10 above), release = 10 * 0.1 = 1.0/tick
	config := SlowRampConfig{
		ThresholdSeconds:      30,
		PressureCapSeconds:    60,
		RateLinear:            0.0,
		RateQuadratic:         1.0,
		DecayMultiplier:       2.0,
		FullPressureDiff:      100,
		Damping:               0,                // Disable damping to isolate release behavior
		PressureReleaseFactor: 0.1,
	}

	tests := []struct {
		name         string
		pressure     float64
		diff         float64
		wantPressure float64
	}{
		// Below threshold - no release applied
		{"below_threshold_pos", 20, 0, 20},
		{"below_threshold_neg", -20, 0, -20},
		{"at_threshold_pos", 30, 0, 30},
		{"at_threshold_neg", -30, 0, -30},

		// Above threshold - release pulls toward threshold
		// At pressure=40, excess=10, release=10*0.1=1.0
		{"above_threshold_pos", 40, 0, 39},
		{"above_threshold_neg", -40, 0, -39},

		// Release clamps to threshold (doesn't overshoot)
		// At pressure=32, excess=2, release=0.2, 32-0.2=31.8 (not clamped, above threshold)
		// At pressure=30.5, excess=0.5, release=0.05, 30.5-0.05=30.45 (not clamped, above threshold)
		// Need large enough release to hit clamp: pressure=31, excess=1, release=0.1
		// If release would take us below threshold, we clamp
		// At pressure=30.08, excess=0.08, release=0.008, 30.08-0.008=30.072 (above threshold, no clamp)
		// Test with excess large enough that release hits threshold
		{"release_to_threshold_pos", 31, 0, 30.9}, // 31 - 0.1 = 30.9, no clamp needed
		{"release_to_threshold_neg", -31, 0, -30.9},

		// Release scales with excess
		// At pressure=50, excess=20, release=20*0.1=2.0
		{"release_scales_pos", 50, 0, 48},
		{"release_scales_neg", -50, 0, -48},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := &SlowRampState{Pressure: tt.pressure, initialized: true}
			state.updatePressure(tt.diff, 1.0, config)
			assert.InDelta(t, tt.wantPressure, state.Pressure, 0.001)
		})
	}
}

func TestPressureReleaseEquilibrium(t *testing.T) {
	// Test that pressure stabilizes at equilibrium
	// With FullPressureDiff=100, diff=100 gives buildRate=1.0
	// With PressureReleaseFactor=0.1:
	// Equilibrium: P = threshold + buildRate * (1/factor - 1) = 30 + 1.0 * (10 - 1) = 39
	// (Release happens after build in same tick, so equilibrium is slightly lower than buildRate/factor)
	config := SlowRampConfig{
		ThresholdSeconds:      30,
		PressureCapSeconds:    100,
		RateLinear:            0.0,
		RateQuadratic:         1.0,
		DecayMultiplier:       2.0,
		FullPressureDiff:      100,
		Damping:               0,               // Disable damping to isolate release behavior
		PressureReleaseFactor: 0.1,
	}

	state := SlowRampState{initialized: true}

	// Apply sustained diff until equilibrium
	for range 1000 {
		state.updatePressure(100, 1.0, config)
	}

	// Equilibrium: P = threshold + buildRate * (1/factor - 1) = 30 + 1.0 * 9 = 39
	assert.InDelta(t, 39.0, state.Pressure, 0.1, "Pressure should equilibrate")
}

func TestPressureReleaseWithHighDiff(t *testing.T) {
	// Higher diff = higher build rate = higher equilibrium (capped by PressureCapSeconds)
	config := SlowRampConfig{
		ThresholdSeconds:      30,
		PressureCapSeconds:    50, // Low cap to test capping behavior
		RateLinear:            0.0,
		RateQuadratic:         1.0,
		DecayMultiplier:       2.0,
		FullPressureDiff:      100,
		Damping:               0,
		PressureReleaseFactor: 0.1,
	}

	state := SlowRampState{initialized: true}

	// With diff=500, buildRate=5.0
	// Uncapped equilibrium would be at 30 + 5.0 * 9 = 75
	// But cap is 50, so it hits cap, then release pulls back
	// At cap: release = (50-30) * 0.1 = 2.0
	// Oscillates between 48 (after release) and 50 (after build+cap)
	for range 1000 {
		state.updatePressure(500, 1.0, config)
	}

	assert.InDelta(t, 48.0, state.Pressure, 0.1, "Pressure should stabilize near cap minus release at cap")
}
