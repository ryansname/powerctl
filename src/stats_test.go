package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestCalculateTimeWeightedStats_Empty(t *testing.T) {
	readings := Readings{}
	now := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	avg, min, max := calculateTimeWeightedStats(readings, 1*time.Minute, now)

	assert.Equal(t, 0.0, avg)
	assert.Equal(t, 0.0, min)
	assert.Equal(t, 0.0, max)
}

func TestCalculateTimeWeightedStats_SingleReading(t *testing.T) {
	now := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	readings := Readings{
		{Value: 100.0, Timestamp: now.Add(-30 * time.Second)},
	}
	avg, min, max := calculateTimeWeightedStats(readings, 1*time.Minute, now)

	assert.Equal(t, 100.0, avg)
	assert.Equal(t, 100.0, min)
	assert.Equal(t, 100.0, max)
}

func TestCalculateTimeWeightedStats_MultipleReadings(t *testing.T) {
	now := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	readings := Readings{
		{Value: 100.0, Timestamp: now.Add(-40 * time.Second)},
		{Value: 200.0, Timestamp: now.Add(-20 * time.Second)},
	}
	avg, min, max := calculateTimeWeightedStats(readings, 1*time.Minute, now)

	// First reading active for 20s (100), second for 20s (200)
	// Time-weighted average = (100*20 + 200*20) / 40 = 150
	assert.Equal(t, 150.0, avg)
	assert.Equal(t, 100.0, min)
	assert.Equal(t, 200.0, max)
}

func TestCalculateTimeWeightedStats_OldReadingsUseLastKnown(t *testing.T) {
	now := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	// All readings are older than the window - should use last known value
	readings := Readings{
		{Value: 50.0, Timestamp: now.Add(-5 * time.Minute)},
		{Value: 75.0, Timestamp: now.Add(-3 * time.Minute)},
	}
	avg, min, max := calculateTimeWeightedStats(readings, 1*time.Minute, now)

	// Should return the last known value since no readings in window
	assert.Equal(t, 75.0, avg)
	assert.Equal(t, 75.0, min)
	assert.Equal(t, 75.0, max)
}

func TestCalculateTimeWeightedStats_TimeWeighting(t *testing.T) {
	now := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	// First value held for 10s, second value held for 49s
	// Note: readings at exactly -60s are excluded (not strictly after cutoff)
	readings := Readings{
		{Value: 100.0, Timestamp: now.Add(-59 * time.Second)},
		{Value: 200.0, Timestamp: now.Add(-49 * time.Second)},
	}
	avg, _, _ := calculateTimeWeightedStats(readings, 1*time.Minute, now)

	// 100 * 10s + 200 * 49s = 1000 + 9800 = 10800
	// 10800 / 59s = 183.05
	expected := 10800.0 / 59.0
	assert.Equal(t, expected, avg)
}

func TestCalculateStats_UpdatesAllWindows(t *testing.T) {
	now := time.Now()
	readings := Readings{
		{Value: 100.0, Timestamp: now.Add(-30 * time.Second)},
	}

	data := &FloatTopicData{}
	calculateStats(data, readings)

	// All windows should have the same value for a single recent reading
	assert.Equal(t, 100.0, data.Average._1)
	assert.Equal(t, 100.0, data.Average._5)
	assert.Equal(t, 100.0, data.Average._15)
}

func TestCalculateTimeWeightedStats_MillisecondDurations(t *testing.T) {
	now := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	// Test with sub-second (millisecond) durations
	readings := Readings{
		{Value: 100.0, Timestamp: now.Add(-500 * time.Millisecond)},
		{Value: 200.0, Timestamp: now.Add(-250 * time.Millisecond)},
	}
	avg, min, max := calculateTimeWeightedStats(readings, 1*time.Second, now)

	// First reading active for 250ms (100), second for 250ms (200)
	// Time-weighted average = (100*0.25 + 200*0.25) / 0.5 = 75 / 0.5 = 150
	assert.Equal(t, 150.0, avg)
	assert.Equal(t, 100.0, min)
	assert.Equal(t, 200.0, max)
}

func TestCalculateTimeWeightedStats_ShortSpike(t *testing.T) {
	now := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	// Simulate a short 100ms spike in the middle of stable readings
	readings := Readings{
		{Value: 100.0, Timestamp: now.Add(-500 * time.Millisecond)},
		{Value: 500.0, Timestamp: now.Add(-300 * time.Millisecond)}, // spike
		{Value: 100.0, Timestamp: now.Add(-200 * time.Millisecond)},
	}
	avg, min, max := calculateTimeWeightedStats(readings, 1*time.Second, now)

	// 100 for 200ms, 500 for 100ms, 100 for 200ms
	// (100*0.2 + 500*0.1 + 100*0.2) / 0.5 = (20 + 50 + 20) / 0.5 = 180
	assert.Equal(t, 180.0, avg)
	assert.Equal(t, 100.0, min)
	assert.Equal(t, 500.0, max)
}

func TestCalculateTimeWeightedStats_ZeroDuration(t *testing.T) {
	now := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	// Simulate startup: single reading with timestamp exactly equal to now
	readings := Readings{
		{Value: 100.0, Timestamp: now},
	}
	avg, min, max := calculateTimeWeightedStats(readings, 1*time.Minute, now)

	// Even with zero duration, should return the value not zero
	assert.Equal(t, 100.0, avg)
	assert.Equal(t, 100.0, min)
	assert.Equal(t, 100.0, max)
}
