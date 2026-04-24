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
	CerboSOCTopic        string // If set, SOC entity reads from this Cerbo MQTT topic instead of powerctl state
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

// buildInverterGroup converts a BatteryConfig to a BatteryInverterGroup.
func buildInverterGroup(b BatteryConfig, availableEnergyTopic string) BatteryInverterGroup {
	inverters := make([]InverterInfo, len(b.InverterSwitchIDs))
	for i, entityID := range b.InverterSwitchIDs {
		parts := strings.SplitN(entityID, ".", 2)
		stateTopic := ""
		if len(parts) == 2 {
			stateTopic = "homeassistant/" + parts[0] + "/" + parts[1] + "/state"
		}
		inverters[i] = InverterInfo{EntityID: entityID, StateTopic: stateTopic}
	}
	deviceID := strings.ReplaceAll(strings.ToLower(b.Name), " ", "_")
	return BatteryInverterGroup{
		Name:                 b.Name,
		Inverters:            inverters,
		ChargeStateTopic:     b.ChargeStateTopic,
		SOCTopic:             "homeassistant/sensor/" + deviceID + "_state_of_charge/state",
		BatteryVoltageTopic:  b.BatteryVoltageTopic,
		CapacityWh:           b.CapacityKWh * 1000,
		SolarMultiplier:      3.9,
		AvailableEnergyTopic: availableEnergyTopic,
	}
}

// BuildBaselineInverterConfig creates configuration for the baseline inverter controller.
func BuildBaselineInverterConfig(battery2, battery3 BatteryConfig) BaselineInverterConfig {
	group := buildInverterGroup(battery2, TopicBattery2Energy)
	deviceID2 := strings.ReplaceAll(strings.ToLower(battery2.Name), " ", "_")

	inverterStateTopics := make([]string, len(battery2.InverterSwitchIDs))
	for i, entityID := range battery2.InverterSwitchIDs {
		parts := strings.SplitN(entityID, ".", 2)
		if len(parts) == 2 {
			inverterStateTopics[i] = "homeassistant/" + parts[0] + "/" + parts[1] + "/state"
		}
	}

	input := BaselineInputConfig{
		Battery2SOCTopic:         "homeassistant/sensor/" + deviceID2 + "_state_of_charge/state",
		Battery2ChargeStateTopic: battery2.ChargeStateTopic,
		Battery2VoltageTopic:     battery2.BatteryVoltageTopic,
		Battery2EnergyTopic:      TopicBattery2Energy,
		Solar1PowerTopic:         TopicSolar1Power,
		Solar2PowerTopic:         "homeassistant/sensor/primo_5_0_ac_power/state",
		HouseLoadTopic:           "homeassistant/sensor/home_sweet_home_load_power_2/state",
		GridStatusTopic:          "homeassistant/binary_sensor/home_sweet_home_grid_status_2/state",
		ACFrequencyTopic:         "homeassistant/sensor/lounge_ac_frequency/state",
		ForecastRemainingTopic:   "homeassistant/sensor/solcast_pv_forecast_forecast_remaining_today/state",
		DetailedForecastTopic:    "homeassistant/sensor/solcast_pv_forecast_forecast_today/detailedForecast",
		InverterStateTopics:      inverterStateTopics,
		Battery3SOCTopic:         "homeassistant/sensor/" + strings.ReplaceAll(strings.ToLower(battery3.Name), " ", "_") + "_state_of_charge/state",
		PowerwallSOCTopic:        "homeassistant/sensor/home_sweet_home_charge/state",
		ExpectingPowerCutsTopic:  TopicExpectingPowerCutsState,
	}

	return BaselineInverterConfig{
		Input:                    input,
		Battery2:                 group,
		WattsPerInverter:         255.0,
		MaxTransferPower:         5000.0,
		MaxBaselineWatts:         500.0,
		OverflowSOCTurnOffStart:  98.5,
		OverflowSOCTurnOffEnd:    95.0,
		OverflowSOCTurnOnStart:   95.75,
		OverflowSOCTurnOnEnd:     99.5,
		LowVoltageTurnOnStart:  52.0,
		LowVoltageTurnOnEnd:    53.0,
		LowVoltageTurnOffStart: 50.75,
		LowVoltageTurnOffEnd:   52.0,
	}
}

// BuildDynamicInverterConfig creates configuration for the dynamic (Multiplus) inverter controller.
func BuildDynamicInverterConfig(battery2, battery3 BatteryConfig) DynamicInverterConfig {
	return DynamicInverterConfig{
		Input: DynamicInputConfig{
			HouseLoadTopic:            "homeassistant/sensor/home_sweet_home_load_power_2/state",
			Solar1PowerTopic:          TopicSolar1Power,
			Solar2PowerTopic:          "homeassistant/sensor/primo_5_0_ac_power/state",
			Inverter1to9PowerTopics:   battery2.OutflowPowerTopics,
			MultiplusACPowerTopic:     TopicMultiplusACPower,
			Battery3SOCTopic:          battery3.CerboSOCTopic,
			GridStatusTopic:           "homeassistant/binary_sensor/home_sweet_home_grid_status_2/state",
			ACFrequencyTopic:          "homeassistant/sensor/lounge_ac_frequency/state",
			PowerwallSOCTopic:         "homeassistant/sensor/home_sweet_home_charge/state",
			DynamicAutoTopic:          TopicDynamicAutoState,
			MultiplusSetpointCmdTopic: TopicInverter10SetpointCmd,
			CarChargingEnabledTopic:   TopicCarChargingEnabledState,
			CarChargingActiveTopic:    "homeassistant/binary_sensor/plb942_charging/state",
			CarBatterySOCTopic:        "homeassistant/sensor/plb942_battery/state",
			CarBattery3CutoffTopic:    TopicCarChargingBattery3CutoffState,
		},
	}
}
