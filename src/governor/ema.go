package governor

import (
	"math"
	"time"
)

// EMA is a time-aware exponential moving average (first-order low-pass filter).
//
// Each Update folds in a sample using alpha = 1 - exp(-dt/tau), where dt is the
// wall-clock interval since the previous Update. Deriving alpha from the actual
// elapsed time (rather than a fixed per-sample weight) keeps the smoothing time
// constant tau honest even when samples arrive irregularly, and makes the result
// independent of how a given interval is subdivided.
//
// The first Update seeds the average to the sample value (no cold-start ramp from
// zero). Value returns 0 before any Update.
type EMA struct {
	tau    time.Duration
	value  float64
	last   time.Time
	inited bool
}

// NewEMA creates an EMA with the given smoothing time constant. A larger tau is
// smoother but laggier. tau <= 0 makes Update a pass-through (no smoothing).
func NewEMA(tau time.Duration) EMA {
	return EMA{tau: tau}
}

// Update folds a sample in at the current time and returns the smoothed value.
func (e *EMA) Update(value float64) float64 {
	return e.updateAt(value, time.Now())
}

// updateAt folds a sample in at an explicit time (for testing). A sample whose
// time is not after the previous one leaves the value unchanged (alpha = 0),
// except in pass-through mode (tau <= 0) where the latest sample always wins.
func (e *EMA) updateAt(value float64, now time.Time) float64 {
	if !e.inited {
		e.value = value
		e.last = now
		e.inited = true
		return e.value
	}
	dt := now.Sub(e.last).Seconds()
	e.last = now
	switch {
	case e.tau <= 0:
		e.value = value
	case dt > 0:
		alpha := 1 - math.Exp(-dt/e.tau.Seconds())
		e.value += alpha * (value - e.value)
	}
	return e.value
}

// Value returns the current smoothed value (0 before the first Update).
func (e *EMA) Value() float64 {
	return e.value
}
