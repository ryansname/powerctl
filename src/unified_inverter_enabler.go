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

const floatChargingState = "Float Charging"

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

// BatteryOverflowState holds per-battery runtime state for overflow mode.
type BatteryOverflowState struct {
	LastWatts  float64
	InFloat    bool
	Hysteresis *governor.SteppedHysteresis
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

	// Overflow state for Battery 2
	overflow2State BatteryOverflowState

	// Stepped hysteresis controllers
	powerwallLow   *governor.SteppedHysteresis // Powerwall Low mode (9 inverters)
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
	chargeState string,
	soc float64,
	shortName string,
	wattsPerInverter float64,
	state *BatteryOverflowState,
) PowerRequest {
	name := "Overflow (" + shortName + ")"
	inFloat := chargeState == floatChargingState

	if !inFloat {
		state.InFloat = false
		return PowerRequest{Name: name, Watts: 0}
	}

	if !state.InFloat && soc < 100 {
		return PowerRequest{Name: name, Watts: 0}
	}

	count := state.Hysteresis.Update(soc)
	watts := float64(count) * wattsPerInverter

	if !state.InFloat {
		state.InFloat = true
		state.LastWatts = watts
	} else {
		watts = min(watts, state.LastWatts)
		state.LastWatts = watts
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
	overflow2 := checkBatteryOverflow(
		data.GetString(config.Battery2.ChargeStateTopic),
		data.GetFloat(config.Battery2.SOCTopic).Current,
		config.Battery2.ShortName,
		config.WattsPerInverter,
		&state.overflow2State,
	)

	// 2. Per-battery forecast excess (already capped at max inverter power)
	var forecast2 governor.ForecastPeriods
	data.GetJSON(config.DetailedForecastTopic, &forecast2)
	forecastExcess2 := forecastExcessRequest(
		data.GetFloat(config.SolarForecastRemainingTopic).Current,
		forecast2,
		data.GetFloat(config.Battery2.AvailableEnergyTopic).Current,
		config.WattsPerInverter,
		config.Battery2,
		&state.forecastExcess2,
	)

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
	limit := powerhouseTransferLimit(data.GetPercentile(config.Solar1PowerTopic, P90, Window15Min), config.MaxTransferPower)
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
		overflow2State: BatteryOverflowState{
			Hysteresis: governor.NewSteppedHysteresis(
				b2Inverters, true,
				config.OverflowSOCTurnOnStart, config.OverflowSOCTurnOnEnd,
				config.OverflowSOCTurnOffStart, config.OverflowSOCTurnOffEnd,
			),
		},
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
			inverterStates := make([]bool, len(config.Battery2.Inverters))
			for i, inv := range config.Battery2.Inverters {
				inverterStates[i] = data.GetBoolean(inv.StateTopic)
			}
			changed := applyInverterChanges(inverterStates, config.Battery2.Inverters, sender, modeResult.Battery2Count)

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
func forecastExcessRequest(
	forecastRemainingWh float64,
	forecast governor.ForecastPeriods,
	availableWh float64,
	wattsPerInverter float64,
	battery BatteryInverterGroup,
	state *governor.ForecastExcessState,
) PowerRequest {
	input := governor.ForecastExcessInput{
		Now:                 time.Now(),
		ForecastRemainingWh: forecastRemainingWh,
		Forecast:            forecast,
		AvailableWh:         availableWh,
		InverterCount:       len(battery.Inverters),
		WattsPerInverter:    wattsPerInverter,
		SolarMultiplier:     battery.SolarMultiplier,
		CapacityWh:          battery.CapacityWh,
		ShortName:           battery.ShortName,
	}
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
func powerhouseTransferLimit(solar1P90_15Min float64, maxTransferPower float64) PowerLimit {
	return PowerLimit{Name: "PowerhouseTransfer", Watts: maxTransferPower - solar1P90_15Min}
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

// applyInverterChanges enables/disables inverters to match the desired count.
// currentStates must be indexed parallel to inverters.
func applyInverterChanges(
	currentStates []bool,
	inverters []InverterInfo,
	sender *MQTTSender,
	desiredCount int,
) bool {
	changed := false

	for i, inv := range inverters {
		current := i < len(currentStates) && currentStates[i]
		desired := i < desiredCount

		if current != desired {
			if desired {
				log.Printf("Enabling %s\n", inv.EntityID)
				sender.CallService("switch", "turn_on", inv.EntityID, nil)
			} else {
				log.Printf("Disabling %s\n", inv.EntityID)
				sender.CallService("switch", "turn_off", inv.EntityID, nil)
			}
			changed = true
		}
	}

	return changed
}
