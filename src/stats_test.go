package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestPrepareWindowData_Empty(t *testing.T) {
	readings := Readings{}
	now := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	pairs, totalDuration, fallback := prepareWindowData(readings, 1*time.Minute, now)

	assert.Nil(t, pairs)
	assert.Equal(t, 0.0, totalDuration)
	assert.Equal(t, 0.0, fallback)
}

func TestPrepareWindowData_SingleReading(t *testing.T) {
	now := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	readings := Readings{
		{Value: 100.0, Timestamp: now.Add(-30 * time.Second)},
	}
	pairs, _, fallback := prepareWindowData(readings, 1*time.Minute, now)

	// Single reading returns nil pairs (uses fallback)
	assert.Nil(t, pairs)
	assert.Equal(t, 100.0, fallback)
}

func TestCalculateSelectedPercentile_FromPreparedData(t *testing.T) {
	now := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	readings := Readings{
		{Value: 100.0, Timestamp: now.Add(-40 * time.Second)},
		{Value: 200.0, Timestamp: now.Add(-20 * time.Second)},
	}
	pairs, totalDuration, fallback := prepareWindowData(readings, 1*time.Minute, now)

	// First reading active for 20s (100), second for 20s (200)
	// Total 40s. Sorted: 100 (20s), 200 (20s)
	p1 := calculateSelectedPercentile(pairs, totalDuration, 1, fallback)
	p50 := calculateSelectedPercentile(pairs, totalDuration, 50, fallback)
	p66 := calculateSelectedPercentile(pairs, totalDuration, 66, fallback)
	p99 := calculateSelectedPercentile(pairs, totalDuration, 99, fallback)

	// P50 target = 20s, cumulative after 100 = 20s, so P50 = 100
	// P66 target = 26.4s, cumulative after 100 = 20s, after 200 = 40s, so P66 = 200
	assert.Equal(t, 100.0, p1)
	assert.Equal(t, 100.0, p50)
	assert.Equal(t, 200.0, p66)
	assert.Equal(t, 200.0, p99)
}

func TestPrepareWindowData_OldReadingsUseFallback(t *testing.T) {
	now := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	// All readings are older than the window - should use fallback (last known value)
	readings := Readings{
		{Value: 50.0, Timestamp: now.Add(-5 * time.Minute)},
		{Value: 75.0, Timestamp: now.Add(-3 * time.Minute)},
	}
	pairs, _, fallback := prepareWindowData(readings, 1*time.Minute, now)

	// Should return nil pairs and last known value as fallback
	assert.Nil(t, pairs)
	assert.Equal(t, 75.0, fallback)
}

func TestCalculateSelectedPercentile_TimeWeighting(t *testing.T) {
	now := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	// First value held for 10s, second value held for 49s
	// Note: readings at exactly -60s are excluded (not strictly after cutoff)
	readings := Readings{
		{Value: 100.0, Timestamp: now.Add(-59 * time.Second)},
		{Value: 200.0, Timestamp: now.Add(-49 * time.Second)},
	}
	pairs, totalDuration, fallback := prepareWindowData(readings, 1*time.Minute, now)

	p1 := calculateSelectedPercentile(pairs, totalDuration, 1, fallback)
	p50 := calculateSelectedPercentile(pairs, totalDuration, 50, fallback)
	p66 := calculateSelectedPercentile(pairs, totalDuration, 66, fallback)
	p99 := calculateSelectedPercentile(pairs, totalDuration, 99, fallback)

	// Sorted by value: 100 (10s), 200 (49s). Total 59s.
	// P50 target = 29.5s. After 100's 10s, cumulative = 10s. After 200's 49s, cumulative = 59s.
	// 29.5s > 10s, so we're in 200's range. P50 = 200
	// P66 target = 38.94s > 10s, so P66 = 200
	assert.Equal(t, 100.0, p1)
	assert.Equal(t, 200.0, p50)
	assert.Equal(t, 200.0, p66)
	assert.Equal(t, 200.0, p99)
}

func TestCalculateRequiredStats_UpdatesSpecifiedPercentiles(t *testing.T) {
	now := time.Now()
	readings := Readings{
		{Value: 100.0, Timestamp: now.Add(-30 * time.Second)},
	}

	// Temporarily add a test topic to the registry
	testTopic := "test/topic/for/unit/test"
	requiredPercentiles[testTopic] = []PercentileSpec{
		{50, 5 * time.Minute},
		{99, 15 * time.Minute},
	}
	defer delete(requiredPercentiles, testTopic)

	percentiles := make(map[PercentileKey]float64)
	calculateRequiredStats(testTopic, readings, percentiles)

	// Only registered specs should be set
	assert.Equal(t, 100.0, percentiles[PercentileKey{testTopic, 50, Window5Min}])
	assert.Equal(t, 100.0, percentiles[PercentileKey{testTopic, 99, Window15Min}])
	// Unregistered combinations should not exist
	_, exists := percentiles[PercentileKey{testTopic, 50, 1 * time.Minute}]
	assert.False(t, exists)
	_, exists = percentiles[PercentileKey{testTopic, 50, Window15Min}]
	assert.False(t, exists)
	_, exists = percentiles[PercentileKey{testTopic, 1, Window15Min}]
	assert.False(t, exists)
}

