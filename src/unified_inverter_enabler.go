package main

import (
	"context"
	"fmt"
	"log"
	"math"
	"sort"
	"strings"
)

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
	Name             string
	Inverters        []InverterInfo
	ChargeStateTopic string
	SOCTopic         string
}

// UnifiedInverterConfig holds configuration for the unified inverter enabler
type UnifiedInverterConfig struct {
	Battery2 BatteryInverterGroup
	Battery3 BatteryInverterGroup

	// Topics for mode selection and target calculation
	SolarForecastTopic string
	Solar1PowerTopic   string
	Solar2PowerTopic   string
	LoadPowerTopic     string
	PowerwallSOCTopic  string

	// Constants
	WattsPerInverter             float64
	MaxTransferPower             float64
	MaxInverterModeSolarForecast float64
	MaxInverterModeSolarPower    float64
	PowerwallLowThreshold        float64

	// Overflow mode configuration (SOC-based hysteresis)
	OverflowFloatChargeState string  // "Float Charging"
	OverflowSOCTurnOffStart  float64 // 98.5% - first inverter turns off when SOC drops below
	OverflowSOCTurnOffEnd    float64 // 95.0% - last inverter turns off when SOC drops below
	OverflowSOCTurnOnStart   float64 // 95.75% - first inverter turns on when SOC rises above
	OverflowSOCTurnOnEnd     float64 // 99.5% - last inverter turns on when SOC rises above
}

// InverterEnablerState holds runtime state for the unified inverter enabler
type InverterEnablerState struct {
	// Per-battery lockout state for hysteresis (set when SOC < 12.5%, cleared when > 15%)
	battery2LockedOut bool
	battery3LockedOut bool
	// Last published debug output for change detection
	lastDebugOutput string
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
	MaxInverter   float64
	PowerwallLast float64
	PowerwallLow  float64
	Overflow2     float64
	Overflow3     float64
	Winner        string
}

