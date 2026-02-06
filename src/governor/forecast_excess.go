package governor

import (
	"time"
)

// ForecastPeriod represents a single period from Solcast detailed forecast
type ForecastPeriod struct {
	PeriodStart  time.Time `json:"period_start"`
	PvEstimate   float64   `json:"pv_estimate"`
	PvEstimate10 float64   `json:"pv_estimate10"`
	PvEstimate90 float64   `json:"pv_estimate90"`
}

// ForecastPeriods is a slice of ForecastPeriod with helper methods
type ForecastPeriods []ForecastPeriod

// FindSolarEndTime returns the end of the last period with generation exceeding the threshold.
// If no periods exceed the threshold, returns zero time.
func (periods ForecastPeriods) FindSolarEndTime(minPvEstimateKw float64) time.Time {
	var lastExceeding time.Time
	for _, period := range periods {
		if period.PvEstimate > minPvEstimateKw {
			lastExceeding = period.PeriodStart.Add(30 * time.Minute)
		}
	}
	return lastExceeding
}

// GetCurrentGeneration returns pv_estimate for the current 30-min period
func (periods ForecastPeriods) GetCurrentGeneration(now time.Time) float64 {
	for _, period := range periods {
		periodEnd := period.PeriodStart.Add(30 * time.Minute)
		if !now.Before(period.PeriodStart) && now.Before(periodEnd) {
			return period.PvEstimate
		}
	}
	return 0
}

// SumGenerationAfter returns total expected kWh from the cutoff time until end of forecast.
// Each period contributes (pv_estimate * 0.5) kWh since periods are 30 minutes.
func (periods ForecastPeriods) SumGenerationAfter(cutoff time.Time) float64 {
	var totalKwh float64
	for _, period := range periods {
		// Only count periods that start at or after cutoff
		if !period.PeriodStart.Before(cutoff) {
			totalKwh += period.PvEstimate * 0.5
		}
	}
	return totalKwh
}

// ForecastExcessResult holds the output of a forecast excess calculation
type ForecastExcessResult struct {
	Name  string
	Watts float64
}

// ForecastExcessState tracks per-battery state for forecast excess inverter mode
type ForecastExcessState struct {
	currentTargetWatts    float64
	lastActiveDate        time.Time              // For daily reset (zero value triggers reset on startup)
	lastForecastRemaining float64                // For caching (only recalculate when forecast changes)
	cachedResult          ForecastExcessResult

	// Debug values from last calculation (published to HA sensors)
	DebugExpectedSolarWh float64
	DebugExcessWh        float64
}

// ForecastExcessInput holds typed input data for ForecastExcessRequestCore
type ForecastExcessInput struct {
	Now                 time.Time
	ForecastRemainingWh float64
	Forecast            ForecastPeriods
	AvailableWh         float64
	InverterCount       int
	WattsPerInverter    float64
	SolarMultiplier     float64
	CapacityWh          float64
	ShortName           string
}

// ForecastExcessRequestCore calculates forecast excess inverter power for a single battery.
// Returns target watts based on excess energy divided by hours until solar ends.
// Target can only decrease during the day unless a daily reset occurs.
func ForecastExcessRequestCore(input ForecastExcessInput, state *ForecastExcessState) ForecastExcessResult {
	name := "Forecast Excess (" + input.ShortName + ")"

	// Cache key is intentionally only ForecastRemainingWh (not AvailableWh).
	// Solcast updates every 15-30 min; between updates, recalculating with stale forecast
	// but fresh battery data would produce worse results than the cached calculation.
	if input.ForecastRemainingWh == state.lastForecastRemaining {
		return state.cachedResult
	}

	// Cache the result on any exit path
	var result ForecastExcessResult
	defer func() {
		state.lastForecastRemaining = input.ForecastRemainingWh
		state.cachedResult = result
	}()

	// Night cycle check: if current forecast generation is 0, disable forecast excess
	currentGeneration := input.Forecast.GetCurrentGeneration(input.Now)
	if currentGeneration == 0 {
		result = ForecastExcessResult{Name: name, Watts: 0}
		return result
	}

	// Find solar end time: last period where expected generation exceeds inverter capacity
	maxInverterWatts := float64(input.InverterCount) * input.WattsPerInverter
	minForecastKw := (maxInverterWatts * 1.1) / (input.SolarMultiplier * 1000)
	solarEndTime := input.Forecast.FindSolarEndTime(minForecastKw)

	// Calculate hours remaining until solar end
	hoursRemaining := solarEndTime.Sub(input.Now).Hours()
	if hoursRemaining <= 0 {
		result = ForecastExcessResult{Name: name, Watts: 0}
		return result
	}

	// Check for daily reset (date changed, or zero value on startup)
	today := time.Date(input.Now.Year(), input.Now.Month(), input.Now.Day(), 0, 0, 0, 0, input.Now.Location())
	dailyReset := !state.lastActiveDate.Equal(today)
	if dailyReset {
		state.lastActiveDate = today
	}

	// Calculate excess energy
	// Exclude solar after cutoff - it can be fully inverted without using the battery
	forecastAfterCutoffKwh := input.Forecast.SumGenerationAfter(solarEndTime)
	solarBeforeCutoffWh := input.ForecastRemainingWh - (forecastAfterCutoffKwh * 1000)
	expectedSolarWh := input.SolarMultiplier * solarBeforeCutoffWh
	excessWh := (input.AvailableWh + expectedSolarWh) - input.CapacityWh

	// Store debug values for HA sensors
	state.DebugExpectedSolarWh = expectedSolarWh
	state.DebugExcessWh = excessWh

	if excessWh <= 0 {
		state.currentTargetWatts = 0
		result = ForecastExcessResult{Name: name, Watts: 0}
		return result
	}

	// Calculate optimal power
	optimalWatts := excessWh / hoursRemaining

	// Lerp down to 0 in the last hour before solar end for smooth handoff to other modes
	const handoffWindowHours = 1.0
	if hoursRemaining < handoffWindowHours {
		handoffFactor := hoursRemaining / handoffWindowHours
		optimalWatts *= handoffFactor
	}

	// Apply ratchet-down logic (can only decrease unless daily reset)
	if dailyReset {
		state.currentTargetWatts = optimalWatts
	} else {
		state.currentTargetWatts = min(state.currentTargetWatts, optimalWatts)
	}

	// Cap at maximum inverter power for this battery
	// Offset by half an inverter to counteract ceil rounding in calculateInverterCount
	halfInverter := 0.5 * input.WattsPerInverter
	result = ForecastExcessResult{Name: name, Watts: max(0, min(state.currentTargetWatts-halfInverter, maxInverterWatts))}
	return result
}
