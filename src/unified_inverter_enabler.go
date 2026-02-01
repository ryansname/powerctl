package main

import (
	"context"
	"fmt"
	"log"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/ryansname/powerctl/src/governor"
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

// PowerRequest represents a power request from a rule
type PowerRequest struct {
	Name  string
	Watts float64
}

// PowerLimit represents a power limit from a rule
type PowerLimit struct {
	Name  string
	Watts float64
}

// InverterInfo holds information about a single inverter
type InverterInfo struct {
	EntityID   string // e.g., "switch.powerhouse_inverter_1_switch_0"
	StateTopic string // e.g., "homeassistant/switch/powerhouse_inverter_1_switch_0/state"
}

// BatteryInverterGroup holds inverters for a single battery
type BatteryInverterGroup struct {
	Name                 string
	ShortName            string // Short name for display (e.g., "B2", "B3")
	Inverters            []InverterInfo
	ChargeStateTopic     string
	SOCTopic             string
	CapacityWh           float64 // Battery capacity in Wh (e.g., 10000 for 10 kWh)
	SolarMultiplier      float64 // Multiplier for solar forecast (e.g., 4.5)
	AvailableEnergyTopic string  // Topic for battery available energy
}

// UnifiedInverterConfig holds configuration for the unified inverter enabler
type UnifiedInverterConfig struct {
	Battery2 BatteryInverterGroup
	Battery3 BatteryInverterGroup

	// Topics for mode selection and target calculation
	SolarForecastTopic          string // Total forecast for today (kWh, converted to Wh)
	SolarForecastRemainingTopic string // Remaining forecast for today (kWh, converted to Wh)
	DetailedForecastTopic       string // JSON array of forecast periods
	Solar1PowerTopic            string
	Solar2PowerTopic            string
	LoadPowerTopic              string
	PowerwallSOCTopic           string

	// Grid status topic (binary sensor, "on" = grid available)
	GridStatusTopic string

	// AC frequency topic for high frequency protection
	ACFrequencyTopic string

	// Constants
	WattsPerInverter float64
	MaxTransferPower float64

	// Powerwall Low mode configuration (SOC-based hysteresis, 9 inverters)
	PowerwallLowSOCTurnOnStart  float64 // 41% - first inverter turns on when SOC drops below
	PowerwallLowSOCTurnOnEnd    float64 // 25% - last inverter turns on when SOC drops below
	PowerwallLowSOCTurnOffStart float64 // 28% - first inverter turns off when SOC rises above
	PowerwallLowSOCTurnOffEnd   float64 // 44% - last inverter turns off when SOC rises above

	// Overflow mode configuration (SOC-based hysteresis)
	OverflowFloatChargeState string  // "Float Charging"
	OverflowSOCTurnOffStart  float64 // 98.5% - first inverter turns off when SOC drops below
	OverflowSOCTurnOffEnd    float64 // 95.0% - last inverter turns off when SOC drops below
	OverflowSOCTurnOnStart   float64 // 95.75% - first inverter turns on when SOC rises above
	OverflowSOCTurnOnEnd     float64 // 99.5% - last inverter turns on when SOC rises above
}

// ForecastExcessState tracks per-battery state for forecast excess inverter mode
type ForecastExcessState struct {
	currentTargetWatts    float64
	lastActiveDate        time.Time // For daily reset (zero value triggers reset on startup)
	lastForecastRemaining float64   // For caching (only recalculate when forecast changes)
	cachedResult          PowerRequest

	// Debug values from last calculation (published to HA sensors)
	DebugExpectedSolarWh float64
	DebugExcessWh        float64
}

// ForecastExcessInput holds typed input data for forecastExcessRequestCore
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

// InverterEnablerState holds runtime state for the unified inverter enabler
type InverterEnablerState struct {
	// Last published debug output for change detection
	lastDebugOutput string
	// Per-battery forecast excess state
	forecastExcess2 ForecastExcessState
	forecastExcess3 ForecastExcessState

	// Slow ramp smoothing for Powerwall Last mode (pressure-gated accelerating ramp)
	powerwallLastRamp governor.SlowRampState

	// Stepped hysteresis controllers
	powerwallLow   *governor.SteppedHysteresis // Powerwall Low mode (9 inverters)
	overflow2      *governor.SteppedHysteresis // Overflow mode for Battery 2
	overflow3      *governor.SteppedHysteresis // Overflow mode for Battery 3
	socLimit2      *governor.SteppedHysteresis // SOC-based inverter limit for Battery 2
	socLimit3      *governor.SteppedHysteresis // SOC-based inverter limit for Battery 3
}

// ModeResult represents the outcome of mode selection (in inverter counts)
type ModeResult struct {
	Battery2Count int
	Battery3Count int
}

// TotalCount returns the total number of inverters
func (m ModeResult) TotalCount() int {
	return m.Battery2Count + m.Battery3Count
}

// ModeState represents a mode's value and whether it's contributing to the final inverter count
type ModeState struct {
	Name         string
	Watts        float64
	Contributing bool
}

// DebugModeInfo contains all mode states for debug output
type DebugModeInfo struct {
	Modes           []ModeState
	SafetyReason    string  // Non-empty when safety protection is active
	GridFreqCurrent float64 // Current AC frequency (for safety display)
	GridFreqP100    float64 // AC frequency 5min P100 (for safety display)
	PowerwallSOC    float64 // Powerwall SOC % (for safety display)
}

// checkBatteryOverflow returns inverter count for overflow mode using SOC-based hysteresis.
// If not Float Charging, returns 0.
// Otherwise, uses the SteppedHysteresis controller to determine inverter count.
func checkBatteryOverflow(
	data DisplayData,
	battery BatteryInverterGroup,
	config UnifiedInverterConfig,
	hysteresis *governor.SteppedHysteresis,
) PowerRequest {
	name := "Overflow (" + battery.ShortName + ")"

	chargeState := data.GetString(battery.ChargeStateTopic)
	isFloatCharging := strings.Contains(chargeState, config.OverflowFloatChargeState)

	if !isFloatCharging {
		return PowerRequest{Name: name, Watts: 0}
	}

	soc := data.GetFloat(battery.SOCTopic).Current
	count := hysteresis.Update(soc)

	return PowerRequest{Name: name, Watts: float64(count) * config.WattsPerInverter}
}

// applyLimitToPerBattery applies a global limit to per-battery counts.
// When reducing, it reduces from the higher count first (B3 wins ties).
func applyLimitToPerBattery(b2Count, b3Count int, limitWatts, wattsPerInverter float64) (int, int) {
	maxTotal := int(limitWatts / wattsPerInverter)
	for b2Count+b3Count > maxTotal {
		switch {
		case b2Count > b3Count:
			b2Count--
		case b3Count > 0:
			b3Count--
		default:
			b2Count--
		}
	}
	return b2Count, b3Count
}

// roundRobinFromBase adds inverters to reach targetTotal using strict alternation.
// Starts from B3: B3 → B2 → B3 → B2... (skips if at max).
// max2/max3 come from maxInvertersForSOC to respect SOC limits.
func roundRobinFromBase(base2, base3, targetTotal, max2, max3 int) (b2Count, b3Count int) {
	b2Count, b3Count = base2, base3
	turn := 3 // Start with B3
	for b2Count+b3Count < targetTotal {
		if turn == 3 {
			if b3Count < max3 {
				b3Count++
			}
			turn = 2
		} else {
			if b2Count < max2 {
				b2Count++
			}
			turn = 3
		}
		if b2Count >= max2 && b3Count >= max3 {
			break
		}
	}
	return
}

// selectMode calculates per-battery overflow/forecast excess and global targets, applying limits,
// and returns whichever produces higher total inverter count.
// If global is higher, it starts from limited per-battery base and round-robins additional inverters.
func selectMode(
	data DisplayData,
	config UnifiedInverterConfig,
	state *InverterEnablerState,
) (ModeResult, DebugModeInfo) {
	gridAvailable := data.GetBoolean(config.GridStatusTopic)
	powerwallSOC := data.GetFloat(config.PowerwallSOCTopic).Current
	acFreqCurrent := data.GetFloat(config.ACFrequencyTopic).Current
	acFreqP100 := data.GetPercentile(config.ACFrequencyTopic, P100, Window5Min)

	// Safety check: High grid frequency (>52.75Hz) - disable all inverters
	// Uses 5min P100 (max) to stay in safety mode until frequency has been stable
	if acFreqP100 > 52.75 {
		return ModeResult{}, DebugModeInfo{
			SafetyReason:    "High frequency",
			GridFreqCurrent: acFreqCurrent,
			GridFreqP100:    acFreqP100,
			PowerwallSOC:    powerwallSOC,
		}
	}

	// Safety check: Grid off + high Powerwall SOC (>90%) - disable all inverters
	if !gridAvailable && powerwallSOC > 90.0 {
		return ModeResult{}, DebugModeInfo{
			SafetyReason:    "Grid off + high Powerwall",
			GridFreqCurrent: acFreqCurrent,
			GridFreqP100:    acFreqP100,
			PowerwallSOC:    powerwallSOC,
		}
	}

	// Grid off: disable per-battery modes (overflow and forecast excess)
	// Global modes (Powerwall Last/Low) still work to help supply house during outages

	// 1. Calculate per-battery overflow (SOC-based hysteresis)
	overflow2 := checkBatteryOverflow(data, config.Battery2, config, state.overflow2)
	overflow3 := checkBatteryOverflow(data, config.Battery3, config, state.overflow3)

	// 2. Calculate per-battery forecast excess (already capped at max inverter power)
	forecastExcess2 := forecastExcessRequest(data, config, config.Battery2, &state.forecastExcess2)
	forecastExcess3 := forecastExcessRequest(data, config, config.Battery3, &state.forecastExcess3)

	// Zero out per-battery modes when grid is unavailable
	if !gridAvailable {
		overflow2.Watts = 0
		overflow3.Watts = 0
		forecastExcess2.Watts = 0
		forecastExcess3.Watts = 0
	}

	// 3. For each battery, take max of overflow and forecast excess
	perBattery2 := maxPowerRequest(overflow2, forecastExcess2)
	perBattery3 := maxPowerRequest(overflow3, forecastExcess3)
	perBattery2Count := calculateInverterCount(perBattery2.Watts, config.WattsPerInverter)
	perBattery3Count := calculateInverterCount(perBattery3.Watts, config.WattsPerInverter)

	// 3.5. Apply SOC-based limits to per-battery counts
	soc2 := data.GetFloat(config.Battery2.SOCTopic).Current
	soc3 := data.GetFloat(config.Battery3.SOCTopic).Current
	maxB2 := maxInvertersForSOC(soc2, state.socLimit2)
	maxB3 := maxInvertersForSOC(soc3, state.socLimit3)
	perBattery2Count = min(perBattery2Count, maxB2)
	perBattery3Count = min(perBattery3Count, maxB3)

	// 4. Apply global limit to per-battery counts (PowerhouseTransfer limit)
	limit := powerhouseTransferLimit(data, config)
	limitedB2, limitedB3 := applyLimitToPerBattery(perBattery2Count, perBattery3Count, limit.Watts, config.WattsPerInverter)
	limitedPerBatteryTotal := limitedB2 + limitedB3

	// 5. Calculate global targets (Powerwall modes)
	currentSolar := currentSolarGeneration(data, config)
	requests := []PowerRequest{
		powerwallLastRequest(data, config, currentSolar, state),
		checkPowerwallLow(data, config, state),
	}
	limits := []PowerLimit{limit}
	targetWatts, globalModes := calculateTargetPower(requests, limits)
	globalCount := calculateInverterCount(targetWatts, config.WattsPerInverter)

	// 6. Compare and select
	globalWins := globalCount > limitedPerBatteryTotal && targetWatts > 0

	// Clear global mode contributions if global doesn't win
	if !globalWins {
		for i := range globalModes {
			globalModes[i].Contributing = false
		}
	}

	// Build debug info with contributing flags
	debug := DebugModeInfo{
		Modes: []ModeState{
			{Name: "Forecast Excess (B2)", Watts: forecastExcess2.Watts, Contributing: limitedB2 > 0 && perBattery2.Name == forecastExcess2.Name},
			{Name: "Forecast Excess (B3)", Watts: forecastExcess3.Watts, Contributing: limitedB3 > 0 && perBattery3.Name == forecastExcess3.Name},
			globalModes[0],
			globalModes[1],
			{Name: "Overflow (B2)", Watts: overflow2.Watts, Contributing: limitedB2 > 0 && perBattery2.Name == overflow2.Name},
			{Name: "Overflow (B3)", Watts: overflow3.Watts, Contributing: limitedB3 > 0 && perBattery3.Name == overflow3.Name},
		},
	}

	if globalWins {
		// Global target is higher: round-robin from limited per-battery base
		b2, b3 := roundRobinFromBase(limitedB2, limitedB3, globalCount, maxB2, maxB3)
		return ModeResult{Battery2Count: b2, Battery3Count: b3}, debug
	}

	return ModeResult{Battery2Count: limitedB2, Battery3Count: limitedB3}, debug
}

// formatDebugOutput formats debug mode values as a GFM table for Home Assistant
func formatDebugOutput(debug DebugModeInfo) string {
	var sb strings.Builder

	// Safety mode: show reason and key values
	if debug.SafetyReason != "" {
		sb.WriteString("| Safety | Value |\n")
		sb.WriteString("|--------|------:|\n")
		fmt.Fprintf(&sb, "| Reason | %s |\n", debug.SafetyReason)
		fmt.Fprintf(&sb, "| Freq (now) | %.2f Hz |\n", debug.GridFreqCurrent)
		fmt.Fprintf(&sb, "| Freq (5m max) | %.2f Hz |\n", debug.GridFreqP100)
		fmt.Fprintf(&sb, "| Powerwall SOC | %.1f%% |\n", debug.PowerwallSOC)
		return sb.String()
	}

	// Normal mode: sort by watts descending
	modes := make([]ModeState, len(debug.Modes))
	copy(modes, debug.Modes)
	sort.Slice(modes, func(i, j int) bool {
		return modes[i].Watts > modes[j].Watts
	})

	sb.WriteString("| Mode | Watts |  |\n")
	sb.WriteString("|------|------:|--|\n")
	for _, m := range modes {
		marker := ""
		if m.Contributing && m.Watts > 0 {
			marker = "✓"
		}
		fmt.Fprintf(&sb, "| %s | %.0f | %s |\n", m.Name, m.Watts, marker)
	}

	return sb.String()
}

// unifiedInverterEnabler manages all inverters across both batteries
func unifiedInverterEnabler(
	ctx context.Context,
	dataChan <-chan DisplayData,
	config UnifiedInverterConfig,
	sender *MQTTSender,
) {
	log.Println("Unified inverter enabler started")

	b2Inverters := len(config.Battery2.Inverters)
	b3Inverters := len(config.Battery3.Inverters)

	state := &InverterEnablerState{
		// Powerwall Low: descending (SOC↓ → inverters↑), 9 steps
		powerwallLow: governor.NewSteppedHysteresis(
			9, false,
			config.PowerwallLowSOCTurnOnStart, config.PowerwallLowSOCTurnOnEnd,
			config.PowerwallLowSOCTurnOffStart, config.PowerwallLowSOCTurnOffEnd,
		),
		// Overflow: ascending (SOC↑ → inverters↑), per-battery
		overflow2: governor.NewSteppedHysteresis(
			b2Inverters, true,
			config.OverflowSOCTurnOnStart, config.OverflowSOCTurnOnEnd,
			config.OverflowSOCTurnOffStart, config.OverflowSOCTurnOffEnd,
		),
		overflow3: governor.NewSteppedHysteresis(
			b3Inverters, true,
			config.OverflowSOCTurnOnStart, config.OverflowSOCTurnOnEnd,
			config.OverflowSOCTurnOffStart, config.OverflowSOCTurnOffEnd,
		),
		// SOC Limits: ascending (SOC↑ → limit↑), steps = inverter count per battery
		// Enter thresholds: 15% → 25% (ascending)
		// Exit thresholds: 12.5% → 22.5% (ascending)
		socLimit2: governor.NewSteppedHysteresis(b2Inverters, true, 15, 25, 12.5, 22.5),
		socLimit3: governor.NewSteppedHysteresis(b3Inverters, true, 15, 25, 12.5, 22.5),
	}
	// Initialize SOC limits to max (all inverters allowed)
	state.socLimit2.Current = b2Inverters
	state.socLimit3.Current = b3Inverters

	for {
		select {
		case data := <-dataChan:
			modeResult, debugInfo := selectMode(data, config, state)

			// Publish debug output only when it changes
			debugOutput := formatDebugOutput(debugInfo)
			if debugOutput != state.lastDebugOutput {
				sender.CallService("input_text", "set_value", "input_text.powerhouse_control_debug", map[string]string{"value": debugOutput})
				state.lastDebugOutput = debugOutput
			}

			// Publish forecast excess debug sensors
			sender.PublishDebugSensor("powerctl_b2_expected_solar", state.forecastExcess2.DebugExpectedSolarWh)
			sender.PublishDebugSensor("powerctl_b2_excess", state.forecastExcess2.DebugExcessWh)
			sender.PublishDebugSensor("powerctl_b3_expected_solar", state.forecastExcess3.DebugExpectedSolarWh)
			sender.PublishDebugSensor("powerctl_b3_excess", state.forecastExcess3.DebugExcessWh)

			// Publish slow ramp debug sensors (Powerwall Last only)
			sender.PublishDebugSensor("powerctl_powerwall_last_smoothed", state.powerwallLastRamp.Current)
			sender.PublishDebugSensor("powerctl_powerwall_last_pressure", state.powerwallLastRamp.Pressure)

			// Publish Powerwall Low count for debugging
			sender.PublishDebugSensor("powerctl_powerwall_low_count", float64(state.powerwallLow.Current))

			// Apply changes
			changed := applyInverterChanges(data, config, sender, modeResult.Battery2Count, modeResult.Battery3Count)

			if changed {
				totalWatts := float64(modeResult.TotalCount()) * config.WattsPerInverter
				log.Printf("Unified inverter enabler: watts=%.0fW, B2=%d, B3=%d\n",
					totalWatts, modeResult.Battery2Count, modeResult.Battery3Count)
			}

		case <-ctx.Done():
			log.Println("Unified inverter enabler stopped")
			return
		}
	}
}

// forecastExcessRequest calculates forecast excess inverter power for a single battery.
// This is a wrapper that extracts data from DisplayData and calls forecastExcessRequestCore.
func forecastExcessRequest(
	data DisplayData,
	config UnifiedInverterConfig,
	battery BatteryInverterGroup,
	state *ForecastExcessState,
) PowerRequest {
	input := ForecastExcessInput{
		Now:                 time.Now(),
		ForecastRemainingWh: data.GetFloat(config.SolarForecastRemainingTopic).Current,
		AvailableWh:         data.GetFloat(battery.AvailableEnergyTopic).Current,
		InverterCount:       len(battery.Inverters),
		WattsPerInverter:    config.WattsPerInverter,
		SolarMultiplier:     battery.SolarMultiplier,
		CapacityWh:          battery.CapacityWh,
		ShortName:           battery.ShortName,
	}
	data.GetJSON(config.DetailedForecastTopic, &input.Forecast)
	return forecastExcessRequestCore(input, state)
}

// forecastExcessRequestCore calculates forecast excess inverter power for a single battery.
// Returns target watts based on excess energy divided by hours until solar ends.
// Target can only decrease during the day unless a daily reset occurs.
func forecastExcessRequestCore(input ForecastExcessInput, state *ForecastExcessState) PowerRequest {
	name := "Forecast Excess (" + input.ShortName + ")"

	// Check if forecast has changed - if not, return cached result
	if input.ForecastRemainingWh == state.lastForecastRemaining {
		return state.cachedResult
	}

	// Cache the result on any exit path
	var result PowerRequest
	defer func() {
		state.lastForecastRemaining = input.ForecastRemainingWh
		state.cachedResult = result
	}()

	// Night cycle check: if current forecast generation is 0, disable forecast excess
	currentGeneration := input.Forecast.GetCurrentGeneration(input.Now)
	if currentGeneration == 0 {
		result = PowerRequest{Name: name, Watts: 0}
		return result
	}

	// Find solar end time: last period where expected generation exceeds inverter capacity
	maxInverterWatts := float64(input.InverterCount) * input.WattsPerInverter
	minForecastKw := maxInverterWatts / (input.SolarMultiplier * 1000)
	solarEndTime := input.Forecast.FindSolarEndTime(minForecastKw)

	// Calculate hours remaining until solar end
	hoursRemaining := solarEndTime.Sub(input.Now).Hours()
	if hoursRemaining <= 0 {
		result = PowerRequest{Name: name, Watts: 0}
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
		result = PowerRequest{Name: name, Watts: 0}
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
	result = PowerRequest{Name: name, Watts: min(state.currentTargetWatts, maxInverterWatts)}
	return result
}

// slowRampConfig returns the configuration for Powerwall mode slow ramp smoothing.
// FullPressureDiff is set to 2x inverter's power (510W) so rate=1 at one inverter change.
// Damping is set so break-even point is at half an inverter's power (127.5W).
func slowRampConfig(wattsPerInverter float64) governor.SlowRampConfig {
	config := governor.DefaultSlowRampConfig()
	config.FullPressureDiff = wattsPerInverter * 2
	// Damping = breakEvenDiff / FullPressureDiff, where breakEvenDiff = wattsPerInverter / 2
	config.Damping = (wattsPerInverter / 2) / config.FullPressureDiff
	return config
}

// powerwallLastRequest returns 2/3 of the remaining load after solar subtraction,
// leaving the Powerwall to supply only 1/3 of the net load.
// Slow ramp smoothing is applied with pressure-gated accelerating ramp.
func powerwallLastRequest(
	data DisplayData,
	config UnifiedInverterConfig,
	currentSolar float64,
	state *InverterEnablerState,
) PowerRequest {
	loadPower := data.GetFloat(config.LoadPowerTopic).Current
	// Supply 2/3 of the remaining load after solar, leaving Powerwall to cover only 1/3
	// Allow negative targets so slow ramp can respond to excess solar generation
	rawTarget := (loadPower - currentSolar) * (2.0 / 3.0)
	smoothed := state.powerwallLastRamp.Update(rawTarget, slowRampConfig(config.WattsPerInverter))
	return PowerRequest{Name: "PowerwallLast", Watts: smoothed}
}

// checkPowerwallLow returns inverter count for Powerwall Low mode using SOC-based hysteresis.
// Uses Powerwall SOC (current value) to determine how many inverters to enable.
func checkPowerwallLow(
	data DisplayData,
	config UnifiedInverterConfig,
	state *InverterEnablerState,
) PowerRequest {
	soc := data.GetFloat(config.PowerwallSOCTopic).Current
	count := state.powerwallLow.Update(soc)
	return PowerRequest{Name: "PowerwallLow", Watts: float64(count) * config.WattsPerInverter}
}

// powerhouseTransferLimit returns the available capacity after accounting for solar generation
func powerhouseTransferLimit(data DisplayData, config UnifiedInverterConfig) PowerLimit {
	solar1Power15MinP99 := data.GetPercentile(config.Solar1PowerTopic, P99, Window15Min)
	availableCapacity := config.MaxTransferPower - solar1Power15MinP99
	return PowerLimit{Name: "PowerhouseTransfer", Watts: availableCapacity}
}

// calculateTargetPower computes target watts by taking max of all requests and applying all limits
// maxPowerRequest returns the PowerRequest with the highest watts
func maxPowerRequest(a, b PowerRequest) PowerRequest {
	if a.Watts >= b.Watts {
		return a
	}
	return b
}

func calculateTargetPower(requests []PowerRequest, limits []PowerLimit) (float64, []ModeState) {
	modes := make([]ModeState, len(requests))
	target := 0.0
	winningIdx := -1

	for i, r := range requests {
		modes[i] = ModeState{Name: r.Name, Watts: r.Watts}
		if r.Watts > target {
			target = r.Watts
			winningIdx = i
		}
	}

	// Mark the winner as contributing
	if winningIdx >= 0 {
		modes[winningIdx].Contributing = true
	}

	for _, l := range limits {
		target = min(target, l.Watts)
	}

	return max(target, 0), modes
}

// currentSolarGeneration returns the current solar generation from solar 1 and 2
func currentSolarGeneration(data DisplayData, config UnifiedInverterConfig) float64 {
	solar1 := data.GetFloat(config.Solar1PowerTopic).Current
	solar2 := data.GetFloat(config.Solar2PowerTopic).Current
	return solar1 + solar2
}

// calculateInverterCount computes how many inverters are needed for target power
func calculateInverterCount(targetWatts, wattsPerInverter float64) int {
	if targetWatts <= 0 {
		return 0
	}

	count := int(math.Ceil(targetWatts / wattsPerInverter))
	return min(count, 9)
}

// maxInvertersForSOC returns the max inverters allowed based on SOC percentage.
// Uses SteppedHysteresis where step count equals the battery's inverter count.
// Each step enables one additional inverter as SOC rises.
func maxInvertersForSOC(socPercent float64, hysteresis *governor.SteppedHysteresis) int {
	return hysteresis.Update(socPercent)
}

// applyInverterChanges enables/disables inverters to match desired counts
func applyInverterChanges(
	data DisplayData,
	config UnifiedInverterConfig,
	sender *MQTTSender,
	battery2Count, battery3Count int,
) bool {
	changed := false

	batteries := []struct {
		inverters []InverterInfo
		count     int
		name      string
	}{
		{config.Battery2.Inverters, battery2Count, "Battery 2"},
		{config.Battery3.Inverters, battery3Count, "Battery 3"},
	}

	for _, b := range batteries {
		for i, inv := range b.inverters {
			current := data.GetBoolean(inv.StateTopic)
			desired := i < b.count // Simple: enable first N inverters

			if current != desired {
				if desired {
					log.Printf("Enabling %s (%s)\n", inv.EntityID, b.name)
					sender.CallService("switch", "turn_on", inv.EntityID, nil)
				} else {
					log.Printf("Disabling %s (%s)\n", inv.EntityID, b.name)
					sender.CallService("switch", "turn_off", inv.EntityID, nil)
				}
				changed = true
			}
		}
	}

	return changed
}
