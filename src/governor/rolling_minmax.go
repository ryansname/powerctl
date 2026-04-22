package governor

import (
	"math"
	"sort"
	"time"
)

// minMaxBucket holds min/max values for a single bucket interval
type minMaxBucket struct {
	min, max float64
}

// RollingMinMax tracks min/max values over a rolling N-bucket window.
// Buckets are indexed by (absolute tick % N), so advancing past a bucket
// automatically expires its old data.
type RollingMinMax struct {
	buckets       []minMaxBucket
	currentTick   int64 // absolute tick counter (Unix seconds / bucketDivisor)
	bucketDivisor int64 // seconds per bucket (60 = minutes, 3600 = hours, 1 = seconds)
}

// NewRollingMinMax creates a RollingMinMax with 1-minute buckets and a window of the given number of minutes.
func NewRollingMinMax(minutes int) RollingMinMax {
	return newRollingMinMax(minutes, 60)
}

// NewRollingMinMaxSeconds creates a RollingMinMax with 1-second buckets and a window of the given number of seconds.
func NewRollingMinMaxSeconds(seconds int) RollingMinMax {
	return newRollingMinMax(seconds, 1)
}

// NewRollingMinMaxHours creates a RollingMinMax with 1-hour buckets and a window of the given number of hours.
func NewRollingMinMaxHours(hours int) RollingMinMax {
	return newRollingMinMax(hours, 3600)
}

func newRollingMinMax(numBuckets int, bucketDivisor int64) RollingMinMax {
	if numBuckets <= 0 {
		numBuckets = 1
	}
	r := RollingMinMax{
		buckets:       make([]minMaxBucket, numBuckets),
		currentTick:   -1,
		bucketDivisor: bucketDivisor,
	}
	for i := range r.buckets {
		r.buckets[i] = minMaxBucket{min: math.MaxFloat64, max: -math.MaxFloat64}
	}
	return r
}

// Update records a value at the current time
func (r *RollingMinMax) Update(value float64) {
	r.updateAt(value, time.Now().Unix()/r.bucketDivisor)
}

// updateAt records a value at the specified absolute tick (for testing).
// tick must be non-decreasing across calls; earlier ticks are ignored.
func (r *RollingMinMax) updateAt(value float64, tick int64) {
	if r.currentTick == -1 {
		r.currentTick = tick
	}
	n := int64(len(r.buckets))
	if n <= 0 {
		return
	}

	for ; r.currentTick < tick; r.currentTick++ {
		idx := int((r.currentTick + 1) % n)
		r.buckets[idx] = minMaxBucket{min: math.MaxFloat64, max: -math.MaxFloat64}
	}

	idx := int((r.currentTick) % n)
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

// BucketMinPercentile returns the p-th percentile of per-bucket minimums.
// Returns 0 when no valid buckets exist.
func (r *RollingMinMax) BucketMinPercentile(p int) float64 {
	var mins []float64
	for _, b := range r.buckets {
		if b.min != math.MaxFloat64 {
			mins = append(mins, b.min)
		}
	}
	if len(mins) == 0 {
		return 0
	}
	sort.Float64s(mins)
	if p <= 0 {
		return mins[0]
	}
	if p >= 100 {
		return mins[len(mins)-1]
	}
	idx := float64(p) / 100.0 * float64(len(mins)-1)
	lo := int(idx)
	hi := lo + 1
	if hi >= len(mins) {
		return mins[lo]
	}
	frac := idx - float64(lo)
	return mins[lo] + frac*(mins[hi]-mins[lo])
}
