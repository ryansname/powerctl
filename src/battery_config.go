package main

import (
	"strings"
	"time"
)

// BatteryConfig holds shared configuration for a battery
type BatteryConfig struct {
	Name                 string
	CapacityKWh          float64
	Manufacturer         string
	InflowTopics         []string
	OutflowTopics        []string
	ChargeStateTopic     string
	BatteryVoltageTopic  string
	CalibrationTopics    CalibrationTopics
	HighVoltageThreshold float64
	FloatChargeState     string
	ConversionLossRate   float64
	InverterSwitchIDs    []string
}

// CalibrationTopics holds statestream topic paths for calibration data
type CalibrationTopics struct {
	Inflows  string
	Outflows string
}

// BatteryCalibConfig holds configuration for the calibration worker
type BatteryCalibConfig struct {
	Name                 string
	ChargeStateTopic     string
	BatteryVoltageTopic  string
	InflowTopics         []string
	OutflowTopics        []string
	HighVoltageThreshold float64
	FloatChargeState     string
}

// BatterySOCConfig holds configuration for the SOC worker
type BatterySOCConfig struct {
	Name               string
	CapacityKWh        float64
	InflowTopics       []string
	OutflowTopics      []string
	CalibrationTopics  CalibrationTopics
	ConversionLossRate float64
}

// LowVoltageConfig holds configuration for the low voltage protection worker
type LowVoltageConfig struct {
	Name                string
	BatteryVoltageTopic string
	InverterSwitchIDs   []string
	LowVoltageThreshold float64
}


// CalibConfig creates a BatteryCalibConfig from the shared BatteryConfig
func (c *BatteryConfig) CalibConfig() BatteryCalibConfig {
	return BatteryCalibConfig{
		Name:                 c.Name,
		ChargeStateTopic:     c.ChargeStateTopic,
		BatteryVoltageTopic:  c.BatteryVoltageTopic,
		InflowTopics:         c.InflowTopics,
		OutflowTopics:        c.OutflowTopics,
		HighVoltageThreshold: c.HighVoltageThreshold,
		FloatChargeState:     c.FloatChargeState,
	}
}

// SOCConfig creates a BatterySOCConfig from the shared BatteryConfig
func (c *BatteryConfig) SOCConfig() BatterySOCConfig {
	return BatterySOCConfig{
		Name:               c.Name,
		CapacityKWh:        c.CapacityKWh,
		InflowTopics:       c.InflowTopics,
		OutflowTopics:      c.OutflowTopics,
		CalibrationTopics:  c.CalibrationTopics,
		ConversionLossRate: c.ConversionLossRate,
	}
}

// LowVoltageProtectionConfig creates a LowVoltageConfig from the shared BatteryConfig
func (c *BatteryConfig) LowVoltageProtectionConfig(threshold float64) LowVoltageConfig {
	return LowVoltageConfig{
		Name:                c.Name,
		BatteryVoltageTopic: c.BatteryVoltageTopic,
		InverterSwitchIDs:   c.InverterSwitchIDs,
		LowVoltageThreshold: threshold,
	}
}

// BuildUnifiedInverterConfig creates configuration for the unified inverter enabler
func BuildUnifiedInverterConfig(battery2, battery3 BatteryConfig) UnifiedInverterConfig {
	buildInverterGroup := func(b BatteryConfig) BatteryInverterGroup {
		inverters := make([]InverterInfo, len(b.InverterSwitchIDs))
		for i, entityID := range b.InverterSwitchIDs {
			// Convert entity ID to state topic
			// e.g., "switch.powerhouse_inverter_1_switch_0" -> "homeassistant/switch/powerhouse_inverter_1_switch_0/state"
			parts := strings.SplitN(entityID, ".", 2)
			stateTopic := ""
			if len(parts) == 2 {
				stateTopic = "homeassistant/" + parts[0] + "/" + parts[1] + "/state"
			}
			inverters[i] = InverterInfo{
				EntityID:   entityID,
				StateTopic: stateTopic,
			}
		}

		deviceID := strings.ReplaceAll(strings.ToLower(b.Name), " ", "_")
		return BatteryInverterGroup{
			Name:             b.Name,
			Inverters:        inverters,
			ChargeStateTopic: b.ChargeStateTopic,
			SOCTopic:         "homeassistant/sensor/" + deviceID + "_state_of_charge/state",
		}
	}

	return UnifiedInverterConfig{
		Battery2:                     buildInverterGroup(battery2),
		Battery3:                     buildInverterGroup(battery3),
		SolarForecastTopic:           "homeassistant/sensor/solcast_pv_forecast_forecast_today/state",
		Solar1PowerTopic:             "homeassistant/sensor/solar_1_power/state",
		LoadPowerTopic:               "homeassistant/sensor/home_sweet_home_load_power_2/state",
		PowerwallSOCTopic:            "homeassistant/sensor/home_sweet_home_charge/state",
		WattsPerInverter:             255.0,
		MaxTransferPower:             5000.0,
		MaxInverterModeSolarForecast: 3000.0, // Wh (converted from kWh)
		MaxInverterModeSolarPower:    1000.0,
		PowerwallLowThreshold:        30.0,
		CooldownDuration:             1 * time.Minute,
	}
}

// Topics returns all MQTT topics needed by the unified inverter enabler
func (c UnifiedInverterConfig) Topics() []string {
	topics := []string{
		c.SolarForecastTopic,
		c.Solar1PowerTopic,
		c.LoadPowerTopic,
		c.PowerwallSOCTopic,
		c.Battery2.ChargeStateTopic,
		c.Battery3.ChargeStateTopic,
		c.Battery2.SOCTopic,
		c.Battery3.SOCTopic,
	}

	for _, inv := range c.Battery2.Inverters {
		topics = append(topics, inv.StateTopic)
	}
	for _, inv := range c.Battery3.Inverters {
		topics = append(topics, inv.StateTopic)
	}

	return topics
}

