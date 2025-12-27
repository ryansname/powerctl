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
	VoltageTopic     string // From BatteryConfig.BatteryVoltageTopic
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
	CooldownDuration             time.Duration

	// Overflow mode configuration
	OverflowFloatChargeState         string        // "Float Charging"
	OverflowStepInterval             time.Duration // 4 min - time between count changes
	OverflowIncreaseVoltageThreshold float64       // 53.55V - voltage 5min P1 must exceed to increase
	OverflowDecreaseVoltageThreshold float64       // 53.3V - voltage 1min P50 must drop below to decrease
	OverflowFastStartMinVoltage      float64       // 53.0V - voltage must exceed for fast-start
}

// OverflowState tracks per-battery overflow inverter count with step-based changes
type OverflowState struct {
	CurrentCount   int
	LastChangeTime time.Time
}

// Step adjusts count by ±1 based on voltage conditions, rate-limited to one change per interval.
// Step up: Float Charging AND voltage 5min P1 > increaseThreshold
// Step down: voltage 1min P50 < decreaseThreshold
func (s *OverflowState) Step(
	isFloatCharging bool,
	voltage5MinP1 float64,
	voltage1MinP50 float64,
	increaseThreshold float64,
	decreaseThreshold float64,
	maxCount int,
	stepInterval time.Duration,
) int {
	if time.Since(s.LastChangeTime) < stepInterval {
		return s.CurrentCount
	}

	if isFloatCharging && voltage5MinP1 > increaseThreshold && s.CurrentCount < maxCount {
		s.CurrentCount++
		s.LastChangeTime = time.Now()
	} else if voltage1MinP50 < decreaseThreshold && s.CurrentCount > 0 {
		s.CurrentCount--
		s.LastChangeTime = time.Now()
	}

	return s.CurrentCount
}

// InitFromDisplayData initializes overflow state from current inverter states.
// Only triggers on first Float Charging evaluation with voltage > minVoltage (fast-start).
func (s *OverflowState) InitFromDisplayData(
	data DisplayData,
	battery BatteryInverterGroup,
	isFloatCharging bool,
	currentVoltage float64,
	minVoltage float64,
) {
	if !s.LastChangeTime.IsZero() || !isFloatCharging || currentVoltage < minVoltage {
		return
	}
	count := 0
	for _, inv := range battery.Inverters {
		if data.GetBoolean(inv.StateTopic) {
			count++
		}
	}
	s.CurrentCount = count
	s.LastChangeTime = time.Now()
}

