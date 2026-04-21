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
	ShortName            string // Short name for display (e.g., "B2")
	Inverters            []InverterInfo
	ChargeStateTopic     string
	SOCTopic             string
	BatteryVoltageTopic  string  // Topic for battery voltage (used by low voltage safety)
	CapacityWh           float64 // Battery capacity in Wh (e.g., 10000 for 10 kWh)
	SolarMultiplier      float64 // Multiplier for solar forecast (e.g., 4.5)
	AvailableEnergyTopic string  // Topic for battery available energy
}

// UnifiedInverterConfig holds configuration for the unified inverter enabler
type UnifiedInverterConfig struct {
	Battery2 BatteryInverterGroup

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
	OverflowSOCTurnOffStart float64 // 98.5% - first inverter turns off when SOC drops below
	OverflowSOCTurnOffEnd   float64 // 95.0% - last inverter turns off when SOC drops below
	OverflowSOCTurnOnStart  float64 // 95.75% - first inverter turns on when SOC rises above
	OverflowSOCTurnOnEnd    float64 // 99.5% - last inverter turns on when SOC rises above

	// Low voltage safety (per-battery, applies to 15-min P1 voltage)
	LowVoltageTripThreshold  float64 // e.g. 50.75V - latch battery inverters off below this
	LowVoltageResetThreshold float64 // e.g. 52.00V - release once 15-min P1 recovers above this
}

// InverterEnablerState holds runtime state for the unified inverter enabler
type InverterEnablerState struct {
	// Last published debug output for change detection
	lastDebugOutput string
	// Forecast excess state for Battery 2
	forecastExcess2 governor.ForecastExcessState

	// Rolling min/max windows for Powerwall Last mode (1-hour, 1-minute buckets)
	loadWindow  governor.RollingMinMax
	solarWindow governor.RollingMinMax

	// Combined solar max for grid-off per-battery mode gating
	gridOffSolarMax governor.RollingMinMax

	// Battery 2 rolling minimum voltage for low voltage safety (1-hour window)
	battery2VoltageMin governor.RollingMinMax

	// Overflow state for Battery 2 (decrease-only to prevent flapping)
	lastOverflow2Watts float64
	overflow2InFloat   bool

	// Stepped hysteresis controllers
	powerwallLow   *governor.SteppedHysteresis // Powerwall Low mode (9 inverters)
	overflow2      *governor.SteppedHysteresis // Overflow mode for Battery 2
	socLimit2      *governor.SteppedHysteresis // SOC-based inverter limit for Battery 2
	powerCutAllow2 *governor.SteppedHysteresis // Expecting power cuts: allow inverters above ~50% SOC
	lowVoltage2    *governor.SteppedHysteresis // Low voltage safety latch for Battery 2
}

// ModeResult represents the outcome of mode selection (in inverter counts)
type ModeResult struct {
	Battery2Count int
}

// TotalCount returns the total number of inverters
func (m ModeResult) TotalCount() int {
	return m.Battery2Count
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

	// Battery 2 low voltage safety state
	Battery2LowVoltage bool
	Battery2VoltageMin float64 // 15 min rolling minimum voltage for B2
}

// checkBatteryOverflow returns inverter count for overflow mode using SOC-based hysteresis.
// Requires Float Charging + 100% SOC to enter. Once entered, stays active while in Float
// (even as SOC drops). Watts can only decrease to prevent inverter flapping.
func checkBatteryOverflow(
	data DisplayData,
	battery BatteryInverterGroup,
	config UnifiedInverterConfig,
	state *InverterEnablerState,
) PowerRequest {
	name := "Overflow (" + battery.ShortName + ")"

	chargeState := data.GetString(battery.ChargeStateTopic)
	inFloat := chargeState == "Float Charging"
	soc := data.GetFloat(battery.SOCTopic).Current

	if !inFloat {
		state.overflow2InFloat = false
		return PowerRequest{Name: name, Watts: 0}
	}

	if !state.overflow2InFloat && soc < 100 {
		return PowerRequest{Name: name, Watts: 0}
	}

	count := state.overflow2.Update(soc)
	watts := float64(count) * config.WattsPerInverter

	if !state.overflow2InFloat {
		state.overflow2InFloat = true
		state.lastOverflow2Watts = watts
	} else {
		watts = min(watts, state.lastOverflow2Watts)
		state.lastOverflow2Watts = watts
	}

	return PowerRequest{Name: name, Watts: watts}
}

