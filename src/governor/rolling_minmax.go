package governor

import (
	"math"
	"time"
)

// minMaxBucket holds min/max values for a single minute
type minMaxBucket struct {
	min, max float64
}

// RollingMinMax tracks min/max values over a rolling 1-hour window using 60 1-minute buckets
type RollingMinMax struct {
	buckets       [60]minMaxBucket
	currentMinute int // -1 = uninitialized
}

// NewRollingMinMax creates a new RollingMinMax with all buckets initialized to sentinel values
func NewRollingMinMax() RollingMinMax {
	r := RollingMinMax{currentMinute: -1}
	for i := range r.buckets {
		r.buckets[i] = minMaxBucket{min: math.MaxFloat64, max: -math.MaxFloat64}
	}
	return r
}

// Update records a value at the current time
func (r *RollingMinMax) Update(value float64) {
	r.updateAt(value, time.Now().Minute())
}

// updateAt records a value at the specified minute (for testing)
func (r *RollingMinMax) updateAt(value float64, minute int) {
	if r.currentMinute >= 0 && minute != r.currentMinute {
		// Clear missed buckets (wrap around)
		for i := (r.currentMinute + 1) % 60; i != minute; i = (i + 1) % 60 {
			r.buckets[i] = minMaxBucket{min: math.MaxFloat64, max: -math.MaxFloat64}
		}
	}

	if minute != r.currentMinute {
		// First value for this minute - init directly
		r.buckets[minute] = minMaxBucket{min: value, max: value}
		r.currentMinute = minute
		return
	}

	// Update existing bucket
	b := &r.buckets[minute]
	b.min = min(b.min, value)
	b.max = max(b.max, value)
}

// Min returns the minimum value across all buckets, or 0 if no data
func (r *RollingMinMax) Min() float64 {
	result := math.MaxFloat64
	for _, b := range r.buckets {
		result = min(result, b.min)
	}
	if result == math.MaxFloat64 {
		return 0
	}
	return result
}

// Max returns the maximum value across all buckets, or 0 if no data
func (r *RollingMinMax) Max() float64 {
	result := -math.MaxFloat64
	for _, b := range r.buckets {
		result = max(result, b.max)
	}
	if result == -math.MaxFloat64 {
		return 0
	}
	return result
}
