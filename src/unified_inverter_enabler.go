package main

import (
	"context"
	"fmt"
	"log"
	"math"
	"sort"
	"strings"
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

// FindSolarEndTime returns the end of the last period with non-zero generation
func (periods ForecastPeriods) FindSolarEndTime() time.Time {
	var lastNonZero time.Time
	for _, period := range periods {
		if period.PvEstimate > 0 {
			lastNonZero = period.PeriodStart.Add(30 * time.Minute)
		}
	}
	return lastNonZero
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

	// Constants
	WattsPerInverter      float64
	MaxTransferPower      float64
	PowerwallLowThreshold float64

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
}

// InverterEnablerState holds runtime state for the unified inverter enabler
type InverterEnablerState struct {
	// Per-battery SOC limit with 2.5% hysteresis at each level
	// Stores current max inverters allowed (0, 1, 2, or hardwareMax for unlimited)
	battery2SOCLimit int
	battery3SOCLimit int
	// Last published debug output for change detection
	lastDebugOutput string
	// Per-battery forecast excess state
	forecastExcess2 ForecastExcessState
	forecastExcess3 ForecastExcessState
}

// ModeResult represents the outcome of mode selection (in inverter counts)
type ModeResult struct {
	RuleName      string
	Battery2Count int
	Battery3Count int
}

// TotalCount returns the total number of inverters
func (m ModeResult) TotalCount() int {
	return m.Battery2Count + m.Battery3Count
}

// DebugModeInfo contains all individual mode values for debug output
type DebugModeInfo struct {
	ForecastExcess2 float64
	ForecastExcess3 float64
	PowerwallLast   float64
	PowerwallLow    float64
	Overflow2       float64
	Overflow3       float64
	Winner          string
	SelectedB2      string // Name of winning mode for B2 (matches PowerRequest.Name)
	SelectedB3      string // Name of winning mode for B3 (matches PowerRequest.Name)
}

// checkBatteryOverflow returns inverter count for overflow mode using SOC-based hysteresis.
// If not Float Charging, returns 0.
// Otherwise, uses separate turn-on and turn-off thresholds to prevent oscillation.
func checkBatteryOverflow(
	data DisplayData,
	battery BatteryInverterGroup,
	config UnifiedInverterConfig,
) PowerRequest {
	name := "Overflow (" + battery.ShortName + ")"

	chargeState := data.GetString(battery.ChargeStateTopic)
	isFloatCharging := strings.Contains(chargeState, config.OverflowFloatChargeState)

	if !isFloatCharging {
		return PowerRequest{Name: name, Watts: 0}
	}

	soc := data.GetFloat(battery.SOCTopic).Current
	maxCount := len(battery.Inverters)

	// Count currently enabled inverters
	currentCount := 0
	for _, inv := range battery.Inverters {
		if data.GetBoolean(inv.StateTopic) {
			currentCount++
		}
	}

	// Calculate OFF count (max allowed based on falling thresholds)
	offCount := calculateOverflowOffCount(soc, maxCount, config)

	// Calculate ON count (min required based on rising thresholds)
	onCount := calculateOverflowOnCount(soc, maxCount, config)

	// Apply hysteresis
	var count int
	switch {
	case currentCount > offCount:
		count = offCount // SOC dropped, reduce
	case currentCount < onCount:
		count = onCount // SOC rose, increase
	default:
		count = currentCount // Stay in hysteresis zone
	}

	return PowerRequest{Name: name, Watts: float64(count) * config.WattsPerInverter}
}

// calculateOverflowOffCount returns max inverters allowed based on turn-off thresholds.
// Thresholds are evenly spread from TurnOffStart (98.5%) to TurnOffEnd (95.0%).
func calculateOverflowOffCount(soc float64, maxCount int, config UnifiedInverterConfig) int {
	if maxCount <= 1 {
		if soc >= config.OverflowSOCTurnOffStart {
			return maxCount
		}
		return 0
	}

	step := (config.OverflowSOCTurnOffStart - config.OverflowSOCTurnOffEnd) / float64(maxCount-1)
	for i := 1; i <= maxCount; i++ {
		threshold := config.OverflowSOCTurnOffStart - float64(i-1)*step
		if soc >= threshold {
			return maxCount - i + 1
		}
	}
	return 0
}

// calculateOverflowOnCount returns min inverters required based on turn-on thresholds.
// Thresholds are evenly spread from TurnOnStart (95.75%) to TurnOnEnd (99.5%).
func calculateOverflowOnCount(soc float64, maxCount int, config UnifiedInverterConfig) int {
	if maxCount <= 1 {
		if soc >= config.OverflowSOCTurnOnEnd {
			return maxCount
		}
		return 0
	}

	step := (config.OverflowSOCTurnOnEnd - config.OverflowSOCTurnOnStart) / float64(maxCount-1)
	for i := maxCount; i >= 1; i-- {
		threshold := config.OverflowSOCTurnOnStart + float64(i-1)*step
		if soc >= threshold {
			return i
		}
	}
	return 0
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
	// 1. Calculate per-battery overflow (SOC-based hysteresis)
	overflow2 := checkBatteryOverflow(data, config.Battery2, config)
	overflow3 := checkBatteryOverflow(data, config.Battery3, config)

	// 2. Calculate per-battery forecast excess (already capped at max inverter power)
	forecastExcess2 := forecastExcessRequest(data, config, config.Battery2, &state.forecastExcess2)
	forecastExcess3 := forecastExcessRequest(data, config, config.Battery3, &state.forecastExcess3)

	// 3. For each battery, take max of overflow and forecast excess
	perBattery2 := maxPowerRequest(overflow2, forecastExcess2)
	perBattery3 := maxPowerRequest(overflow3, forecastExcess3)
	perBattery2Count := calculateInverterCount(perBattery2.Watts, config.WattsPerInverter)
	perBattery3Count := calculateInverterCount(perBattery3.Watts, config.WattsPerInverter)

	// 3.5. Apply SOC-based limits to per-battery counts
	soc2 := data.GetFloat(config.Battery2.SOCTopic).Current
	soc3 := data.GetFloat(config.Battery3.SOCTopic).Current
	maxB2 := maxInvertersForSOC(soc2, len(config.Battery2.Inverters), &state.battery2SOCLimit)
	maxB3 := maxInvertersForSOC(soc3, len(config.Battery3.Inverters), &state.battery3SOCLimit)
	perBattery2Count = min(perBattery2Count, maxB2)
	perBattery3Count = min(perBattery3Count, maxB3)

	// 4. Apply global limit to per-battery counts (PowerhouseTransfer limit)
	limit := powerhouseTransferLimit(data, config)
	limitedB2, limitedB3 := applyLimitToPerBattery(perBattery2Count, perBattery3Count, limit.Watts, config.WattsPerInverter)
	limitedPerBatteryTotal := limitedB2 + limitedB3

	// 5. Calculate global targets (Powerwall modes only)
	currentSolar := currentSolarGeneration(data, config)
	requests := []PowerRequest{
		powerwallLastRequest(data, config, currentSolar),
		powerwallLowRequest(data, config, currentSolar),
	}
	limits := []PowerLimit{limit}
	targetWatts, winningRule := calculateTargetPower(requests, limits)
	globalCount := calculateInverterCount(targetWatts, config.WattsPerInverter)

	// Build debug info (before limiting for display)
	debug := DebugModeInfo{
		ForecastExcess2: forecastExcess2.Watts,
		ForecastExcess3: forecastExcess3.Watts,
		PowerwallLast:   requests[0].Watts,
		PowerwallLow:    requests[1].Watts,
		Overflow2:       overflow2.Watts,
		Overflow3:       overflow3.Watts,
		SelectedB2:      perBattery2.Name,
		SelectedB3:      perBattery3.Name,
	}

	// 6. Compare and select
	if globalCount > limitedPerBatteryTotal {
		// Global target is higher: round-robin from limited per-battery base
		b2, b3 := roundRobinFromBase(limitedB2, limitedB3, globalCount, maxB2, maxB3)

		// Winner is the global rule (only if non-zero)
		if targetWatts > 0 {
			debug.Winner = winningRule
		}
		return ModeResult{RuleName: winningRule, Battery2Count: b2, Battery3Count: b3}, debug
	}

	// Per-battery mode wins (overflow or forecast excess)
	if limitedPerBatteryTotal > 0 {
		switch {
		case limitedB2 > 0 && limitedB3 > 0:
			debug.Winner = "Per-Battery (B2+B3)"
		case limitedB2 > 0:
			debug.Winner = "Per-Battery (B2)"
		case limitedB3 > 0:
			debug.Winner = "Per-Battery (B3)"
		}
	}
	return ModeResult{
		RuleName:      debug.Winner,
		Battery2Count: limitedB2,
		Battery3Count: limitedB3,
	}, debug
}

// formatDebugOutput formats debug mode values as a GFM table for Home Assistant
func formatDebugOutput(debug DebugModeInfo) string {
	type modeValue struct {
		name  string
		watts float64
	}

	values := []modeValue{
		{"Forecast Excess (B2)", debug.ForecastExcess2},
		{"Forecast Excess (B3)", debug.ForecastExcess3},
		{"Powerwall Last", debug.PowerwallLast},
		{"Powerwall Low", debug.PowerwallLow},
		{"Overflow (B2)", debug.Overflow2},
		{"Overflow (B3)", debug.Overflow3},
	}

	// Sort by watts descending
	sort.Slice(values, func(i, j int) bool {
		return values[i].watts > values[j].watts
	})

	var sb strings.Builder
	sb.WriteString("| Mode | Watts |  |\n")
	sb.WriteString("|------|------:|--|\n")
	for _, v := range values {
		marker := ""
		isSelected := v.name == debug.Winner || v.name == debug.SelectedB2 || v.name == debug.SelectedB3
		if isSelected && v.watts > 0 {
			marker = "✓"
		}
		sb.WriteString(fmt.Sprintf("| %s | %.0f | %s |\n", v.name, v.watts, marker))
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

	state := &InverterEnablerState{
		battery2SOCLimit: len(config.Battery2.Inverters), // Start unlimited
		battery3SOCLimit: len(config.Battery3.Inverters), // Start unlimited
	}

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

			// Apply changes
			changed := applyInverterChanges(data, config, sender, modeResult.Battery2Count, modeResult.Battery3Count)

			if changed {
				totalWatts := float64(modeResult.TotalCount()) * config.WattsPerInverter
				log.Printf("Unified inverter enabler: rule=%s, watts=%.0fW, B2=%d, B3=%d\n",
					modeResult.RuleName, totalWatts, modeResult.Battery2Count, modeResult.Battery3Count)
			}

		case <-ctx.Done():
			log.Println("Unified inverter enabler stopped")
			return
		}
	}
}