// selectMode calculates per-battery overflow/forecast excess and global targets, applying limits,
// and returns whichever produces higher total inverter count.
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

	// 1. Per-battery overflow (SOC-based hysteresis)
	overflow2 := checkBatteryOverflow(data, config.Battery2, config, state)

	// 2. Per-battery forecast excess (already capped at max inverter power)
	forecastExcess2 := forecastExcessRequest(data, config, config.Battery2, &state.forecastExcess2)

	// Grid off: disable per-battery modes only when solar is high (>= 3kW max over 1hr).
	// When solar is low, allow overflow/forecast excess to prevent wasting energy on borderline days.
	// Global modes (Powerwall Last/Low) always work during outages regardless.
	if !gridAvailable {
		combinedSolar := data.GetFloat(config.Solar1PowerTopic).Current +
			data.GetFloat(config.Solar2PowerTopic).Current
		state.gridOffSolarMax.Update(combinedSolar)
		if state.gridOffSolarMax.Max() >= 3000 {
			overflow2.Watts = 0
			forecastExcess2.Watts = 0
		}
	}

	// 3. Take max of overflow and forecast excess
	perBattery2 := maxPowerRequest(overflow2, forecastExcess2)
	perBattery2Count := calculateInverterCount(perBattery2.Watts, config.WattsPerInverter)

	// 3.5. Apply SOC-based limit
	soc2 := data.GetFloat(config.Battery2.SOCTopic).Current
	maxB2 := maxInvertersForSOC(soc2, state.socLimit2)
	perBattery2Count = min(perBattery2Count, maxB2)

	// 4. Apply global limit (PowerhouseTransfer limit)
	limit := powerhouseTransferLimit(data, config)
	limitedB2 := min(perBattery2Count, int(limit.Watts/config.WattsPerInverter))
	if limitedB2 < 0 {
		limitedB2 = 0
	}

	// 5. Calculate global targets (Powerwall modes)
	requests := []PowerRequest{
		powerwallLastRequest(state),
		checkPowerwallLow(data, config, state),
	}
	limits := []PowerLimit{limit}
	targetWatts, globalModes := calculateTargetPower(requests, limits)
	globalCount := calculateInverterCount(targetWatts, config.WattsPerInverter)

	// 6. Compare and select
	globalWins := globalCount > limitedB2 && targetWatts > 0

	// Clear global mode contributions if global doesn't win
	if !globalWins {
		for i := range globalModes {
			globalModes[i].Contributing = false
		}
	}

	// Build debug info with contributing flags
	debug := DebugModeInfo{
		PowerwallSOC: powerwallSOC,
		Modes: []ModeState{
			{Name: "Forecast Excess (B2)", Watts: forecastExcess2.Watts, Contributing: limitedB2 > 0 && perBattery2.Name == forecastExcess2.Name},
			globalModes[0],
			globalModes[1],
			{Name: "Overflow (B2)", Watts: overflow2.Watts, Contributing: limitedB2 > 0 && perBattery2.Name == overflow2.Name},
		},
	}

	if globalWins {
		return ModeResult{Battery2Count: min(globalCount, maxB2)}, debug
	}

	return ModeResult{Battery2Count: limitedB2}, debug
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

	// Append a low-voltage row if battery is currently latched off
	if debug.Battery2LowVoltage {
		fmt.Fprintf(&sb, "| Low Voltage (B2) | %.2fV | ⚠ |\n", debug.Battery2VoltageMin)
	}

	return sb.String()
}

