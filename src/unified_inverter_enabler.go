package main

import (
	"context"
	"log"
	"math"
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
	OverflowVoltageThreshold float64 // 53.4V
	OverflowFloatChargeState string  // "Float Charging"
	OverflowSolarDivisor     float64 // 3000W (solar 1 p_max)
	OverflowMaxPower         float64 // 4800W (per-battery solar bank p_max)
}

// InverterEnablerState holds runtime state for the unified inverter enabler
type InverterEnablerState struct {
	// Last modification time for cooldown
	lastModificationTime time.Time
	// Per-battery lockout state for hysteresis (set when SOC < 12.5%, cleared when > 15%)
	battery2LockedOut bool
	battery3LockedOut bool
}

// ModeResult represents the outcome of mode selection
// Both modes use per-battery watts for uniform handling
type ModeResult struct {
	RuleName      string
	TotalWatts    float64
	Battery2Watts float64
	Battery3Watts float64
}

// checkBatteryOverflow returns effective watts for overflow mode (0 if not triggered)
// Does NOT apply SOC limits - Float Charging + high voltage implies high SOC anyway
func checkBatteryOverflow(
	data DisplayData,
	battery BatteryInverterGroup,
	solar1Power5MinP50 float64,
	config UnifiedInverterConfig,
) float64 {
	// Check trigger conditions
	chargeState := data.GetString(battery.ChargeStateTopic)
	isFloatCharging := strings.Contains(chargeState, config.OverflowFloatChargeState)

	voltage5MinP50 := data.GetFloat(battery.VoltageTopic).P50._5
	isHighVoltage := voltage5MinP50 > config.OverflowVoltageThreshold

	if !isFloatCharging || !isHighVoltage {
		return 0
	}

	// raw_target = (solar_1_power / 3kW) * 4.8kW
	rawTarget := (solar1Power5MinP50 / config.OverflowSolarDivisor) * config.OverflowMaxPower

	// floor(raw_target / 250W) then cap at hardware max
	inverterCount := int(rawTarget / 250.0)
	inverterCount = min(inverterCount, len(battery.Inverters))

	return float64(inverterCount) * config.WattsPerInverter
}

// splitOverallWatts distributes overall watts between batteries based on priority and SOC limits
// SOC limits are applied during split so watts can overflow to the other battery
func splitOverallWatts(
	data DisplayData,
	config UnifiedInverterConfig,
	totalWatts float64,
	state *InverterEnablerState,
) (b2Watts, b3Watts float64) {
	if totalWatts <= 0 {
		return 0, 0
	}

	battery2Priority := isBatteryPriority(data, config.Battery2)
	battery3Priority := isBatteryPriority(data, config.Battery3)

	// Calculate SOC-limited maximums (in watts)
	soc2 := data.GetFloat(config.Battery2.SOCTopic).Current
	soc3 := data.GetFloat(config.Battery3.SOCTopic).Current
	maxB2Inverters := maxInvertersForSOC(soc2, len(config.Battery2.Inverters), &state.battery2LockedOut)
	maxB3Inverters := maxInvertersForSOC(soc3, len(config.Battery3.Inverters), &state.battery3LockedOut)
	maxB2Watts := float64(maxB2Inverters) * config.WattsPerInverter
	maxB3Watts := float64(maxB3Inverters) * config.WattsPerInverter

	switch {
	case battery2Priority && !battery3Priority:
		b2Watts = min(totalWatts, maxB2Watts)
		b3Watts = min(totalWatts-b2Watts, maxB3Watts)
	case battery3Priority && !battery2Priority:
		b3Watts = min(totalWatts, maxB3Watts)
		b2Watts = min(totalWatts-b3Watts, maxB2Watts)
	default:
		// Split 50/50, prefer Battery 3 for odd amounts
		b3Watts = min((totalWatts+config.WattsPerInverter)/2, maxB3Watts)
		b2Watts = min(totalWatts-b3Watts, maxB2Watts)
		// Handle overflow if one battery hit SOC limit
		if b3Watts >= maxB3Watts {
			b2Watts = min(totalWatts-b3Watts, maxB2Watts)
		}
		if b2Watts >= maxB2Watts {
			b3Watts = min(totalWatts-b2Watts, maxB3Watts)
		}
	}

	return b2Watts, b3Watts
}

