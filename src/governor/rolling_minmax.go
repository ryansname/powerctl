package governor

import (
	"math"
	"time"
)

// minMaxBucket holds min/max values for a single minute
type minMaxBucket struct {
	min, max float64
}

// RollingMinMax tracks min/max values over a rolling N-minute window using
// N one-minute buckets. Buckets are indexed by (absolute minute % N), so
// advancing past a bucket automatically expires its old data.
type RollingMinMax struct {
	buckets       []minMaxBucket
	currentMinute int64 // absolute minute counter, -1 = uninitialized
}

// NewRollingMinMax creates a new RollingMinMax with a window of the given number of minutes.
func NewRollingMinMax(minutes int) RollingMinMax {
	if minutes <= 0 {
		minutes = 1
	}
	r := RollingMinMax{
		buckets:       make([]minMaxBucket, minutes),
		currentMinute: -1,
	}
	for i := range r.buckets {
		r.buckets[i] = minMaxBucket{min: math.MaxFloat64, max: -math.MaxFloat64}
	}
	return r
}

// Update records a value at the current time
func (r *RollingMinMax) Update(value float64) {
	r.updateAt(value, time.Now().Unix()/60)
}

// updateAt records a value at the specified absolute minute (for testing).
// minute must be non-decreasing across calls; earlier minutes are ignored.
func (r *RollingMinMax) updateAt(value float64, minute int64) {
	n := int64(len(r.buckets))
	if n <= 0 {
		return
	}

	if r.currentMinute >= 0 && minute < r.currentMinute {
		return // time travel guard
	}

	if r.currentMinute >= 0 && minute != r.currentMinute {
		gap := minute - r.currentMinute
		if gap >= n {
			// Entire window is stale — clear every bucket.
			for i := range r.buckets {
				r.buckets[i] = minMaxBucket{min: math.MaxFloat64, max: -math.MaxFloat64}
			}
		} else {
			// Clear skipped buckets between currentMinute (exclusive) and minute (exclusive).
			for i := int64(1); i < gap; i++ {
				idx := int((r.currentMinute + i) % n)
				r.buckets[idx] = minMaxBucket{min: math.MaxFloat64, max: -math.MaxFloat64}
			}
		}
	}

	idx := int(minute % n)
	if minute != r.currentMinute {
		// First value for this minute - init directly (overwrites any stale wrapped data)
		r.buckets[idx] = minMaxBucket{min: value, max: value}
		r.currentMinute = minute
		return
	}

	// Update existing bucket
	b := &r.buckets[idx]
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
