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
	ThresholdSeconds float64 // Pressure magnitude required before responding (e.g., 30)
	RateAccel        float64 // Acceleration of ramp rate in units/s² (e.g., 1.0)
	DecayMultiplier  float64 // How much faster pressure drains vs builds (e.g., 2.0)
}

// DefaultSlowRampConfig returns the default configuration for power smoothing.
// With threshold=120s, pressure cap=240s, progressSeconds at cap=120s.
// RateAccel chosen so maxRate = 100 W/s at cap: 100 / 120² = 0.00694
func DefaultSlowRampConfig() SlowRampConfig {
	return SlowRampConfig{
		ThresholdSeconds: 120.0,
		RateAccel:        100.0 / (120.0 * 120.0), // 100 W/s max at pressure cap
		DecayMultiplier:  2.0,
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
	s.updatePressure(diff, dt, config.DecayMultiplier, config.ThresholdSeconds)

	// Only ramp when pressure magnitude exceeds threshold
	absPressure := math.Abs(s.Pressure)
	if absPressure > config.ThresholdSeconds {
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
func (s *SlowRampState) updatePressure(diff, dt, decayMultiplier, thresholdSeconds float64) {
	// Faster rate when diff and pressure are on opposite sides (moving toward zero)
	rate := dt
	if diff*s.Pressure < 0 || diff == 0 {
		rate *= decayMultiplier
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

	// Cap pressure at 2x threshold (limits max ramp rate)
	maxPressure := thresholdSeconds * 2
	s.Pressure = max(-maxPressure, min(maxPressure, s.Pressure))
}
