package governor

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRollingMinMax_Empty(t *testing.T) {
	r := NewRollingMinMax()
	assert.Equal(t, 0.0, r.Min())
	assert.Equal(t, 0.0, r.Max())
}

func TestRollingMinMax_SingleValue(t *testing.T) {
	r := NewRollingMinMax()
	r.updateAt(100, 0)
	assert.Equal(t, 100.0, r.Min())
	assert.Equal(t, 100.0, r.Max())
}

func TestRollingMinMax_MultipleValuesSameMinute(t *testing.T) {
	r := NewRollingMinMax()
	r.updateAt(100, 0)
	r.updateAt(50, 0)
	r.updateAt(150, 0)
	assert.Equal(t, 50.0, r.Min())
	assert.Equal(t, 150.0, r.Max())
}

func TestRollingMinMax_MultipleMinutes(t *testing.T) {
	r := NewRollingMinMax()
	r.updateAt(100, 0)
	r.updateAt(200, 1)
	r.updateAt(50, 2)
	assert.Equal(t, 50.0, r.Min())
	assert.Equal(t, 200.0, r.Max())
}

func TestRollingMinMax_MissedMinutesClearsOldData(t *testing.T) {
	r := NewRollingMinMax()
	r.updateAt(100, 0)
	r.updateAt(50, 1)
	// Jump to minute 5, skipping 2-4
	r.updateAt(75, 5)
	// Minutes 0,1,5 have data; 2-4 should be cleared
	assert.Equal(t, 50.0, r.Min())  // From minute 1
	assert.Equal(t, 100.0, r.Max()) // From minute 0
}

func TestRollingMinMax_WrapAround(t *testing.T) {
	r := NewRollingMinMax()
	r.updateAt(100, 58)
	r.updateAt(200, 59)
	// Wrap to minute 2, clearing 0,1
	r.updateAt(150, 2)
	assert.Equal(t, 100.0, r.Min())
	assert.Equal(t, 200.0, r.Max())
}

func TestRollingMinMax_SameMinuteUpdatesInPlace(t *testing.T) {
	r := NewRollingMinMax()
	r.updateAt(10, 0)
	r.updateAt(500, 0) // Same minute, updates in place
	assert.Equal(t, 10.0, r.Min())
	assert.Equal(t, 500.0, r.Max())
}
