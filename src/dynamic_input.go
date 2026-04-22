package main

// DynamicInputConfig holds the topics needed to extract DynamicInput from DisplayData.
type DynamicInputConfig struct {
	HouseLoadTopic            string
	Solar1PowerTopic          string
	Solar2PowerTopic          string
	Inverter1to9PowerTopics   []string
	MultiplusACPowerTopic     string
	Battery3SOCTopic          string
	GridStatusTopic           string
	ACFrequencyTopic          string
	PowerwallSOCTopic         string
	DynamicAutoTopic          string
	MultiplusSetpointCmdTopic string
}

// DynamicInput holds extracted values for the dynamic inverter controller.
type DynamicInput struct {
	HouseLoad              float64
	Solar1Power            float64
	Solar2Power            float64
	Inverter1to9Power      float64
	MultiplusACPower       float64
	Battery3SOC            float64
	GridAvailable          bool
	ACFreqP100_5Min        float64
	PowerwallSOC           float64
	DynamicAutoEnabled     bool
	MultiplusSetpointCmd   float64
}

// ExtractDynamicInput extracts values from DisplayData for the dynamic controller.
func ExtractDynamicInput(data DisplayData, config DynamicInputConfig) DynamicInput {
	return DynamicInput{
		HouseLoad:            data.GetFloat(config.HouseLoadTopic).Current,
		Solar1Power:          data.GetFloat(config.Solar1PowerTopic).Current,
		Solar2Power:          data.GetFloat(config.Solar2PowerTopic).Current,
		Inverter1to9Power:    data.SumTopics(config.Inverter1to9PowerTopics),
		MultiplusACPower:     data.GetFloat(config.MultiplusACPowerTopic).Current,
		Battery3SOC:          data.GetFloat(config.Battery3SOCTopic).Current,
		GridAvailable:        data.GetBoolean(config.GridStatusTopic),
		ACFreqP100_5Min:      data.GetPercentile(config.ACFrequencyTopic, P100, Window5Min),
		PowerwallSOC:         data.GetFloat(config.PowerwallSOCTopic).Current,
		DynamicAutoEnabled:   data.GetBoolean(config.DynamicAutoTopic),
		MultiplusSetpointCmd: data.GetFloat(config.MultiplusSetpointCmdTopic).Current,
	}
}
