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
