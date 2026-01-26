// Package governor provides power governing algorithms for smoothing and rate limiting.
package governor

import "math"

// SlowRampState tracks state for the Pressure-Gated Accelerating Ramp smoother.
// This smoother ignores brief fluctuations and only responds to sustained changes,
// with a slow initial response that accelerates over time.
type SlowRampState struct {
	Current     float64 // Current smoothed output value
	Pressure    float64 // Signed accumulator (positive = target above current, negative = below)
	initialized bool
}

// SlowRampConfig holds tunable parameters for the slow ramp smoother.
type SlowRampConfig struct {
	ThresholdSeconds   float64 // Pressure magnitude required before responding (e.g., 600)
	PressureCapSeconds float64 // Maximum pressure magnitude (e.g., 900)
	RateAccel          float64 // Acceleration of ramp rate in units/s² (e.g., 0.00111)
	DecayMultiplier    float64 // How much faster pressure drains vs builds (e.g., 4.0)
	FullPressureDiff   float64 // Diff magnitude at which pressure builds at full (1x) rate; below this, rate is lerped from 0
	DoublePressureDiff float64 // Diff magnitude above which pressure builds 2x faster (e.g., 1000)
}

// DefaultSlowRampConfig returns the default configuration for power smoothing.
// With threshold=600s (10min), pressure cap=900s (15min), progressSeconds at cap=300s.
// RateAccel chosen so maxRate = 100 W/s at cap: 100 / 300² = 0.00111
func DefaultSlowRampConfig() SlowRampConfig {
	return SlowRampConfig{
		ThresholdSeconds:   600.0,
		PressureCapSeconds: 900.0,
		RateAccel:          100.0 / (300.0 * 300.0), // 100 W/s max at pressure cap
		DecayMultiplier:    4.0,
		DoublePressureDiff: 1000.0, // 1kW
	}
}

// Update calculates the next smoothed value using the Pressure-Gated Accelerating Ramp algorithm.
// The algorithm:
// 1. Accumulates signed "pressure" based on how long target differs from current
// 2. Only starts ramping when |pressure| exceeds ThresholdSeconds
// 3. Ramp rate accelerates quadratically: rate = RateAccel * progressSeconds²
// 4. Step is capped at remaining difference to prevent overshoot
// 5. Pressure drains faster than it builds (hysteresis) to reject oscillations
func (s *SlowRampState) Update(target float64, config SlowRampConfig) float64 {
	const dt = 1.0

	if !s.initialized {
		s.Current = target
		s.initialized = true
		return s.Current
	}

	diff := target - s.Current

	// Update pressure with hysteresis
	s.updatePressure(diff, dt, config)

	// Only ramp when pressure magnitude exceeds threshold AND pressure/diff agree on direction
	// This prevents ramping away from target when target reverses
	absPressure := math.Abs(s.Pressure)
	if absPressure > config.ThresholdSeconds && diff*s.Pressure > 0 {
		// Quadratic acceleration: slow start, speeds up over time
		progressSeconds := absPressure - config.ThresholdSeconds
		maxRate := config.RateAccel * progressSeconds * progressSeconds

		// Move toward target, capped by max rate (prevents overshoot)
		step := diff
		if math.Abs(step) > maxRate {
			step = math.Copysign(maxRate, diff)
		}
		s.Current += step
	}

	return s.Current
}

// updatePressure updates the pressure accumulator with hysteresis.
// Uses normalization to reason about only one direction (positive diff).
// Pressure building rate scales with diff magnitude:
//   - Below FullPressureDiff: rate lerps from 0 (at diff=0) to 1 (at diff=FullPressureDiff)
//   - Above FullPressureDiff: rate = 1x
//   - Above DoublePressureDiff: rate = 2x
func (s *SlowRampState) updatePressure(diff, dt float64, config SlowRampConfig) {
	// Normalize: flip signs so diff is always positive
	// This lets us reason about only one direction
	sign := 1.0
	mDiff, mPressure := diff, s.Pressure
	if diff < 0 {
		sign = -1.0
		mDiff = -diff
		mPressure = -s.Pressure
	}

	// After normalization:
	// - mDiff >= 0 (always positive)
	// - mPressure > 0 means pressure in correct direction (building)
	// - mPressure < 0 means pressure in wrong direction (needs draining)

	switch {
	case mPressure < 0:
		// Wrong direction - drain toward zero (add because negative)
		mPressure = min(0, mPressure+dt*config.DecayMultiplier)

	case mDiff > config.DoublePressureDiff:
		// Large diff - build at 2x rate
		mPressure += dt * 2

	case mDiff >= config.FullPressureDiff:
		// Normal - build at 1x rate
		mPressure += dt

	case config.FullPressureDiff > 0:
		// Below FullPressureDiff - build at lerped rate (0 at diff=0, 1 at diff=FullPressureDiff)
		rate := mDiff / config.FullPressureDiff
		mPressure += dt * rate

	default:
		// FullPressureDiff disabled (0) and diff is 0 - no change
	}

	// Denormalize and cap
	s.Pressure = sign * mPressure
	s.Pressure = max(-config.PressureCapSeconds, min(config.PressureCapSeconds, s.Pressure))
}
