package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestSumGenerationAfter(t *testing.T) {
	// Create test periods: 4 consecutive 30-min periods starting at noon
	noon := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	periods := ForecastPeriods{
		{PeriodStart: noon, PvEstimate: 2.0},                       // 12:00-12:30, 2kW
		{PeriodStart: noon.Add(30 * time.Minute), PvEstimate: 3.0}, // 12:30-13:00, 3kW
		{PeriodStart: noon.Add(60 * time.Minute), PvEstimate: 1.0}, // 13:00-13:30, 1kW
		{PeriodStart: noon.Add(90 * time.Minute), PvEstimate: 0.5}, // 13:30-14:00, 0.5kW
	}

	tests := []struct {
		name        string
		cutoff      time.Time
		expectedKwh float64
	}{
		{
			name:   "cutoff at start includes all",
			cutoff: noon,
			// All 4 periods: 2*0.5 + 3*0.5 + 1*0.5 + 0.5*0.5 = 3.25 kWh
			expectedKwh: 3.25,
		},
		{
			name:   "cutoff in middle",
			cutoff: noon.Add(time.Hour), // 13:00
			// Last 2 periods: 1*0.5 + 0.5*0.5 = 0.75 kWh
			expectedKwh: 0.75,
		},
		{
			name:   "cutoff at last period",
			cutoff: noon.Add(90 * time.Minute), // 13:30
			// Last period: 0.5*0.5 = 0.25 kWh
			expectedKwh: 0.25,
		},
		{
			name:   "cutoff after all periods",
			cutoff: noon.Add(2 * time.Hour), // 14:00
			// No periods
			expectedKwh: 0,
		},
		{
			name:   "cutoff mid-period counts full period",
			cutoff: noon.Add(45 * time.Minute), // 12:45 (mid second period)
			// Periods starting at 12:30, 13:00, 13:30: 3*0.5 + 1*0.5 + 0.5*0.5 = 2.25 kWh
			// Note: 12:30 period starts before 12:45, so it's NOT included
			expectedKwh: 0.75,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := periods.SumGenerationAfter(tt.cutoff)
			assert.InDelta(t, tt.expectedKwh, result, 0.001)
		})
	}
}

func TestSumGenerationAfter_EmptyPeriods(t *testing.T) {
	periods := ForecastPeriods{}
	noon := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)

	result := periods.SumGenerationAfter(noon)
	assert.Equal(t, 0.0, result)
}
