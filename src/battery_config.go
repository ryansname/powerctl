package main

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

