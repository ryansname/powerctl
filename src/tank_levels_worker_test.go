package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func makeTankInput(headerADC, storageADC float64) TankLevelInput {
	return TankLevelInput{
		HeaderADC:           headerADC,
		StorageADC:          storageADC,
		HeaderFullVoltage:   4.76,
		HeaderEmptyVoltage:  0.0,
		StorageFullVoltage:  5.0,
		StorageEmptyVoltage: 0.0,
	}
}

// covers: TANK-CALIB-1
func TestComputeTankLevels_HeaderLinearInterpolation(t *testing.T) {
	out := ComputeTankLevels(makeTankInput(3.23, -1))
	assert.True(t, out.HeaderValid)
	assert.InDelta(t, 67.9, out.Header.PercentFull, 0.001, "3.23V of 0-4.76V is 67.857%, rounded to 0.1")
}

// covers: TANK-CALIB-1 (non-zero empty voltage)
func TestComputeTankLevels_CalibrationOffset(t *testing.T) {
	in := makeTankInput(-1, 2.6)
	in.StorageEmptyVoltage = 0.2
	in.StorageFullVoltage = 4.86

	out := ComputeTankLevels(in)
	assert.True(t, out.StorageValid)
	assert.InDelta(t, 51.5, out.Storage.PercentFull, 0.001, "(2.6-0.2)/(4.86-0.2) = 51.502%")
}

// covers: TANK-HEADER-1
func TestComputeTankLevels_HeaderUnclamped(t *testing.T) {
	out := ComputeTankLevels(makeTankInput(5.0, -1))
	assert.True(t, out.HeaderValid)
	assert.InDelta(t, 105.0, out.Header.PercentFull, 0.001, "above full voltage reads above 100%")
}

// covers: TANK-STORAGE-1, TANK-STORAGE-2, TANK-STORAGE-3, TANK-STORAGE-4
func TestComputeTankLevels_StorageStackedTanks(t *testing.T) {
	out := ComputeTankLevels(makeTankInput(-1, 3.3)) // raw 66.0%
	assert.True(t, out.StorageValid)
	assert.InDelta(t, 66.0, out.Storage.PercentFull, 0.001)
	assert.InDelta(t, 0.0, out.Storage.Tank1PercentFull, 0.001, "(66-66.6)*3 clamps to 0")
	assert.InDelta(t, 98.1, out.Storage.Tank2PercentFull, 0.001, "(66-33.3)*3")
	assert.InDelta(t, 100.0, out.Storage.Tank3PercentFull, 0.001, "66*3 clamps to 100")
}

// covers: TANK-STORAGE-1
func TestComputeTankLevels_StorageOverallClamped(t *testing.T) {
	out := ComputeTankLevels(makeTankInput(-1, 5.5)) // raw 110%
	assert.True(t, out.StorageValid)
	assert.InDelta(t, 100.0, out.Storage.PercentFull, 0.001)

	out = ComputeTankLevels(makeTankInput(-1, 0)) // raw 0%
	assert.True(t, out.StorageValid)
	assert.InDelta(t, 0.0, out.Storage.PercentFull, 0.001)
	assert.InDelta(t, 0.0, out.Storage.Tank1PercentFull, 0.001)
}

// covers: TANK-VALID-1 (sentinel / no data yet)
func TestComputeTankLevels_NegativeADCInvalid(t *testing.T) {
	out := ComputeTankLevels(makeTankInput(-1, -1))
	assert.False(t, out.HeaderValid)
	assert.False(t, out.StorageValid)
}

// covers: TANK-VALID-1 (one sensor down doesn't take out the other)
func TestComputeTankLevels_IndependentValidity(t *testing.T) {
	out := ComputeTankLevels(makeTankInput(3.23, -1))
	assert.True(t, out.HeaderValid)
	assert.False(t, out.StorageValid)
}

// covers: TANK-VALID-2
func TestComputeTankLevels_DegenerateCalibrationInvalid(t *testing.T) {
	in := makeTankInput(3.23, 3.3)
	in.HeaderFullVoltage = in.HeaderEmptyVoltage
	in.StorageFullVoltage = in.StorageEmptyVoltage + 0.05 // below minCalibrationRange

	out := ComputeTankLevels(in)
	assert.False(t, out.HeaderValid)
	assert.False(t, out.StorageValid)
}

// covers: TANK-SMOOTH-1 (pins the P50/5m wiring through DisplayData)
func TestExtractTankLevelInput(t *testing.T) {
	data := DisplayData{
		TopicData: map[string]any{
			TopicHeaderTankFullVoltage:   &FloatTopicData{Current: 4.76},
			TopicHeaderTankEmptyVoltage:  &FloatTopicData{Current: 0.0},
			TopicStorageTankFullVoltage:  &FloatTopicData{Current: 4.86},
			TopicStorageTankEmptyVoltage: &FloatTopicData{Current: 0.2},
		},
		Percentiles: map[PercentileKey]float64{
			{TopicHeaderTankADC, P50, Window5Min}:  3.23,
			{TopicStorageTankADC, P50, Window5Min}: 2.6,
		},
	}

	in := ExtractTankLevelInput(data)
	assert.InDelta(t, 3.23, in.HeaderADC, 0.001)
	assert.InDelta(t, 2.6, in.StorageADC, 0.001)
	assert.InDelta(t, 4.76, in.HeaderFullVoltage, 0.001)
	assert.InDelta(t, 0.0, in.HeaderEmptyVoltage, 0.001)
	assert.InDelta(t, 4.86, in.StorageFullVoltage, 0.001)
	assert.InDelta(t, 0.2, in.StorageEmptyVoltage, 0.001)
}