// InverterEnablerState holds runtime state for the unified inverter enabler
type InverterEnablerState struct {
	// Last modification time for cooldown
	lastModificationTime time.Time
	// Per-battery lockout state for hysteresis (set when SOC < 12.5%, cleared when > 15%)
	battery2LockedOut bool
	battery3LockedOut bool
	// Per-battery overflow state for rate limiting
	battery2Overflow OverflowState
	battery3Overflow OverflowState
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

// checkBatteryOverflow returns inverter count for overflow mode using step-based control.
// Step up (+1): Float Charging AND voltage 5min P1 > OverflowIncreaseVoltageThreshold
// Step down (-1): voltage 1min P50 < OverflowDecreaseVoltageThreshold
// Changes are rate-limited to one step per OverflowStepInterval.
func checkBatteryOverflow(
	data DisplayData,
	battery BatteryInverterGroup,
	config UnifiedInverterConfig,
	overflow *OverflowState,
) int {
	chargeState := data.GetString(battery.ChargeStateTopic)
	isFloatCharging := strings.Contains(chargeState, config.OverflowFloatChargeState)

	voltageData := data.GetFloat(battery.VoltageTopic)
	voltage5MinP1 := voltageData.P1._5
	voltage1MinP50 := voltageData.P50._1

	overflow.InitFromDisplayData(
		data,
		battery,
		isFloatCharging,
		voltageData.Current,
		config.OverflowFastStartMinVoltage,
	)
	return overflow.Step(
		isFloatCharging,
		voltage5MinP1,
		voltage1MinP50,
		config.OverflowIncreaseVoltageThreshold,
		config.OverflowDecreaseVoltageThreshold,
		len(battery.Inverters),
		config.OverflowStepInterval,
	)
}

// splitOverallCount distributes inverter count between batteries with 50/50 split
// SOC limits are applied during split so count can overflow to the other battery
func splitOverallCount(
	data DisplayData,
	config UnifiedInverterConfig,
	totalCount int,
	state *InverterEnablerState,
) (b2Count, b3Count int) {
	if totalCount <= 0 {
		return 0, 0
	}

	// Calculate SOC-limited maximums
	soc2 := data.GetFloat(config.Battery2.SOCTopic).Current
	soc3 := data.GetFloat(config.Battery3.SOCTopic).Current
	maxB2 := maxInvertersForSOC(soc2, len(config.Battery2.Inverters), &state.battery2LockedOut)
	maxB3 := maxInvertersForSOC(soc3, len(config.Battery3.Inverters), &state.battery3LockedOut)

	// Split 50/50, prefer Battery 3 for odd amounts
	b3Count = min((totalCount+1)/2, maxB3)
	b2Count = min(totalCount-b3Count, maxB2)
	// Handle overflow if B2 hit SOC limit - give remainder to B3
	if b2Count < totalCount-b3Count {
		b3Count = min(totalCount-b2Count, maxB3)
	}

	return b2Count, b3Count
}

// selectMode compares overall mode vs per-battery overflow mode
// and returns whichever produces higher total inverter count
func selectMode(
	data DisplayData,
	config UnifiedInverterConfig,
	state *InverterEnablerState,
) (ModeResult, DebugModeInfo) {
	// Pre-compute values needed by multiple modes
	currentSolar := currentSolarGeneration(data, config)

	// Calculate all requests (each mode handles its own solar logic)
	requests := []PowerRequest{
		maxInverterRequest(data, config),
		powerwallLastRequest(data, config, currentSolar),
		powerwallLowRequest(data, config, currentSolar),
	}
	limits := []PowerLimit{
		powerhouseTransferLimit(data, config),
	}

	targetWatts, winningRule := calculateTargetPower(requests, limits)
	overallCount := calculateInverterCount(targetWatts, config.WattsPerInverter)

	// Calculate per-battery overflow counts
	overflow2Count := checkBatteryOverflow(data, config.Battery2, config, &state.battery2Overflow)
	overflow3Count := checkBatteryOverflow(data, config.Battery3, config, &state.battery3Overflow)
	overflowTotal := overflow2Count + overflow3Count

	// Build debug info (watts for display)
	debug := DebugModeInfo{
		MaxInverter:   requests[0].Watts,
		PowerwallLast: requests[1].Watts,
		PowerwallLow:  requests[2].Watts,
		Overflow2:     float64(overflow2Count) * config.WattsPerInverter,
		Overflow3:     float64(overflow3Count) * config.WattsPerInverter,
	}

	// Compare and select (overflow wins only if > overall)
	if overflowTotal > overallCount {
		var ruleName string
		switch {
		case overflow2Count > 0 && overflow3Count > 0:
			ruleName = "Overflow(B2+B3)"
		case overflow2Count > 0:
			ruleName = "Overflow(B2)"
		default:
			ruleName = "Overflow(B3)"
		}

		debug.Winner = ruleName
		return ModeResult{
			RuleName:      ruleName,
			Battery2Count: overflow2Count,
			Battery3Count: overflow3Count,
		}, debug
	}

	// Overall mode: split into per-battery counts (with SOC limits for overflow handling)
	b2Count, b3Count := splitOverallCount(data, config, overallCount, state)

	debug.Winner = winningRule
	return ModeResult{
		RuleName:      winningRule,
		Battery2Count: b2Count,
		Battery3Count: b3Count,
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
		if strings.Contains(debug.Winner, v.name) ||
			(v.name == "Max Inverter" && debug.Winner == "MaxInverter") ||
			(v.name == "Powerwall Last" && debug.Winner == "PowerwallLast") ||
			(v.name == "Powerwall Low" && debug.Winner == "PowerwallLow") ||
			(v.name == "Overflow (B2)" && strings.Contains(debug.Winner, "B2")) ||
			(v.name == "Overflow (B3)" && strings.Contains(debug.Winner, "B3")) {
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
			// Calculate mode (even during cooldown for debug output)
			modeResult, debugInfo := selectMode(data, config, state)

			// Publish debug output only when it changes
			debugOutput := formatDebugOutput(debugInfo)
			if debugOutput != state.lastDebugOutput {
				sender.SetInputText(
					"input_text.powerhouse_control_debug",
					debugOutput,
				)
				state.lastDebugOutput = debugOutput
			}

			// Check cooldown for inverter changes
			if time.Since(state.lastModificationTime) < config.CooldownDuration {
				continue
			}

			// Apply changes
			changed := applyInverterChanges(data, config, sender, modeResult.Battery2Count, modeResult.Battery3Count)

			if changed {
				state.lastModificationTime = time.Now()
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

// maxInverterRequest returns 10kW if solar conditions are good, else 0
func maxInverterRequest(data DisplayData, config UnifiedInverterConfig) PowerRequest {
	solarForecast := data.GetFloat(config.SolarForecastTopic).Current
	solarPower5MinAvg := data.GetFloat(config.Solar1PowerTopic).P50._5

	watts := 0.0
	if solarForecast > config.MaxInverterModeSolarForecast &&
		solarPower5MinAvg > config.MaxInverterModeSolarPower {
		watts = 100000.0
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
					sender.CallService("switch", "turn_on", inv.EntityID)
				} else {
					log.Printf("Disabling %s (%s)\n", inv.EntityID, b.name)
					sender.CallService("switch", "turn_off", inv.EntityID)
				}
				changed = true
			}
		}
	}

	return changed
}
