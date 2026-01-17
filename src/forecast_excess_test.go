package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// Helper to create forecast periods starting from a given time
func makeForecastPeriods(start time.Time, pvEstimates ...float64) ForecastPeriods {
	periods := make(ForecastPeriods, len(pvEstimates))
	for i, pv := range pvEstimates {
		periods[i] = ForecastPeriod{
			PeriodStart: start.Add(time.Duration(i) * 30 * time.Minute),
			PvEstimate:  pv,
		}
	}
	return periods
}

// Calculate total forecast remaining in Wh from periods
func forecastRemainingWh(periods ForecastPeriods) float64 {
	var total float64
	for _, p := range periods {
		total += p.PvEstimate * 0.5 * 1000 // kW * 0.5h * 1000 = Wh
	}
	return total
}

func TestForecastExcessRequestCore_NoExcessEnergy(t *testing.T) {
	// Test case 1: No excess energy
	// - Capacity: 10000 Wh, AvailableWh: 5000 Wh
	// - Forecast: 4 periods of 2.0 kW each (2 hours total)
	// - Total remaining: 4 × 2.0 × 0.5 = 4000 Wh
	// - Excess: 5000 + 4000 - 10000 = -1000 Wh → 0 watts

	now := time.Date(2026, 1, 17, 10, 0, 0, 0, time.UTC)
	forecast := makeForecastPeriods(now, 2.0, 2.0, 2.0, 2.0)

	input := ForecastExcessInput{
		Now:                 now,
		ForecastRemainingWh: forecastRemainingWh(forecast),
		Forecast:            forecast,
		AvailableWh:         5000,
		InverterCount:       4,
		WattsPerInverter:    250,
		SolarMultiplier:     1.0,
		CapacityWh:          10000,
		ShortName:           "Test",
	}

	state := &ForecastExcessState{}
	result := forecastExcessRequestCore(input, state)

	assert.Equal(t, "Forecast Excess (Test)", result.Name)
	assert.Equal(t, 0.0, result.Watts, "Should return 0 watts when no excess energy")
}

func TestForecastExcessRequestCore_HasExcessEnergy(t *testing.T) {
	// Test case 2: Battery has excess energy
	// - Capacity: 10000 Wh, AvailableWh: 8000 Wh
	// - Forecast: 4 periods of 2.0 kW (2 hours, 4000 Wh total)
	// - Excess: 8000 + 4000 - 10000 = 2000 Wh over 2 hours → 1000W

	now := time.Date(2026, 1, 17, 10, 0, 0, 0, time.UTC)
	forecast := makeForecastPeriods(now, 2.0, 2.0, 2.0, 2.0)

	input := ForecastExcessInput{
		Now:                 now,
		ForecastRemainingWh: forecastRemainingWh(forecast),
		Forecast:            forecast,
		AvailableWh:         8000,
		InverterCount:       4,
		WattsPerInverter:    250,
		SolarMultiplier:     1.0,
		CapacityWh:          10000,
		ShortName:           "Test",
	}

	state := &ForecastExcessState{}
	result := forecastExcessRequestCore(input, state)

	assert.Equal(t, "Forecast Excess (Test)", result.Name)
	assert.InDelta(t, 1000.0, result.Watts, 0.001, "Should return 1000W (2000 Wh / 2 hours)")
}

func TestForecastExcessRequestCore_SolarEndAtThreshold(t *testing.T) {
	// Test various timestamps leading up to solar cutoff
	// - Inverters: 5 at 250W = 1250W max, multiplier: 1.0 → threshold 1.250 kW
	// - Forecast: 2.0, 1.5, 1.0, 0.5 kW (solar ends after 1.5 kW period)
	// - Solar end time: 11:00 (end of 1.5 kW period, since 1.5 > 1.250 but 1.0 < 1.250)
	// - Battery starts at 9500 Wh and discharges ~100 Wh over the hour
	// - Capacity: 10000 Wh
	// - Forecast remaining decreases each step to bypass cache and show ratchet-down

	baseTime := time.Date(2026, 1, 17, 10, 0, 0, 0, time.UTC)
	forecast := makeForecastPeriods(baseTime, 2.0, 1.5, 1.0, 0.5)
	// Periods: 10:00-10:30 (2.0kW), 10:30-11:00 (1.5kW), 11:00-11:30 (1.0kW), 11:30-12:00 (0.5kW)
	// After cutoff (11:00): 750 Wh

	tests := []struct {
		name                string
		nowOffset           time.Duration
		forecastRemainingWh float64 // Decreases as solar is generated
		availableWh         float64 // Battery slowly discharging
		expectedWatts       float64
	}{
		{
			name:                "10:00 - start of day",
			nowOffset:           0,
			forecastRemainingWh: 2500, // All 4 periods
			availableWh:         9500,
			// Hours: 1.0, Before cutoff: 2500-750=1750
			// Excess: 9500+1750-10000=1250 Wh
			// Optimal: 1250/1.0=1250W (hits cap)
			expectedWatts: 1250,
		},
		{
			name:                "10:15 - mid first period",
			nowOffset:           15 * time.Minute,
			forecastRemainingWh: 2250, // Half of first period generated (250 Wh)
			availableWh:         9475, // -25 Wh discharged
			// Hours: 0.75, Before cutoff: 2250-750=1500
			// Excess: 9475+1500-10000=975 Wh
			// Optimal before handoff: 975/0.75=1300W
			// Handoff factor: 0.75/1.0=0.75
			// Optimal after handoff: 1300*0.75=975W
			// Ratchet-down: min(1250, 975)=975W
			expectedWatts: 975,
		},
		{
			name:                "10:30 - start of second period",
			nowOffset:           30 * time.Minute,
			forecastRemainingWh: 1500, // First period complete (1000 Wh generated)
			availableWh:         9450, // -50 Wh discharged
			// Hours: 0.5, Before cutoff: 1500-750=750
			// Excess: 9450+750-10000=200 Wh
			// Optimal before handoff: 200/0.5=400W
			// Handoff factor: 0.5/1.0=0.5
			// Optimal after handoff: 400*0.5=200W
			// Ratchet-down: min(975, 200)=200W
			expectedWatts: 200,
		},
		{
			name:                "10:45 - mid second period",
			nowOffset:           45 * time.Minute,
			forecastRemainingWh: 1125, // Half of second period generated (375 Wh)
			availableWh:         9425, // -75 Wh discharged
			// Hours: 0.25, Before cutoff: 1125-750=375
			// Excess: 9425+375-10000=-200 Wh → 0W (no excess)
			expectedWatts: 0,
		},
	}

	// Single state across all timestamps - ratchet-down prevents increases
	state := &ForecastExcessState{}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := ForecastExcessInput{
				Now:                 baseTime.Add(tt.nowOffset),
				ForecastRemainingWh: tt.forecastRemainingWh,
				Forecast:            forecast,
				AvailableWh:         tt.availableWh,
				InverterCount:       5,
				WattsPerInverter:    250,
				SolarMultiplier:     1.0,
				CapacityWh:          10000,
				ShortName:           "Test",
			}

			result := forecastExcessRequestCore(input, state)

			assert.Equal(t, "Forecast Excess (Test)", result.Name)
			assert.InDelta(t, tt.expectedWatts, result.Watts, 0.001, "Watts mismatch")
		})
	}
}
