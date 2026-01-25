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
	DoublePressureDiff float64 // Diff magnitude above which pressure builds 2x faster (e.g., 1000)
	Deadband           float64 // Diff magnitude below which diff is treated as 0 (e.g., 127.5)
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
// Pressure builds when diff pushes away from zero, and drains faster when
// moving back toward zero (controlled by decayMultiplier).
// Deadband has two zones:
//   - Outer half (deadband/2 to deadband): no pressure change (neutral zone)
//   - Inner half (0 to deadband/2): diff treated as zero, pressure drains
// Large diffs (> DoublePressureDiff) build pressure 2x faster.
func (s *SlowRampState) updatePressure(diff, dt float64, config SlowRampConfig) {
	absDiff := math.Abs(diff)

	// Outer half of deadband: no pressure change at all (neutral zone)
	if absDiff > config.Deadband/2 && absDiff <= config.Deadband {
		return
	}

	// Inner half of deadband: treat as zero (pressure drains)
	if absDiff <= config.Deadband/2 {
		diff = 0
	}

	// Base rate, doubled for large diffs
	rate := dt
	if math.Abs(diff) > config.DoublePressureDiff {
		rate *= 2
	}

	// Faster drain when diff and pressure are on opposite sides (moving toward zero)
	if diff*s.Pressure < 0 || diff == 0 {
		rate *= config.DecayMultiplier
	}

	// Update pressure
	switch {
	case diff > 0:
		s.Pressure += rate
	case diff < 0:
		s.Pressure -= rate
	default:
		// Drain toward zero (clamped)
		drainAmount := min(rate, math.Abs(s.Pressure))
		s.Pressure -= math.Copysign(drainAmount, s.Pressure)
	}

	// Cap pressure
	s.Pressure = max(-config.PressureCapSeconds, min(config.PressureCapSeconds, s.Pressure))
}
