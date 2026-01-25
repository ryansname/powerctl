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
		ThresholdSeconds:   30,
		PressureCapSeconds: 60,
		RateAccel:          1.0,
		DecayMultiplier:    2.0,
		DoublePressureDiff: 1e9, // effectively disabled for tests
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
		ThresholdSeconds:   30,
		PressureCapSeconds: 60,
		RateAccel:          1.0,
		DecayMultiplier:    2.0,
		DoublePressureDiff: 1e9, // effectively disabled for tests
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
		ThresholdSeconds:   30,
		PressureCapSeconds: 60,
		RateAccel:          1.0,
		DecayMultiplier:    2.0,
		DoublePressureDiff: 1e9, // effectively disabled for tests
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
		ThresholdSeconds:   30,
		PressureCapSeconds: 60,
		RateAccel:          1.0,
		DecayMultiplier:    2.0,
		DoublePressureDiff: 1e9, // effectively disabled for tests
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
		ThresholdSeconds:   30,
		PressureCapSeconds: 60,
		RateAccel:          1.0,
		DecayMultiplier:    2.0,
		DoublePressureDiff: 1e9, // effectively disabled for tests
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
		ThresholdSeconds:   30,
		PressureCapSeconds: 60,
		RateAccel:          1.0,
		DecayMultiplier:    2.0,
		DoublePressureDiff: 1e9, // effectively disabled for tests
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
		ThresholdSeconds:   30,
		PressureCapSeconds: 60,
		RateAccel:          1.0,
		DecayMultiplier:    2.0,
		DoublePressureDiff: 1e9, // effectively disabled for tests
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
		ThresholdSeconds:   30,
		PressureCapSeconds: 60,
		RateAccel:          1.0,
		DecayMultiplier:    2.0,
		DoublePressureDiff: 1e9, // effectively disabled for tests
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

func TestSlowRamp_DoesNotRampAwayFromTarget(t *testing.T) {
	state := SlowRampState{}
	config := SlowRampConfig{
		ThresholdSeconds:   30,
		PressureCapSeconds: 60,
		RateAccel:          1.0,
		DecayMultiplier:    2.0,
		DoublePressureDiff: 1e9, // effectively disabled for tests
	}

	// Initialize at 500
	state.Update(500, config)
	assert.Equal(t, 500.0, state.Current)

	// Build positive pressure past threshold with target above current
	for range 35 {
		state.Update(1000, config)
	}
	assert.Greater(t, state.Pressure, 30.0, "Pressure should exceed threshold")
	assert.Greater(t, state.Current, 500.0, "Should have started ramping up")
	currentBeforeDrop := state.Current

	// Now drop target BELOW current - pressure is still positive
	// Current should NOT decrease (would be ramping away from target)
	for range 10 {
		result := state.Update(0, config)
		assert.GreaterOrEqual(t, result, currentBeforeDrop,
			"Current should not decrease when target drops below it (positive pressure)")
	}

	// Pressure should be draining (diff and pressure have opposite signs)
	assert.Less(t, state.Pressure, 35.0, "Pressure should be draining")
}

func TestSlowRamp_DeadbandIgnoresSmallDiffs(t *testing.T) {
	state := SlowRampState{}
	config := SlowRampConfig{
		ThresholdSeconds:   30,
		PressureCapSeconds: 60,
		RateAccel:          1.0,
		DecayMultiplier:    2.0,
		DoublePressureDiff: 1e9,
		Deadband:           100, // 100W deadband: inner 0-50, outer 50-100
	}

	// Initialize at 500W
	state.Update(500, config)
	assert.Equal(t, 500.0, state.Current)
	assert.Equal(t, 0.0, state.Pressure)

	// Target in outer half of deadband (diff = 75, which is > 50 and <= 100)
	// Should not change pressure at all (neutral zone)
	for range 60 {
		state.Update(575, config)
	}
	assert.Equal(t, 500.0, state.Current, "Should not move when diff in outer deadband")
	assert.Equal(t, 0.0, state.Pressure, "Should not change pressure in outer deadband")

	// Target outside deadband (500 + 150 = 650, diff = 150 > 100)
	// Should build pressure
	for range 35 {
		state.Update(650, config)
	}
	assert.Greater(t, state.Pressure, 30.0, "Should build pressure when diff exceeds deadband")
	assert.Greater(t, state.Current, 500.0, "Should start ramping when pressure exceeds threshold")
}

