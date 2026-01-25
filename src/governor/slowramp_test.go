package governor

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSlowRamp_InitializesToTarget(t *testing.T) {
	state := SlowRampState{}
	config := DefaultSlowRampConfig()

	result := state.Update(1000, config)

	assert.Equal(t, 1000.0, result)
	assert.Equal(t, 1000.0, state.Current)
	assert.Equal(t, 0.0, state.Pressure)
}

func TestSlowRamp_IgnoresOscillation(t *testing.T) {
	state := SlowRampState{}
	config := SlowRampConfig{
		ThresholdSeconds: 30,
		RateAccel:        1.0,
		DecayMultiplier:  2.0,
	}

	// Initialize at 1000W
	state.Update(1000, config)

	// Oscillate +500W/-500W every 20 seconds (never crosses 30s threshold)
	for range 3 {
		// 20 seconds at +500W
		for range 20 {
			result := state.Update(1500, config)
			assert.InDelta(t, 1000, result, 1, "Should not respond to oscillation (up phase)")
		}
		// 20 seconds at -500W
		for range 20 {
			result := state.Update(500, config)
			assert.InDelta(t, 1000, result, 1, "Should not respond to oscillation (down phase)")
		}
	}
}

func TestSlowRamp_RespondsToSustained(t *testing.T) {
	state := SlowRampState{}
	config := SlowRampConfig{
		ThresholdSeconds: 30,
		RateAccel:        1.0,
		DecayMultiplier:  2.0,
	}

	// Initialize at 0
	state.Update(0, config)

	// Sustain at 10000W for 45 seconds (15s past threshold)
	var lastResult float64
	for range 45 {
		lastResult = state.Update(10000, config)
	}

	// Should have started ramping after 30 seconds, but with large gap shouldn't be done
	assert.Greater(t, lastResult, 100.0, "Should have started ramping toward target")
	assert.Less(t, lastResult, 5000.0, "Should not be close to target yet")
}

func TestSlowRamp_AcceleratesOverTime(t *testing.T) {
	state := SlowRampState{}
	config := SlowRampConfig{
		ThresholdSeconds: 30,
		RateAccel:        1.0,
		DecayMultiplier:  2.0,
	}

	// Initialize at 0
	state.Update(0, config)

	// Track current values over 90 seconds toward target of 10000
	values := make([]float64, 0, 90)
	for range 90 {
		result := state.Update(10000, config)
		values = append(values, result)
	}

	// First 30 seconds: no movement (below threshold)
	for i := range 30 {
		assert.Equal(t, 0.0, values[i], "Should not move before threshold at t=%d", i)
	}

	// After threshold: deltas should increase (acceleration)
	// Compare deltas at t=35 vs t=55
	if len(values) > 55 {
		earlyDelta := values[35] - values[34]
		lateDelta := values[55] - values[54]
		assert.Greater(t, lateDelta, earlyDelta, "Should accelerate over time")
	}
}

func TestSlowRamp_NeverOvershoots(t *testing.T) {
	state := SlowRampState{}
	config := SlowRampConfig{
		ThresholdSeconds: 30,
		RateAccel:        1.0,
		DecayMultiplier:  2.0,
	}

	// Initialize at 0
	state.Update(0, config)

	target := 100.0

	// Run for 200 seconds - more than enough to reach target
	for i := range 200 {
		result := state.Update(target, config)
		assert.LessOrEqual(t, result, target, "Should never overshoot target at t=%d", i)
	}

	// Should have reached target
	assert.InDelta(t, target, state.Current, 0.1, "Should reach target")
}

func TestSlowRamp_DrainsThenBuilds(t *testing.T) {
	state := SlowRampState{}
	config := SlowRampConfig{
		ThresholdSeconds: 30,
		RateAccel:        1.0,
		DecayMultiplier:  2.0,
	}

	// Initialize at 0
	state.Update(0, config)

	// Build positive pressure for 25 seconds (below threshold, so current doesn't move)
	for range 25 {
		state.Update(100000, config)
	}
	assert.Equal(t, 25.0, state.Pressure, "Should have built 25s of positive pressure")

	// Now target goes negative - pressure should drain at 2x rate
	for range 10 {
		state.Update(-100000, config)
	}

	// With decayMultiplier=2, 10 seconds should drain 20s of pressure: 25 - 20 = 5
	assert.Equal(t, 5.0, state.Pressure, "Pressure should drain at 2x rate")

	// Continue draining and building negative pressure
	for range 10 {
		state.Update(-100000, config)
	}

	// 5 more seconds at 2x = 10 drained to reach 0, then:
	// - At pressure=0: still 2x rate (diff*pressure = 0)
	// - After crossing zero: diff < 0, pressure < 0, same signs = 1x rate
	// 2.5s at 2x to drain 5, then ~7.5s at 1x building negative = -7.5
	// Actually: 2.5s at 2x to reach 0, 1 more tick at 2x to go to -2, then 7s at 1x = -9
	assert.Less(t, state.Pressure, 0.0, "Pressure should be negative after draining")
	assert.Greater(t, state.Pressure, -15.0, "Pressure should not be too negative")
}

func TestSlowRamp_HysteresisDecaysFaster(t *testing.T) {
	state := SlowRampState{}
	config := SlowRampConfig{
		ThresholdSeconds: 30,
		RateAccel:        1.0,
		DecayMultiplier:  2.0,
	}

	// Initialize at 0
	state.Update(0, config)

	// Build pressure for 20 seconds (below threshold, so current stays at 0)
	for range 20 {
		state.Update(1e9, config)
	}
	assert.Equal(t, 20.0, state.Pressure)

	// Return to baseline (diff = 0, current = 0) - should drain at 2x rate
	for range 5 {
		state.Update(0, config)
	}

	// 5 seconds at 2x decay = 10s drained
	assert.Equal(t, 10.0, state.Pressure, "Should drain at 2x rate when diff=0")
}

func TestSlowRamp_PressureCappedAt2xThreshold(t *testing.T) {
	state := SlowRampState{}
	config := SlowRampConfig{
		ThresholdSeconds: 30,
		RateAccel:        1.0,
		DecayMultiplier:  2.0,
	}

	// Initialize
	state.Update(0, config)

	// Build pressure for 100 seconds using a huge target so current never catches up
	// (even at max rate of 900 W/s, 100 * 900 = 90000W, which is < 1e9)
	for range 100 {
		state.Update(1e9, config)
	}

	// Should be capped at 2 * 30 = 60
	assert.Equal(t, 60.0, state.Pressure, "Pressure should be capped at 2x threshold")
}

func TestSlowRamp_MaxRateAt2xThreshold(t *testing.T) {
	state := SlowRampState{}
	config := SlowRampConfig{
		ThresholdSeconds: 30,
		RateAccel:        1.0,
		DecayMultiplier:  2.0,
	}

	// Initialize at 0
	state.Update(0, config)

	// Build to max pressure (60s) with large target
	for range 100 {
		state.Update(1e9, config)
	}

	// At max pressure (60), progressSeconds = 60 - 30 = 30
	// maxRate = 1.0 * 30^2 = 900 W/s
	// Record the delta at max pressure
	before := state.Current
	state.Update(1e9, config)
	delta := state.Current - before

	assert.InDelta(t, 900.0, delta, 1.0, "Max rate should be 900 W/s at pressure cap")
}
