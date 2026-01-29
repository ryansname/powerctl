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
	ThresholdSeconds      float64 // Pressure magnitude required before responding (e.g., 600)
	PressureCapSeconds    float64 // Maximum pressure magnitude (e.g., 660)
	RateAccel             float64 // Acceleration of ramp rate in units/s² (e.g., 0.02778)
	DecayMultiplier       float64 // How much faster pressure drains vs builds (e.g., 2.0)
	FullPressureDiff      float64 // Diff magnitude at which pressure builds at 1x rate; rate scales linearly (2x at 2*FullPressureDiff, etc.)
	Damping               float64 // Pressure pulled toward zero by this amount per second (e.g., 0.5)
	PressureReleaseFactor float64 // Release rate per second per unit of pressure above threshold (e.g., 0.05)
}

// DefaultSlowRampConfig returns the default configuration for power smoothing.
// With threshold=600s (10min), pressure cap=660s (11min), progressSeconds at cap=60s.
// RateAccel chosen so maxRate = 100 W/s at cap: 100 / 60² = 0.02778
// PressureReleaseFactor creates equilibrium where buildRate = releaseRate above threshold.
func DefaultSlowRampConfig() SlowRampConfig {
	return SlowRampConfig{
		ThresholdSeconds:      600.0,
		PressureCapSeconds:    660.0,
		RateAccel:             100.0 / (60.0 * 60.0), // 100 W/s max at pressure cap
		DecayMultiplier:       2.0,
		Damping:               0.5,
		PressureReleaseFactor: 0.05,
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
// Both building and draining rates scale linearly with diff: rate = diff / FullPressureDiff
// Draining is additionally multiplied by DecayMultiplier for faster response to direction changes.
//nolint:unparam // dt kept for flexibility even though currently always 1.0
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

	if config.FullPressureDiff > 0 {
		rate := mDiff / config.FullPressureDiff
		if mPressure < 0 {
			// Wrong direction - drain toward zero at lerped rate × DecayMultiplier
			mPressure = min(0, mPressure+dt*rate*config.DecayMultiplier)
		} else {
			// Correct direction - build at lerped rate
			mPressure += dt * rate
		}
	}

	// Denormalize and cap
	s.Pressure = sign * mPressure
	s.Pressure = max(-config.PressureCapSeconds, min(config.PressureCapSeconds, s.Pressure))

	// Apply pressure release when above threshold
	// Release rate scales linearly with excess pressure, creating natural equilibrium
	if config.PressureReleaseFactor > 0 && math.Abs(s.Pressure) > config.ThresholdSeconds {
		excess := math.Abs(s.Pressure) - config.ThresholdSeconds
		release := excess * config.PressureReleaseFactor * dt
		if s.Pressure > 0 {
			s.Pressure = max(config.ThresholdSeconds, s.Pressure-release)
		} else {
			s.Pressure = min(-config.ThresholdSeconds, s.Pressure+release)
		}
	}

	// Apply damping - pull pressure toward zero
	// This creates a dead zone where small diffs can't build pressure
	damping := config.Damping * dt
	switch {
	case s.Pressure > damping:
		s.Pressure -= damping
	case s.Pressure < -damping:
		s.Pressure += damping
	default:
		s.Pressure = 0
	}
}
