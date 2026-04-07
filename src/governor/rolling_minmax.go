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
	currentMinute int64 // absolute minute counter
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
	if r.currentMinute == -1 {
		r.currentMinute = minute
	}
	n := int64(len(r.buckets))
	if n <= 0 {
		return
	}

	for ; r.currentMinute < minute; r.currentMinute++ {
		idx := int((r.currentMinute + 1) % n)
		r.buckets[idx] = minMaxBucket{min: math.MaxFloat64, max: -math.MaxFloat64}
	}

	// Update existing bucket
	idx := int((r.currentMinute) % n)
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