// forecastExcessRequest calculates forecast excess inverter power for a single battery.
// Returns target watts based on excess energy divided by hours until solar ends.
// Target can only decrease during the day unless a daily reset occurs.
func forecastExcessRequest(
	data DisplayData,
	config UnifiedInverterConfig,
	battery BatteryInverterGroup,
	state *ForecastExcessState,
) PowerRequest {
	name := "Forecast Excess (" + battery.ShortName + ")"

	// Check if forecast has changed - if not, return cached result
	forecastRemainingWh := data.GetFloat(config.SolarForecastRemainingTopic).Current
	if forecastRemainingWh == state.lastForecastRemaining {
		return state.cachedResult
	}

	// Cache the result on any exit path
	var result PowerRequest
	defer func() {
		state.lastForecastRemaining = forecastRemainingWh
		state.cachedResult = result
	}()

	now := time.Now()

	// Parse forecast once, use for all operations
	var forecast ForecastPeriods
	data.GetJSON(config.DetailedForecastTopic, &forecast)

	// Night cycle check: if current forecast generation is 0, disable forecast excess
	currentGeneration := forecast.GetCurrentGeneration(now)
	if currentGeneration == 0 {
		result = PowerRequest{Name: name, Watts: 0}
		return result
	}

	// Find solar end time from forecast
	solarEndTime := forecast.FindSolarEndTime()

	// Calculate hours remaining until solar end
	hoursRemaining := solarEndTime.Sub(now).Hours()
	if hoursRemaining <= 0 {
		result = PowerRequest{Name: name, Watts: 0}
		return result
	}

	// Check for daily reset (date changed, or zero value on startup)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	dailyReset := !state.lastActiveDate.Equal(today)
	if dailyReset {
		state.lastActiveDate = today
	}

	// Calculate excess energy
	availableWh := data.GetFloat(battery.AvailableEnergyTopic).Current
	expectedSolarWh := battery.SolarMultiplier * forecastRemainingWh
	excessWh := (availableWh + expectedSolarWh) - battery.CapacityWh

	if excessWh <= 0 {
		state.currentTargetWatts = 0
		result = PowerRequest{Name: name, Watts: 0}
		return result
	}

	// Calculate optimal power
	optimalWatts := excessWh / hoursRemaining

	// Apply ratchet-down logic (can only decrease unless daily reset)
	if dailyReset {
		state.currentTargetWatts = optimalWatts
	} else {
		state.currentTargetWatts = min(state.currentTargetWatts, optimalWatts)
	}

	// Cap at maximum inverter power for this battery
	maxWatts := float64(len(battery.Inverters)) * config.WattsPerInverter
	result = PowerRequest{Name: name, Watts: min(state.currentTargetWatts, maxWatts)}
	return result
}

