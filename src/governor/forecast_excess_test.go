package governor

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
	result := ForecastExcessRequestCore(input, state)

	assert.Equal(t, "Forecast Excess (Test)", result.Name)
	assert.Equal(t, 0.0, result.Watts, "Should return 0 watts when no excess energy")
}

func TestForecastExcessRequestCore_HasExcessEnergy(t *testing.T) {
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
	result := ForecastExcessRequestCore(input, state)

	assert.Equal(t, "Forecast Excess (Test)", result.Name)
	assert.InDelta(t, 875.0, result.Watts, 0.001, "Should return 875W (1000W optimal - 125W half-inverter offset)")
}

func TestForecastExcessRequestCore_SolarEndAtThreshold(t *testing.T) {
	baseTime := time.Date(2026, 1, 17, 10, 0, 0, 0, time.UTC)
	forecast := makeForecastPeriods(baseTime, 2.0, 1.5, 1.0, 0.5)

	tests := []struct {
		name                string
		nowOffset           time.Duration
		forecastRemainingWh float64
		availableWh         float64
		expectedWatts       float64
	}{
		{
			name:                "10:00 - start of day",
			nowOffset:           0,
			forecastRemainingWh: 2500,
			availableWh:         9500,
			expectedWatts:       1125,
		},
		{
			name:                "10:15 - mid first period",
			nowOffset:           15 * time.Minute,
			forecastRemainingWh: 2250,
			availableWh:         9475,
			expectedWatts:       850,
		},
		{
			name:                "10:30 - start of second period",
			nowOffset:           30 * time.Minute,
			forecastRemainingWh: 1500,
			availableWh:         9450,
			expectedWatts:       75,
		},
		{
			name:                "10:45 - mid second period",
			nowOffset:           45 * time.Minute,
			forecastRemainingWh: 1125,
			availableWh:         9425,
			expectedWatts:       0,
		},
	}

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

			result := ForecastExcessRequestCore(input, state)

			assert.Equal(t, "Forecast Excess (Test)", result.Name)
			assert.InDelta(t, tt.expectedWatts, result.Watts, 0.001, "Watts mismatch")
		})
	}
}
