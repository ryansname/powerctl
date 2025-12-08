package main

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

// CalibrationTopics holds statestream topic paths for calibration data
type CalibrationTopics struct {
	Inflows  string
	Outflows string
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