// checkBatteryOverflow returns inverter count for overflow mode using SOC-based hysteresis.
// If not Float Charging, returns 0.
// Otherwise, uses separate turn-on and turn-off thresholds to prevent oscillation.
func checkBatteryOverflow(
	data DisplayData,
	battery BatteryInverterGroup,
	config UnifiedInverterConfig,
) int {
	chargeState := data.GetString(battery.ChargeStateTopic)
	isFloatCharging := strings.Contains(chargeState, config.OverflowFloatChargeState)

	if !isFloatCharging {
		return 0
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
	if currentCount > offCount {
		return offCount // SOC dropped, reduce
	}
	if currentCount < onCount {
		return onCount // SOC rose, increase
	}
	return currentCount // Stay in hysteresis zone
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

// overflowRuleName returns the rule name for overflow mode based on which batteries contribute
func overflowRuleName(b2Count, b3Count int) string {
	switch {
	case b2Count > 0 && b3Count > 0:
		return "Overflow(B2+B3)"
	case b2Count > 0:
		return "Overflow(B2)"
	case b3Count > 0:
		return "Overflow(B3)"
	default:
		return ""
	}
}

// selectMode calculates per-battery overflow and global targets, applying limits,
// and returns whichever produces higher total inverter count.
// If global is higher, it starts from limited overflow base and round-robins additional inverters.
func selectMode(
	data DisplayData,
	config UnifiedInverterConfig,
	state *InverterEnablerState,
) (ModeResult, DebugModeInfo) {
	// 1. Calculate per-battery overflow counts (SOC-based hysteresis)
	overflow2 := checkBatteryOverflow(data, config.Battery2, config)
	overflow3 := checkBatteryOverflow(data, config.Battery3, config)

	// 2. Apply global limit to overflow (PowerhouseTransfer limit)
	limit := powerhouseTransferLimit(data, config)
	limitedB2, limitedB3 := applyLimitToPerBattery(overflow2, overflow3, limit.Watts, config.WattsPerInverter)
	limitedOverflowTotal := limitedB2 + limitedB3

	// 3. Calculate global targets (already includes limits)
	currentSolar := currentSolarGeneration(data, config)
	requests := []PowerRequest{
		maxInverterRequest(data, config),
		powerwallLastRequest(data, config, currentSolar),
		powerwallLowRequest(data, config, currentSolar),
	}
	limits := []PowerLimit{limit}
	targetWatts, winningRule := calculateTargetPower(requests, limits)
	globalCount := calculateInverterCount(targetWatts, config.WattsPerInverter)

	// Build debug info (before limiting for display)
	debug := DebugModeInfo{
		MaxInverter:   requests[0].Watts,
		PowerwallLast: requests[1].Watts,
		PowerwallLow:  requests[2].Watts,
		Overflow2:     float64(overflow2) * config.WattsPerInverter,
		Overflow3:     float64(overflow3) * config.WattsPerInverter,
	}

	// 4. Compare and select
	if globalCount > limitedOverflowTotal {
		// Global target is higher: round-robin from limited overflow base
		soc2 := data.GetFloat(config.Battery2.SOCTopic).Current
		soc3 := data.GetFloat(config.Battery3.SOCTopic).Current
		maxB2 := maxInvertersForSOC(soc2, len(config.Battery2.Inverters), &state.battery2LockedOut)
		maxB3 := maxInvertersForSOC(soc3, len(config.Battery3.Inverters), &state.battery3LockedOut)
		b2, b3 := roundRobinFromBase(limitedB2, limitedB3, globalCount, maxB2, maxB3)

		// Winner is the global rule (only if non-zero)
		if targetWatts > 0 {
			debug.Winner = winningRule
		}
		return ModeResult{RuleName: winningRule, Battery2Count: b2, Battery3Count: b3}, debug
	}

	// Per-battery overflow wins (or tie)
	if limitedOverflowTotal > 0 {
		debug.Winner = overflowRuleName(limitedB2, limitedB3)
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
		{"Max Inverter", debug.MaxInverter},
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
		isWinner := strings.Contains(debug.Winner, v.name) ||
			(v.name == "Max Inverter" && debug.Winner == "MaxInverter") ||
			(v.name == "Powerwall Last" && debug.Winner == "PowerwallLast") ||
			(v.name == "Powerwall Low" && debug.Winner == "PowerwallLow") ||
			(v.name == "Overflow (B2)" && strings.Contains(debug.Winner, "B2")) ||
			(v.name == "Overflow (B3)" && strings.Contains(debug.Winner, "B3"))
		if isWinner && v.watts > 0 {
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

	state := &InverterEnablerState{}

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

// maxInverterRequest returns total inverter wattage if solar conditions are good, else 0
func maxInverterRequest(data DisplayData, config UnifiedInverterConfig) PowerRequest {
	solarForecast := data.GetFloat(config.SolarForecastTopic).Current
	solarPower5MinAvg := data.GetFloat(config.Solar1PowerTopic).P50._5

	watts := 0.0
	if solarForecast > config.MaxInverterModeSolarForecast &&
		solarPower5MinAvg > config.MaxInverterModeSolarPower {
		watts = float64(len(config.Battery2.Inverters)+len(config.Battery3.Inverters)) * config.WattsPerInverter
	}
	return PowerRequest{Name: "MaxInverter", Watts: watts}
}

// powerwallLastRequest returns 2/3 of the 15min P66 load power, minus current solar generation
func powerwallLastRequest(
	data DisplayData,
	config UnifiedInverterConfig,
	currentSolar float64,
) PowerRequest {
	loadPower15MinP66 := data.GetFloat(config.LoadPowerTopic).P66._15
	targetLoad := loadPower15MinP66 * (2.0 / 3.0)
	return PowerRequest{Name: "PowerwallLast", Watts: max(targetLoad-currentSolar, 0)}
}

// powerwallLowRequest returns 15min P99 load minus current solar if powerwall SOC is low, else 0
func powerwallLowRequest(
	data DisplayData,
	config UnifiedInverterConfig,
	currentSolar float64,
) PowerRequest {
	powerwallSOC15MinP1 := data.GetFloat(config.PowerwallSOCTopic).P1._15

	if powerwallSOC15MinP1 >= config.PowerwallLowThreshold {
		return PowerRequest{Name: "PowerwallLow", Watts: 0}
	}

	loadPower := data.GetFloat(config.LoadPowerTopic).P99._15
	return PowerRequest{Name: "PowerwallLow", Watts: max(loadPower-currentSolar, 0)}
}

// powerhouseTransferLimit returns the available capacity after accounting for solar generation
func powerhouseTransferLimit(data DisplayData, config UnifiedInverterConfig) PowerLimit {
	solar1Power15MinP99 := data.GetFloat(config.Solar1PowerTopic).P99._15
	availableCapacity := config.MaxTransferPower - solar1Power15MinP99
	return PowerLimit{Name: "PowerhouseTransfer", Watts: availableCapacity}
}

// calculateTargetPower computes target watts by taking max of all requests and applying all limits
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
	solar1 := data.GetFloat(config.Solar1PowerTopic).P66._5
	solar2 := data.GetFloat(config.Solar2PowerTopic).P66._5
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

// maxInvertersForSOC returns the max inverters allowed based on SOC percentage
// Uses hysteresis: once SOC drops below 12.5% (lockout), no inverters until SOC > 15%
func maxInvertersForSOC(socPercent float64, hardwareMax int, lockedOut *bool) int {
	// Check for unlock condition first
	if *lockedOut && socPercent > 15.0 {
		*lockedOut = false
	}

	// If locked out, no inverters allowed
	if *lockedOut {
		return 0
	}

	switch {
	case socPercent < 12.5:
		*lockedOut = true
		return 0
	case socPercent < 17.5:
		return min(1, hardwareMax)
	case socPercent < 25:
		return min(2, hardwareMax)
	default:
		return hardwareMax
	}
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
