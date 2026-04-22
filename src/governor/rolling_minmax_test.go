package governor

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRollingMinMax_Empty(t *testing.T) {
	r := NewRollingMinMax(60)
	assert.Equal(t, 0.0, r.Min())
	assert.Equal(t, 0.0, r.Max())
}

func TestRollingMinMax_SingleValue(t *testing.T) {
	r := NewRollingMinMax(60)
	r.updateAt(100, 0)
	assert.Equal(t, 100.0, r.Min())
	assert.Equal(t, 100.0, r.Max())
}

func TestRollingMinMax_MultipleValuesSameMinute(t *testing.T) {
	r := NewRollingMinMax(60)
	r.updateAt(100, 0)
	r.updateAt(50, 0)
	r.updateAt(150, 0)
	assert.Equal(t, 50.0, r.Min())
	assert.Equal(t, 150.0, r.Max())
}

func TestRollingMinMax_MultipleMinutes(t *testing.T) {
	r := NewRollingMinMax(60)
	r.updateAt(100, 0)
	r.updateAt(200, 1)
	r.updateAt(50, 2)
	assert.Equal(t, 50.0, r.Min())
	assert.Equal(t, 200.0, r.Max())
}

func TestRollingMinMax_MissedMinutesClearsOldData(t *testing.T) {
	r := NewRollingMinMax(60)
	r.updateAt(100, 0)
	r.updateAt(50, 1)
	// Jump to minute 5, skipping 2-4
	r.updateAt(75, 5)
	// Minutes 0,1,5 have data; 2-4 should be cleared
	assert.Equal(t, 50.0, r.Min())  // From minute 1
	assert.Equal(t, 100.0, r.Max()) // From minute 0
}

func TestRollingMinMax_BucketReuseExpiresOldData(t *testing.T) {
	r := NewRollingMinMax(60)
	r.updateAt(100, 58)
	r.updateAt(200, 59)
	// Advance 60 minutes (2 full windows forward). Buckets from minute 58,59
	// are at bucket index 58,59 — now those get overwritten/cleared by
	// minute 118,119 or the missed-minute sweep.
	r.updateAt(150, 120)
	assert.Equal(t, 150.0, r.Min())
	assert.Equal(t, 150.0, r.Max())
}

func TestRollingMinMax_SameMinuteUpdatesInPlace(t *testing.T) {
	r := NewRollingMinMax(60)
	r.updateAt(10, 0)
	r.updateAt(500, 0) // Same minute, updates in place
	assert.Equal(t, 10.0, r.Min())
	assert.Equal(t, 500.0, r.Max())
}

func TestRollingMinMax_ShortWindowExpiry(t *testing.T) {
	// 15-minute window
	r := NewRollingMinMax(15)
	r.updateAt(50, 0)
	r.updateAt(100, 5)
	// At minute 15, bucket 0 (which held 50) is overwritten — 50 falls out of the window
	r.updateAt(75, 15)
	assert.Equal(t, 75.0, r.Min())
	assert.Equal(t, 100.0, r.Max())
}

func TestRollingMinMaxSeconds_BasicResolution(t *testing.T) {
	r := NewRollingMinMaxSeconds(60)
	r.updateAt(10, 0)
	r.updateAt(20, 1)
	r.updateAt(5, 2)
	assert.Equal(t, 5.0, r.Min())
	assert.Equal(t, 20.0, r.Max())
}

func TestRollingMinMaxSeconds_WindowExpiry(t *testing.T) {
	r := NewRollingMinMaxSeconds(10)
	r.updateAt(100, 0)
	r.updateAt(50, 5)
	// Advance past window; tick 0 expires
	r.updateAt(75, 10)
	assert.Equal(t, 50.0, r.Min())
	assert.Equal(t, 75.0, r.Max())
}

func TestRollingMinMaxHours_BasicResolution(t *testing.T) {
	r := NewRollingMinMaxHours(168) // 7-day window
	r.updateAt(1000, 0)
	r.updateAt(500, 1)
	r.updateAt(2000, 2)
	assert.Equal(t, 500.0, r.Min())
	assert.Equal(t, 2000.0, r.Max())
}

func TestRollingMinMaxHours_WindowExpiry(t *testing.T) {
	r := NewRollingMinMaxHours(24)
	r.updateAt(100, 0)
	r.updateAt(200, 12)
	// Advance past 24h window; tick 0 expires
	r.updateAt(150, 24)
	assert.Equal(t, 150.0, r.Min())
	assert.Equal(t, 200.0, r.Max())
}

func TestBucketMinPercentile_Empty(t *testing.T) {
	r := NewRollingMinMaxHours(168)
	assert.Equal(t, 0.0, r.BucketMinPercentile(2))
}

func TestBucketMinPercentile_SingleBucket(t *testing.T) {
	r := NewRollingMinMaxHours(168)
	r.updateAt(500, 0)
	assert.Equal(t, 500.0, r.BucketMinPercentile(2))
	assert.Equal(t, 500.0, r.BucketMinPercentile(50))
	assert.Equal(t, 500.0, r.BucketMinPercentile(98))
}

func TestBucketMinPercentile_PartialFill(t *testing.T) {
	r := NewRollingMinMaxHours(168)
	// Fill 10 of 168 buckets with known values
	for i := int64(0); i < 10; i++ {
		r.updateAt(float64(i+1)*100, i) // 100, 200, ..., 1000
	}
	// P0 should be the minimum (100)
	assert.Equal(t, 100.0, r.BucketMinPercentile(0))
	// P100 should be the maximum (1000)
	assert.Equal(t, 1000.0, r.BucketMinPercentile(100))
	// P50 should be median of [100,200,...,1000] = between 500 and 600
	p50 := r.BucketMinPercentile(50)
	assert.True(t, p50 >= 500 && p50 <= 600, "P50 should be ~550, got %v", p50)
}

func TestBucketMinPercentile_FullFill(t *testing.T) {
	r := NewRollingMinMaxHours(10)
	// Each bucket: min is tick*100, with multiple updates per bucket
	for i := int64(0); i < 10; i++ {
		r.updateAt(float64(i+1)*100, i)
		r.updateAt(float64(i+1)*200, i) // max, shouldn't affect BucketMinPercentile
	}
	// bucket mins: 100, 200, 300, 400, 500, 600, 700, 800, 900, 1000
	assert.Equal(t, 100.0, r.BucketMinPercentile(0))
	assert.Equal(t, 1000.0, r.BucketMinPercentile(100))
	// P2 of 10 values: idx = 0.02*9 = 0.18 → interpolate 100..200 → 118
	assert.InDelta(t, 118.0, r.BucketMinPercentile(2), 1.0)
}

func TestBucketMinPercentile_MultipleValuesPerBucket(t *testing.T) {
	r := NewRollingMinMaxHours(5)
	// Bucket 0: updates 500, 100, 300 → min=100
	r.updateAt(500, 0)
	r.updateAt(100, 0)
	r.updateAt(300, 0)
	// Bucket 1: min=200
	r.updateAt(200, 1)
	r.updateAt(400, 1)
	// Bucket 2: min=50
	r.updateAt(50, 2)
	// mins: [100, 200, 50] → sorted: [50, 100, 200]
	assert.Equal(t, 50.0, r.BucketMinPercentile(0))
	assert.Equal(t, 200.0, r.BucketMinPercentile(100))
}