// powerwallLastRequest returns 2/3 of the 15min P66 load power, minus current solar generation
func powerwallLastRequest(
	data DisplayData,
	config UnifiedInverterConfig,
	currentSolar float64,
) PowerRequest {
	loadPower15MinP66 := data.GetPercentile(config.LoadPowerTopic, P66, Window15Min)
	targetLoad := loadPower15MinP66 * (2.0 / 3.0)
	return PowerRequest{Name: "PowerwallLast", Watts: max(targetLoad-currentSolar, 0)}
}

// powerwallLowRequest returns 15min P99 load minus current solar if powerwall SOC is low, else 0
func powerwallLowRequest(
	data DisplayData,
	config UnifiedInverterConfig,
	currentSolar float64,
) PowerRequest {
	powerwallSOC15MinP1 := data.GetPercentile(config.PowerwallSOCTopic, P1, Window15Min)

	if powerwallSOC15MinP1 >= config.PowerwallLowThreshold {
		return PowerRequest{Name: "PowerwallLow", Watts: 0}
	}

	loadPower := data.GetPercentile(config.LoadPowerTopic, P99, Window15Min)
	return PowerRequest{Name: "PowerwallLow", Watts: max(loadPower-currentSolar, 0)}
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

func calculateTargetPower(requests []PowerRequest, limits []PowerLimit) (float64, string) {
	target := 0.0
	winningRule := ""
	for _, r := range requests {
		if r.Watts > target {
			target = r.Watts
			winningRule = r.Name
		}
	}

	for _, l := range limits {
		target = min(target, l.Watts)
	}

	return max(target, 0), winningRule
}

// currentSolarGeneration returns the current solar generation (5min P66) from solar 1 and 2
func currentSolarGeneration(data DisplayData, config UnifiedInverterConfig) float64 {
	solar1 := data.GetPercentile(config.Solar1PowerTopic, P66, Window5Min)
	solar2 := data.GetPercentile(config.Solar2PowerTopic, P66, Window5Min)
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
// Uses 2.5% hysteresis at each threshold level:
//   - 0 inverters: enters at 12.5%, exits at 15%
//   - max 1 inverter: enters at 17.5%, exits at 20%
//   - max 2 inverters: enters at 22.5%, exits at 25%
//
// currentLimit should be initialized to hardwareMax (unlimited) on startup.
func maxInvertersForSOC(socPercent float64, hardwareMax int, currentLimit *int) int {
	// Determine the limit based on "enter" thresholds (SOC falling)
	var enterLimit int
	switch {
	case socPercent < 12.5:
		enterLimit = 0
	case socPercent < 17.5:
		enterLimit = 1
	case socPercent < 22.5:
		enterLimit = 2
	default:
		enterLimit = hardwareMax
	}

	// Determine the limit based on "exit" thresholds (SOC rising)
	var exitLimit int
	switch {
	case socPercent >= 25:
		exitLimit = hardwareMax
	case socPercent >= 20:
		exitLimit = 2
	case socPercent >= 15:
		exitLimit = 1
	default:
		exitLimit = 0
	}

	// Apply hysteresis: only change if we cross the appropriate threshold
	if *currentLimit > enterLimit {
		// SOC dropped below an enter threshold, reduce limit
		*currentLimit = enterLimit
	} else if *currentLimit < exitLimit {
		// SOC rose above an exit threshold, increase limit
		*currentLimit = exitLimit
	}
	// Otherwise, stay at current limit (in hysteresis band)

	return min(*currentLimit, hardwareMax)
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
