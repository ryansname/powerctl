package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCalculateAvailableWh_FullyCharged(t *testing.T) {
	// Battery at calibration point (100%)
	available := calculateAvailableWh(
		10000, // 10 kWh capacity in Wh
		100.0, // calibration inflows (kWh)
		50.0,  // calibration outflows (kWh)
		100.0, // current inflows = calibration (no change)
		50.0,  // current outflows = calibration (no change)
		0.02,  // 2% conversion loss
	)

	assert.Equal(t, 10000.0, available)
}

func TestCalculateAvailableWh_AfterDischarge(t *testing.T) {
	// Battery discharged 1 kWh since calibration
	available := calculateAvailableWh(
		10000, // 10 kWh capacity in Wh
		100.0, // calibration inflows (kWh)
		50.0,  // calibration outflows (kWh)
		100.0, // current inflows (no charging)
		51.0,  // current outflows = +1 kWh
		0.02,  // 2% conversion loss
	)

	// 1 kWh out * 1.02 loss = 1020 Wh used
	// Available = 10000 - 1020 = 8980 Wh
	assert.Equal(t, 8980.0, available)
}

func TestCalculateAvailableWh_AfterCharge(t *testing.T) {
	// Battery charged 1 kWh since calibration (starting from discharged state)
	available := calculateAvailableWh(
		10000, // 10 kWh capacity in Wh
		100.0, // calibration inflows (kWh)
		60.0,  // calibration outflows (kWh) - battery was at 100% when these were recorded
		101.0, // current inflows = +1 kWh
		60.0,  // current outflows (no discharge)
		0.02,  // 2% conversion loss
	)

	// At calibration: 100% = 10000 Wh
	// Energy in since calibration: 1 kWh = 1000 Wh
	// Energy out since calibration: 0
	// But this doesn't make physical sense - if battery was at 100% at calibration,
	// charging more would exceed capacity
	// The function clamps to capacity
	assert.Equal(t, 10000.0, available)
}

func TestCalculateAvailableWh_ClampsToZero(t *testing.T) {
	// Battery over-discharged (more out than capacity allows)
	available := calculateAvailableWh(
		10000, // 10 kWh capacity in Wh
		100.0, // calibration inflows (kWh)
		50.0,  // calibration outflows (kWh)
		100.0, // current inflows (no charging)
		61.0,  // current outflows = +11 kWh (more than capacity)
		0.02,  // 2% conversion loss
	)

	assert.Equal(t, 0.0, available)
}

func TestCalculateAvailableWh_ClampsToCapacity(t *testing.T) {
	// More energy in than possible
	available := calculateAvailableWh(
		10000, // 10 kWh capacity in Wh
		100.0, // calibration inflows (kWh)
		50.0,  // calibration outflows (kWh)
		120.0, // current inflows = +20 kWh
		50.0,  // current outflows (no discharge)
		0.02,  // 2% conversion loss
	)

	assert.Equal(t, 10000.0, available)
}

func TestCalculateAvailableWh_ZeroLossRate(t *testing.T) {
	// Test with no conversion losses
	available := calculateAvailableWh(
		10000, // 10 kWh capacity in Wh
		100.0, // calibration inflows (kWh)
		50.0,  // calibration outflows (kWh)
		100.0, // current inflows (no change)
		55.0,  // current outflows = +5 kWh
		0.0,   // no loss
	)

	// 5 kWh out = 5000 Wh used
	// Available = 10000 - 5000 = 5000 Wh
	assert.Equal(t, 5000.0, available)
}

func TestCalculateAvailableWh_ChargeAndDischarge(t *testing.T) {
	// Battery charged 2 kWh, discharged 1 kWh since calibration
	available := calculateAvailableWh(
		10000, // 10 kWh capacity in Wh
		100.0, // calibration inflows (kWh)
		50.0,  // calibration outflows (kWh)
		102.0, // current inflows = +2 kWh
		51.0,  // current outflows = +1 kWh
		0.02,  // 2% conversion loss
	)

	// Energy in: 2 kWh = 2000 Wh
	// Energy out: 1 kWh * 1.02 = 1020 Wh
	// Net: 2000 - 1020 = +980 Wh
	// Available = 10000 + 980 = 10980, clamped to 10000
	assert.Equal(t, 10000.0, available)
}
