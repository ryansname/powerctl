package main

import (
	"context"
	"log"
	"math"
	"time"
)

// OperatingMode determines how target power is calculated
type OperatingMode int

const (
	PowerwallLastMode OperatingMode = iota
	PowerwallLowMode
	MaxInverterMode
)

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
	LoadPowerTopic     string
	PowerwallSOCTopic  string

	// Constants
	WattsPerInverter             float64
	MaxTransferPower             float64
	MaxInverterModeSolarForecast float64
	MaxInverterModeSolarPower    float64
	PowerwallLowThreshold        float64
	CooldownDuration             time.Duration
}

// InverterEnablerState holds runtime state for the unified inverter enabler
type InverterEnablerState struct {
	// Last modification time for cooldown
	lastModificationTime time.Time
	// Per-battery lockout state for hysteresis (set when SOC < 12.5%, cleared when > 15%)
	battery2LockedOut bool
	battery3LockedOut bool
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

			// Determine operating mode
			mode := determineMode(data, config)

			// Calculate target power (with limit applied)
			targetWatts := calculateTargetPower(data, config, mode)

			// Calculate desired inverter count
			desiredCount := calculateInverterCount(targetWatts, config.WattsPerInverter)

			// Determine battery allocation
			battery2Count, battery3Count := allocateInverters(data, config, desiredCount, state)

			// Apply changes using circular buffer selection
			changed := applyInverterChanges(data, config, sender, battery2Count, battery3Count)

			if changed {
				state.lastModificationTime = time.Now()
				log.Printf("Unified inverter enabler: mode=%s, target=%.0fW, B2=%d, B3=%d\n",
					modeName(mode), targetWatts, battery2Count, battery3Count)
			}

		case <-ctx.Done():
			log.Println("Unified inverter enabler stopped")
			return
		}
	}
}

// determineMode selects operating mode based on Powerwall SOC and solar conditions
func determineMode(data DisplayData, config UnifiedInverterConfig) OperatingMode {
	// Check Powerwall SOC first - if low, prioritize draining local batteries
	// Use 15min P1 to avoid flip-flopping between modes (filters out brief low readings)
	powerwallSOC15MinP1 := data.GetFloat(config.PowerwallSOCTopic).P1._15
	if powerwallSOC15MinP1 < config.PowerwallLowThreshold {
		return PowerwallLowMode
	}

	// Check solar conditions for max inverter mode
	solarForecast := data.GetFloat(config.SolarForecastTopic).Current
	solarPower5MinAvg := data.GetFloat(config.Solar1PowerTopic).P50._5

	if solarForecast > config.MaxInverterModeSolarForecast &&
		solarPower5MinAvg > config.MaxInverterModeSolarPower {
		return MaxInverterMode
	}

	return PowerwallLastMode
}

// calculateTargetPower computes target watts based on mode with limit applied
func calculateTargetPower(data DisplayData, config UnifiedInverterConfig, mode OperatingMode) float64 {
	var target float64

	switch mode {
	case MaxInverterMode:
		target = 10000.0 // Will be limited by actual hardware anyway
	case PowerwallLowMode:
		loadPower15MinP99 := data.GetFloat(config.LoadPowerTopic).P99._15
		target = loadPower15MinP99 // 100% of peak load when Powerwall is low
	case PowerwallLastMode:
		target = data.GetFloat(config.LoadPowerTopic).P66._15 * (2.0 / 3.0)
	}

	// Apply limit: available capacity = 5000W - solar_1_power_15min_P99
	solar1Power15MinP99 := data.GetFloat(config.Solar1PowerTopic).P99._15
	availableCapacity := config.MaxTransferPower - solar1Power15MinP99
	target = min(target, availableCapacity)
	target = max(target, 0)

	return target
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

// allocateInverters distributes inverter count between batteries
func allocateInverters(
	data DisplayData,
	config UnifiedInverterConfig,
	totalCount int,
	state *InverterEnablerState,
) (battery2Count, battery3Count int) {
	if totalCount == 0 {
		return 0, 0
	}

	battery2Priority := isBatteryPriority(data, config.Battery2)
	battery3Priority := isBatteryPriority(data, config.Battery3)

	// Apply SOC-based limits with hysteresis
	soc2 := data.GetFloat(config.Battery2.SOCTopic).Current
	soc3 := data.GetFloat(config.Battery3.SOCTopic).Current
	maxBattery2 := maxInvertersForSOC(soc2, len(config.Battery2.Inverters), &state.battery2LockedOut)
	maxBattery3 := maxInvertersForSOC(soc3, len(config.Battery3.Inverters), &state.battery3LockedOut)

	switch {
	case battery2Priority && !battery3Priority:
		// Battery 2 priority: use it first, overflow to Battery 3
		battery2Count = min(totalCount, maxBattery2)
		remaining := totalCount - battery2Count
		battery3Count = min(remaining, maxBattery3)
	case battery3Priority && !battery2Priority:
		// Battery 3 priority: use it first, overflow to Battery 2
		battery3Count = min(totalCount, maxBattery3)
		remaining := totalCount - battery3Count
		battery2Count = min(remaining, maxBattery2)
	default:
		// Both priority or neither: split 50/50, prefer Battery 3 for odd
		battery3Count = (totalCount + 1) / 2
		battery2Count = totalCount - battery3Count

		// Respect maximums with overflow
		if battery3Count > maxBattery3 {
			overflow := battery3Count - maxBattery3
			battery3Count = maxBattery3
			battery2Count = min(battery2Count+overflow, maxBattery2)
		}
		if battery2Count > maxBattery2 {
			overflow := battery2Count - maxBattery2
			battery2Count = maxBattery2
			battery3Count = min(battery3Count+overflow, maxBattery3)
		}
	}

	return battery2Count, battery3Count
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

// modeName returns a string representation of the operating mode
func modeName(mode OperatingMode) string {
	switch mode {
	case MaxInverterMode:
		return "MaxInverter"
	case PowerwallLowMode:
		return "PowerwallLow"
	case PowerwallLastMode:
		return "PowerwallLast"
	default:
		return "Unknown"
	}
}