// unifiedInverterEnabler manages all inverters on Battery 2
func unifiedInverterEnabler(
	ctx context.Context,
	dataChan <-chan DisplayData,
	config UnifiedInverterConfig,
	sender *MQTTSender,
) {
	log.Println("Unified inverter enabler started")

	b2Inverters := len(config.Battery2.Inverters)

	state := &InverterEnablerState{
		// Rolling min/max windows for Powerwall Last mode
		loadWindow:         governor.NewRollingMinMax(60),
		solarWindow:        governor.NewRollingMinMax(60),
		gridOffSolarMax:    governor.NewRollingMinMax(60),
		battery2VoltageMin: governor.NewRollingMinMax(15),
		// Powerwall Low: descending (SOC↓ → inverters↑), 9 steps
		powerwallLow: governor.NewSteppedHysteresis(
			9, false,
			config.PowerwallLowSOCTurnOnStart, config.PowerwallLowSOCTurnOnEnd,
			config.PowerwallLowSOCTurnOffStart, config.PowerwallLowSOCTurnOffEnd,
		),
		// Overflow: ascending (SOC↑ → inverters↑)
		overflow2: governor.NewSteppedHysteresis(
			b2Inverters, true,
			config.OverflowSOCTurnOnStart, config.OverflowSOCTurnOnEnd,
			config.OverflowSOCTurnOffStart, config.OverflowSOCTurnOffEnd,
		),
		// SOC Limits: ascending (SOC↑ → limit↑), steps = inverter count
		// Enter thresholds: 15% → 25% (ascending)
		// Exit thresholds: 12.5% → 22.5% (ascending)
		socLimit2: governor.NewSteppedHysteresis(b2Inverters, true, 15, 25, 12.5, 22.5),
		// Expecting power cuts: 1-step ascending, allow at 53%, block at 47%
		powerCutAllow2: governor.NewSteppedHysteresis(1, true, 53, 53, 47, 47),
		// Low voltage safety: 1-step descending. Trip when 15-min P1 voltage drops
		// below TripThreshold, release once it has recovered above ResetThreshold.
		lowVoltage2: governor.NewSteppedHysteresis(
			1, false,
			config.LowVoltageTripThreshold, config.LowVoltageTripThreshold,
			config.LowVoltageResetThreshold, config.LowVoltageResetThreshold,
		),
	}
	// Initialize SOC limit to max (all inverters allowed)
	state.socLimit2.Current = b2Inverters

	for {
		select {
		case data := <-dataChan:
			// Update rolling windows for Powerwall Last mode
			state.loadWindow.Update(data.GetFloat(config.LoadPowerTopic).Current)
			solar := data.GetFloat(config.Solar1PowerTopic).Current + data.GetFloat(config.Solar2PowerTopic).Current
			state.solarWindow.Update(solar)

			modeResult, debugInfo := selectMode(data, config, state)

			// Low voltage safety: per-battery latch using a 1-hour rolling minimum.
			// When the observed minimum drops below the trip threshold, force the
			// battery's inverters off until the minimum recovers above the reset
			// threshold. Applies regardless of other modes and runs before the
			// power-cut conservation block so a voltage trip always wins.
			state.battery2VoltageMin.Update(data.GetFloat(config.Battery2.BatteryVoltageTopic).Current)
			b2VoltageMin := state.battery2VoltageMin.Min()
			prevB2Latched := state.lowVoltage2.Current > 0
			b2LowVoltage := state.lowVoltage2.Update(b2VoltageMin) > 0
			if b2LowVoltage != prevB2Latched {
				if b2LowVoltage {
					log.Printf("Battery 2: LOW VOLTAGE TRIP (1h min %.2fV < %.2fV) - forcing inverters off\n",
						b2VoltageMin, config.LowVoltageTripThreshold)
				} else {
					log.Printf("Battery 2: low voltage cleared (1h min %.2fV >= %.2fV)\n",
						b2VoltageMin, config.LowVoltageResetThreshold)
				}
			}
			if b2LowVoltage {
				modeResult.Battery2Count = 0
			}
			debugInfo.Battery2LowVoltage = b2LowVoltage
			debugInfo.Battery2VoltageMin = b2VoltageMin

			// Expecting power cuts: block discharge around 50% SOC (hysteresis: 47-53%)
			// Only when grid is on -- no point conserving energy during an actual outage
			conservingForPowerCut := data.GetBoolean(TopicExpectingPowerCutsState) && data.GetBoolean(config.GridStatusTopic)
			if conservingForPowerCut {
				if state.powerCutAllow2.Update(data.GetFloat(config.Battery2.SOCTopic).Current) == 0 {
					modeResult.Battery2Count = 0
				}
				if modeResult.TotalCount() == 0 {
					debugInfo.SafetyReason = "Expecting power cuts (battery < 50%)"
				}
			}

			// Publish debug output only when it changes
			debugOutput := formatDebugOutput(debugInfo)
			if debugOutput != state.lastDebugOutput {
				sender.CallService("input_text", "set_value", "input_text.powerhouse_control_debug", map[string]any{"value": debugOutput})
				state.lastDebugOutput = debugOutput
			}

			// Publish forecast excess debug sensors
			sender.PublishDebugSensor("powerctl_b2_expected_solar", state.forecastExcess2.DebugExpectedSolarWh)
			sender.PublishDebugSensor("powerctl_b2_excess", state.forecastExcess2.DebugExcessWh)

			// Publish Powerwall Last and Low debug sensors
			sender.PublishDebugSensor("powerctl_powerwall_last", state.loadWindow.Min()-state.solarWindow.Max())
			sender.PublishDebugSensor("powerctl_powerwall_low_count", float64(state.powerwallLow.Current))

			// Apply changes
			changed := applyInverterChanges(data, config, sender, modeResult.Battery2Count)

			if changed {
				totalWatts := float64(modeResult.TotalCount()) * config.WattsPerInverter
				log.Printf("Unified inverter enabler: watts=%.0fW, B2=%d\n",
					totalWatts, modeResult.Battery2Count)
			}

		case <-ctx.Done():
			log.Println("Unified inverter enabler stopped")
			return
		}
	}
}