func TestSlowRamp_DeadbandDrainsPressure(t *testing.T) {
	state := SlowRampState{}
	config := SlowRampConfig{
		ThresholdSeconds:   30,
		PressureCapSeconds: 60,
		RateAccel:          1.0,
		DecayMultiplier:    2.0,
		DoublePressureDiff: 1e9,
		Deadband:           100, // inner half: 0-50, outer half: 50-100
	}

	// Initialize at 0
	state.Update(0, config)

	// Build pressure with large diff (outside deadband)
	for range 20 {
		state.Update(500, config)
	}
	assert.Equal(t, 20.0, state.Pressure, "Should have built 20s of pressure")

	// Now target in inner half of deadband (diff = 25, which is <= 50)
	// Diff is treated as 0, so pressure should drain at decay rate
	for range 5 {
		state.Update(25, config)
	}
	// 5 seconds at 2x decay = 10s drained: 20 - 10 = 10
	assert.Equal(t, 10.0, state.Pressure, "Pressure should drain when diff in inner deadband")
}

func TestSlowRamp_OuterDeadbandPreservesPressure(t *testing.T) {
	state := SlowRampState{}
	config := SlowRampConfig{
		ThresholdSeconds:   30,
		PressureCapSeconds: 60,
		RateAccel:          1.0,
		DecayMultiplier:    2.0,
		DoublePressureDiff: 1e9,
		Deadband:           100, // inner half: 0-50, outer half: 50-100
	}

	// Initialize at 0
	state.Update(0, config)

	// Build pressure with large diff (outside deadband)
	for range 20 {
		state.Update(500, config)
	}
	assert.Equal(t, 20.0, state.Pressure, "Should have built 20s of pressure")

	// Now target in outer half of deadband (diff = 75, which is > 50 and <= 100)
	// Pressure should NOT change (neutral zone)
	for range 10 {
		state.Update(75, config)
	}
	assert.Equal(t, 20.0, state.Pressure, "Pressure should be preserved in outer deadband")
}

func TestSlowRamp_CoastsWhenEnteringDeadband(t *testing.T) {
	// Test behavior when ramping value enters deadband with pressure above threshold:
	// Should continue "coasting" toward target while pressure drains.
	// Use small RateAccel so maxRate << diff, ensuring multiple coasting ticks.
	state := SlowRampState{}
	config := SlowRampConfig{
		ThresholdSeconds:   30,
		PressureCapSeconds: 120,
		RateAccel:          0.1, // Small accel: at 30s past threshold, maxRate = 0.1 * 30² = 90 W/s
		DecayMultiplier:    2.0,
		DoublePressureDiff: 1e9,
		Deadband:           500, // Large deadband
	}

	// Initialize at 0
	state.Update(0, config)

	// Build pressure to 60s (30s past threshold) targeting 10000W
	for range 60 {
		state.Update(10000, config)
	}
	assert.Equal(t, 60.0, state.Pressure)
	currentAfterBuildup := state.Current
	assert.Greater(t, currentAfterBuildup, 0.0, "Should have started ramping")

	// Now set target within deadband of current
	// diff = 200 < 500 deadband, but maxRate at 30s progress = 90 W/s
	// So it takes multiple ticks to coast to target
	targetInDeadband := currentAfterBuildup + 200

	currentBefore := state.Current
	pressureBefore := state.Pressure

	// First tick: should coast (ramp toward target) while pressure drains
	state.Update(targetInDeadband, config)
	assert.Greater(t, state.Current, currentBefore, "Should coast toward target")
	assert.Less(t, state.Current, targetInDeadband, "Should not reach target in one tick (maxRate < diff)")
	assert.Less(t, state.Pressure, pressureBefore, "Pressure should drain")

	// Continue coasting until reaching target
	coastTicks := 1
	for state.Current < targetInDeadband && coastTicks < 10 {
		state.Update(targetInDeadband, config)
		coastTicks++
	}

	// Should have taken multiple ticks to coast (not instant snap)
	assert.Greater(t, coastTicks, 1, "Should coast over multiple ticks")
	assert.Equal(t, targetInDeadband, state.Current, "Should eventually reach target")

	// Pressure should still be above threshold (drains at 2s/tick, started at 60, 3 ticks = 54)
	assert.Greater(t, state.Pressure, config.ThresholdSeconds, "Pressure should still be above threshold after coasting")

	// After reaching target, diff=0, so no more movement even with pressure above threshold
	finalCurrent := state.Current
	for range 10 {
		state.Update(targetInDeadband, config)
	}
	assert.Equal(t, finalCurrent, state.Current, "Should stop at target (diff=0)")
}