func TestCalculateSelectedPercentile_MillisecondDurations(t *testing.T) {
	now := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	// Test with sub-second (millisecond) durations
	readings := Readings{
		{Value: 100.0, Timestamp: now.Add(-500 * time.Millisecond)},
		{Value: 200.0, Timestamp: now.Add(-250 * time.Millisecond)},
	}
	pairs, totalDuration, fallback := prepareWindowData(readings, 1*time.Second, now)

	p1 := calculateSelectedPercentile(pairs, totalDuration, 1, fallback)
	p50 := calculateSelectedPercentile(pairs, totalDuration, 50, fallback)
	p66 := calculateSelectedPercentile(pairs, totalDuration, 66, fallback)
	p99 := calculateSelectedPercentile(pairs, totalDuration, 99, fallback)

	// First reading active for 250ms (100), second for 250ms (200)
	// Total 500ms. Sorted: 100 (250ms), 200 (250ms)
	// P50 target = 250ms. After 100, cumulative = 250ms. P50 = 100
	// P66 target = 330ms. After 100, cumulative = 250ms. After 200, cumulative = 500ms. P66 = 200
	assert.Equal(t, 100.0, p1)
	assert.Equal(t, 100.0, p50)
	assert.Equal(t, 200.0, p66)
	assert.Equal(t, 200.0, p99)
}

func TestCalculateSelectedPercentile_ShortSpike(t *testing.T) {
	now := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	// Simulate a short 100ms spike in the middle of stable readings
	readings := Readings{
		{Value: 100.0, Timestamp: now.Add(-500 * time.Millisecond)},
		{Value: 500.0, Timestamp: now.Add(-300 * time.Millisecond)}, // spike
		{Value: 100.0, Timestamp: now.Add(-200 * time.Millisecond)},
	}
	pairs, totalDuration, fallback := prepareWindowData(readings, 1*time.Second, now)

	p1 := calculateSelectedPercentile(pairs, totalDuration, 1, fallback)
	p50 := calculateSelectedPercentile(pairs, totalDuration, 50, fallback)
	p66 := calculateSelectedPercentile(pairs, totalDuration, 66, fallback)
	p99 := calculateSelectedPercentile(pairs, totalDuration, 99, fallback)

	// Durations: 100 for 200ms, 500 for 100ms, 100 for 200ms
	// Sorted by value: 100 (400ms total), 500 (100ms)
	// Total 500ms. P50 target = 250ms. After 100, cumulative = 400ms >= 250ms.
	// P50 = 100, P66 = 100 (spike is filtered out!)
	assert.Equal(t, 100.0, p1)
	assert.Equal(t, 100.0, p50)
	assert.Equal(t, 100.0, p66)
	// P99 target = 495ms. After 100, cumulative = 400ms. After 500, cumulative = 500ms >= 495ms.
	// P99 = 500
	assert.Equal(t, 500.0, p99)
}

func TestPrepareWindowData_ZeroDuration(t *testing.T) {
	now := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	// Simulate startup: single reading with timestamp exactly equal to now
	readings := Readings{
		{Value: 100.0, Timestamp: now},
	}
	pairs, _, fallback := prepareWindowData(readings, 1*time.Minute, now)

	// Single reading returns nil pairs, uses fallback
	assert.Nil(t, pairs)
	assert.Equal(t, 100.0, fallback)
}

func TestCalculateSelectedPercentile_Basic(t *testing.T) {
	// 100 for 60% of time, 200 for 40% of time
	pairs := []weightedValue{
		{value: 100, duration: 60},
		{value: 200, duration: 40},
	}
	totalDuration := 100.0
	fallback := 0.0

	p1 := calculateSelectedPercentile(pairs, totalDuration, 1, fallback)
	p50 := calculateSelectedPercentile(pairs, totalDuration, 50, fallback)
	p66 := calculateSelectedPercentile(pairs, totalDuration, 66, fallback)
	p99 := calculateSelectedPercentile(pairs, totalDuration, 99, fallback)

	// P1: target 1s, should be 100
	assert.Equal(t, 100.0, p1)
	// P50: target 50s, should be 100 (cumulative after 100 is 60s >= 50s)
	assert.Equal(t, 100.0, p50)
	// P66: target 66s, should be 200 (cumulative after 100 is 60s, after 200 is 100s >= 66s)
	assert.Equal(t, 200.0, p66)
	// P99: target 99s, should be 200
	assert.Equal(t, 200.0, p99)
}

func TestCalculateSelectedPercentile_OutlierFiltering(t *testing.T) {
	// Simulate: stable at 100 for 98s, brief spike to 1000 for 2s
	// Sorted: 100 (98s), 1000 (2s)
	pairs := []weightedValue{
		{value: 100, duration: 98},
		{value: 1000, duration: 2},
	}
	totalDuration := 100.0
	fallback := 0.0

	// P99 target = 99s. After 100, cumulative = 98s. After 1000, cumulative = 100s >= 99s.
	// P99 = 1000 (the spike IS captured by P99)
	p99 := calculateSelectedPercentile(pairs, totalDuration, 99, fallback)
	assert.Equal(t, 1000.0, p99)

	// But if spike is only 1% of time, P99 should filter it
	pairs2 := []weightedValue{
		{value: 100, duration: 99},
		{value: 1000, duration: 1},
	}
	// P99 target = 99s. After 100, cumulative = 99s >= 99s. P99 = 100!
	p99_2 := calculateSelectedPercentile(pairs2, totalDuration, 99, fallback)
	assert.Equal(t, 100.0, p99_2)
}

func TestCalculateRequiredStats_UnregisteredTopicSkipped(t *testing.T) {
	now := time.Now()
	readings := Readings{
		{Value: 100.0, Timestamp: now.Add(-30 * time.Second)},
	}

	// Use a topic that's not in the registry
	unregisteredTopic := "unregistered/topic/not/in/registry"
	percentiles := make(map[PercentileKey]float64)
	calculateRequiredStats(unregisteredTopic, readings, percentiles)

	// Map should remain empty (nothing calculated for unregistered topic)
	assert.Empty(t, percentiles)
}