// forecastExcessRequest calculates forecast excess inverter power for a single battery.
// Extracts data from DisplayData and delegates to governor.ForecastExcessRequestCore.
func forecastExcessRequest(
	data DisplayData,
	config UnifiedInverterConfig,
	battery BatteryInverterGroup,
	state *governor.ForecastExcessState,
) PowerRequest {
	input := governor.ForecastExcessInput{
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
	result := governor.ForecastExcessRequestCore(input, state)
	return PowerRequest{Name: result.Name, Watts: result.Watts}
}

// powerwallLastRequest returns the gap between minimum house load and maximum solar generation.
// Uses 1-hour rolling min/max windows for natural smoothing.
func powerwallLastRequest(state *InverterEnablerState) PowerRequest {
	return PowerRequest{
		Name:  "PowerwallLast",
		Watts: min(state.loadWindow.Min()-state.solarWindow.Max(), 500),
	}
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
	solar1Power15MinP90 := data.GetPercentile(config.Solar1PowerTopic, P90, Window15Min)
	availableCapacity := config.MaxTransferPower - solar1Power15MinP90
	return PowerLimit{Name: "PowerhouseTransfer", Watts: availableCapacity}
}

// maxPowerRequest returns the PowerRequest with the highest watts
func maxPowerRequest(a, b PowerRequest) PowerRequest {
	if a.Watts >= b.Watts {
		return a
	}
	return b
}

// calculateTargetPower computes target watts by taking max of all requests and applying all limits
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

// applyInverterChanges enables/disables inverters to match the desired count
func applyInverterChanges(
	data DisplayData,
	config UnifiedInverterConfig,
	sender *MQTTSender,
	battery2Count int,
) bool {
	changed := false

	for i, inv := range config.Battery2.Inverters {
		current := data.GetBoolean(inv.StateTopic)
		desired := i < battery2Count

		if current != desired {
			if desired {
				log.Printf("Enabling %s (Battery 2)\n", inv.EntityID)
				sender.CallService("switch", "turn_on", inv.EntityID, nil)
			} else {
				log.Printf("Disabling %s (Battery 2)\n", inv.EntityID)
				sender.CallService("switch", "turn_off", inv.EntityID, nil)
			}
			changed = true
		}
	}

	return changed
}
