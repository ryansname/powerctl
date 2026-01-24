package main

import (
	"strings"
)

// BatteryConfig holds shared configuration for a battery
type BatteryConfig struct {
	Name                 string
	CapacityKWh          float64
	Manufacturer         string
	InflowEnergyTopics   []string // Cumulative energy (kWh)
	OutflowEnergyTopics  []string // Cumulative energy (kWh)
	InflowPowerTopics    []string // Instantaneous power (W)
	OutflowPowerTopics   []string // Instantaneous power (W)
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
	InflowEnergyTopics   []string // Cumulative energy (kWh)
	OutflowEnergyTopics  []string // Cumulative energy (kWh)
	InflowPowerTopics    []string // Instantaneous power (W)
	OutflowPowerTopics   []string // Instantaneous power (W)
	HighVoltageThreshold float64
	FloatChargeState     string
	CalibrationTopics    CalibrationTopics // To read/write calibration values
	SOCTopic             string            // To read current SOC from DisplayData
}

// BatterySOCConfig holds configuration for the SOC worker
type BatterySOCConfig struct {
	Name               string
	CapacityKWh        float64
	InflowEnergyTopics []string
	OutflowEnergyTopics []string
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
	deviceID := strings.ReplaceAll(strings.ToLower(c.Name), " ", "_")
	return BatteryCalibConfig{
		Name:                 c.Name,
		ChargeStateTopic:     c.ChargeStateTopic,
		BatteryVoltageTopic:  c.BatteryVoltageTopic,
		InflowEnergyTopics:   c.InflowEnergyTopics,
		OutflowEnergyTopics:  c.OutflowEnergyTopics,
		InflowPowerTopics:    c.InflowPowerTopics,
		OutflowPowerTopics:   c.OutflowPowerTopics,
		HighVoltageThreshold: c.HighVoltageThreshold,
		FloatChargeState:     c.FloatChargeState,
		CalibrationTopics:    c.CalibrationTopics,
		SOCTopic:             "homeassistant/sensor/" + deviceID + "_state_of_charge/state",
	}
}

// SOCConfig creates a BatterySOCConfig from the shared BatteryConfig
func (c *BatteryConfig) SOCConfig() BatterySOCConfig {
	return BatterySOCConfig{
		Name:                c.Name,
		CapacityKWh:         c.CapacityKWh,
		InflowEnergyTopics:  c.InflowEnergyTopics,
		OutflowEnergyTopics: c.OutflowEnergyTopics,
		CalibrationTopics:   c.CalibrationTopics,
		ConversionLossRate:  c.ConversionLossRate,
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
	buildInverterGroup := func(b BatteryConfig, availableEnergyTopic string) BatteryInverterGroup {
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
		shortName := strings.ReplaceAll(b.Name, "Battery ", "B")
		return BatteryInverterGroup{
			Name:                 b.Name,
			ShortName:            shortName,
			Inverters:            inverters,
			ChargeStateTopic:     b.ChargeStateTopic,
			SOCTopic:             "homeassistant/sensor/" + deviceID + "_state_of_charge/state",
			CapacityWh:           b.CapacityKWh * 1000,
			SolarMultiplier:      4.0, // Solar array size relative to Solcast 1kW reference
			AvailableEnergyTopic: availableEnergyTopic,
		}
	}

	return UnifiedInverterConfig{
		Battery2:                    buildInverterGroup(battery2, TopicBattery2Energy),
		Battery3:                    buildInverterGroup(battery3, TopicBattery3Energy),
		SolarForecastTopic:          "homeassistant/sensor/solcast_pv_forecast_forecast_today/state",
		SolarForecastRemainingTopic: "homeassistant/sensor/solcast_pv_forecast_forecast_remaining_today/state",
		DetailedForecastTopic:       "homeassistant/sensor/solcast_pv_forecast_forecast_today/detailedForecast",
		Solar1PowerTopic:            TopicSolar1Power,
		Solar2PowerTopic:            "homeassistant/sensor/primo_5_0_ac_power/state",
		LoadPowerTopic:              "homeassistant/sensor/home_sweet_home_load_power_2/state",
		PowerwallSOCTopic:           "homeassistant/sensor/home_sweet_home_charge/state",
		GridStatusTopic:             "homeassistant/binary_sensor/home_sweet_home_grid_status_2/state",
		ACFrequencyTopic:            "homeassistant/sensor/lounge_ac_frequency/state",
		WattsPerInverter:            255.0,
		MaxTransferPower:            5000.0,
		PowerwallLowThreshold:       30.0,
		OverflowFloatChargeState:    "Float Charging",
		OverflowSOCTurnOffStart:     98.5,
		OverflowSOCTurnOffEnd:       95.0,
		OverflowSOCTurnOnStart:      95.75,
		OverflowSOCTurnOnEnd:        99.5,
	}
}

// Topics returns all MQTT topics needed by the unified inverter enabler
func (c UnifiedInverterConfig) Topics() []string {
	topics := []string{
		c.SolarForecastTopic,
		c.SolarForecastRemainingTopic,
		c.DetailedForecastTopic,
		c.Solar1PowerTopic,
		c.Solar2PowerTopic,
		c.LoadPowerTopic,
		c.PowerwallSOCTopic,
		c.GridStatusTopic,
		c.ACFrequencyTopic,
		c.Battery2.ChargeStateTopic,
		c.Battery3.ChargeStateTopic,
		c.Battery2.SOCTopic,
		c.Battery3.SOCTopic,
		c.Battery2.AvailableEnergyTopic,
		c.Battery3.AvailableEnergyTopic,
	}

	for _, inv := range c.Battery2.Inverters {
		topics = append(topics, inv.StateTopic)
	}
	for _, inv := range c.Battery3.Inverters {
		topics = append(topics, inv.StateTopic)
	}

	return topics
}