// selectMode compares overall mode vs per-battery overflow mode
// and returns whichever produces higher total power output
func selectMode(data DisplayData, config UnifiedInverterConfig, state *InverterEnablerState) ModeResult {
	// Calculate overall mode result
	targetWatts, winningRule := calculateTargetPower(data, config)
	solarWatts := currentSolarGeneration(data, config)
	overallInverterWatts := max(targetWatts-solarWatts, 0)
	overallInverterCount := calculateInverterCount(overallInverterWatts, config.WattsPerInverter)
	overallEffective := float64(overallInverterCount) * config.WattsPerInverter

	// Calculate per-battery overflow (0 = not triggered)
	solar1Power5MinP50 := data.GetFloat(config.Solar1PowerTopic).P50._5
	overflow2Watts := checkBatteryOverflow(data, config.Battery2, solar1Power5MinP50, config)
	overflow3Watts := checkBatteryOverflow(data, config.Battery3, solar1Power5MinP50, config)

	perBatteryEffective := overflow2Watts + overflow3Watts

	// Compare and select (per-battery wins only if > overall)
	if perBatteryEffective > overallEffective {
		var ruleName string
		switch {
		case overflow2Watts > 0 && overflow3Watts > 0:
			ruleName = "Overflow(B2+B3)"
		case overflow2Watts > 0:
			ruleName = "Overflow(B2)"
		default:
			ruleName = "Overflow(B3)"
		}

		return ModeResult{
			RuleName:      ruleName,
			TotalWatts:    perBatteryEffective,
			Battery2Watts: overflow2Watts,
			Battery3Watts: overflow3Watts,
		}
	}

	// Overall mode: split into per-battery watts (with SOC limits for overflow handling)
	b2Watts, b3Watts := splitOverallWatts(data, config, overallEffective, state)

	return ModeResult{
		RuleName:      winningRule,
		TotalWatts:    overallEffective,
		Battery2Watts: b2Watts,
		Battery3Watts: b3Watts,
	}
}

// wattsToInverterCount converts watts to inverter count
func wattsToInverterCount(watts, wattsPerInverter float64) int {
	if watts <= 0 {
		return 0
	}
	return int(watts / wattsPerInverter)
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
			// Check cooldown
			if time.Since(state.lastModificationTime) < config.CooldownDuration {
				continue
			}

			// Select mode and get per-battery watts (SOC limits already applied for overall mode)
			modeResult := selectMode(data, config, state)

			// Convert watts to counts
			battery2Count := wattsToInverterCount(modeResult.Battery2Watts, config.WattsPerInverter)
			battery3Count := wattsToInverterCount(modeResult.Battery3Watts, config.WattsPerInverter)

			// Apply changes
			changed := applyInverterChanges(data, config, sender, battery2Count, battery3Count)

			if changed {
				state.lastModificationTime = time.Now()
				log.Printf("Unified inverter enabler: rule=%s, watts=%.0fW, B2=%d, B3=%d\n",
					modeResult.RuleName, modeResult.TotalWatts, battery2Count, battery3Count)
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

// powerwallLastRequest returns 2/3 of the 15min P66 load power
func powerwallLastRequest(data DisplayData, config UnifiedInverterConfig) PowerRequest {
	loadPower15MinP66 := data.GetFloat(config.LoadPowerTopic).P66._15
	return PowerRequest{Name: "PowerwallLast", Watts: loadPower15MinP66 * (2.0 / 3.0)}
}

// powerwallLowRequest returns 15min P99 load if powerwall SOC is low, else 0
func powerwallLowRequest(data DisplayData, config UnifiedInverterConfig) PowerRequest {
	powerwallSOC15MinP1 := data.GetFloat(config.PowerwallSOCTopic).P1._15

	watts := 0.0
	if powerwallSOC15MinP1 < config.PowerwallLowThreshold {
		watts = data.GetFloat(config.LoadPowerTopic).P99._15
	}
	return PowerRequest{Name: "PowerwallLow", Watts: watts}
}

// powerhouseTransferLimit returns the available capacity after accounting for solar generation
func powerhouseTransferLimit(data DisplayData, config UnifiedInverterConfig) PowerLimit {
	solar1Power15MinP99 := data.GetFloat(config.Solar1PowerTopic).P99._15
	availableCapacity := config.MaxTransferPower - solar1Power15MinP99
	return PowerLimit{Name: "PowerhouseTransfer", Watts: availableCapacity}
}

// calculateTargetPower computes target watts by taking max of all requests and applying all limits
func calculateTargetPower(data DisplayData, config UnifiedInverterConfig) (float64, string) {
	// Calculate all requests, take max
	requests := []PowerRequest{
		maxInverterRequest(data, config),
		powerwallLastRequest(data, config),
		powerwallLowRequest(data, config),
	}

	target := 0.0
	winningRule := ""
	for _, r := range requests {
		if r.Watts > target {
			target = r.Watts
			winningRule = r.Name
		}
	}

	// Calculate all limits, apply each
	limits := []PowerLimit{
		powerhouseTransferLimit(data, config),
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

// isBatteryPriority checks if battery is in Float Charging state with > 95% SOC
func isBatteryPriority(data DisplayData, battery BatteryInverterGroup) bool {
	chargeState := data.GetString(battery.ChargeStateTopic)
	isFloatCharging := chargeState == "Float Charging"

	socPercent := data.GetFloat(battery.SOCTopic).Current
	isHighSOC := socPercent > 95.0

	return isFloatCharging && isHighSOC
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
