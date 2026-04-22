package main

import "github.com/ryansname/powerctl/src/governor"

// BaselineInputConfig holds the topics needed to extract BaselineInput from DisplayData.
type BaselineInputConfig struct {
	Battery2SOCTopic         string
	Battery2ChargeStateTopic string
	Battery2VoltageTopic     string
	Battery2EnergyTopic      string
	Solar1PowerTopic         string
	Solar2PowerTopic         string
	HouseLoadTopic           string
	GridStatusTopic          string
	ACFrequencyTopic         string
	ForecastRemainingTopic   string
	DetailedForecastTopic    string
	InverterStateTopics      []string
	Battery3SOCTopic         string
	PowerwallSOCTopic        string
	ExpectingPowerCutsTopic  string
}

// BaselineInput holds extracted values for the baseline inverter controller.
type BaselineInput struct {
	Battery2SOC         float64
	Battery2ChargeState string
	Battery2Voltage     float64
	Battery2EnergyWh    float64
	Solar1Power         float64
	Solar2Power         float64
	HouseLoad           float64
	GridAvailable       bool
	ACFrequency         float64
	ACFreqP100_5Min     float64
	ForecastRemainingWh float64
	DetailedForecast    governor.ForecastPeriods
	InverterStates      []bool
	Battery3SOC         float64
	PowerwallSOC        float64
	ExpectingPowerCuts  bool
}

// ExtractBaselineInput extracts values from DisplayData for the baseline controller.
func ExtractBaselineInput(data DisplayData, config BaselineInputConfig) BaselineInput {
	var forecast governor.ForecastPeriods
	data.GetJSON(config.DetailedForecastTopic, &forecast)

	states := make([]bool, len(config.InverterStateTopics))
	for i, topic := range config.InverterStateTopics {
		states[i] = data.GetBoolean(topic)
	}

	return BaselineInput{
		Battery2SOC:         data.GetFloat(config.Battery2SOCTopic).Current,
		Battery2ChargeState: data.GetString(config.Battery2ChargeStateTopic),
		Battery2Voltage:     data.GetFloat(config.Battery2VoltageTopic).Current,
		Battery2EnergyWh:    data.GetFloat(config.Battery2EnergyTopic).Current,
		Solar1Power:         data.GetFloat(config.Solar1PowerTopic).Current,
		Solar2Power:         data.GetFloat(config.Solar2PowerTopic).Current,
		HouseLoad:           data.GetFloat(config.HouseLoadTopic).Current,
		GridAvailable:       data.GetBoolean(config.GridStatusTopic),
		ACFrequency:         data.GetFloat(config.ACFrequencyTopic).Current,
		ACFreqP100_5Min:     data.GetPercentile(config.ACFrequencyTopic, P100, Window5Min),
		ForecastRemainingWh: data.GetFloat(config.ForecastRemainingTopic).Current,
		DetailedForecast:    forecast,
		InverterStates:      states,
		Battery3SOC:         data.GetFloat(config.Battery3SOCTopic).Current,
		PowerwallSOC:        data.GetFloat(config.PowerwallSOCTopic).Current,
		ExpectingPowerCuts:  data.GetBoolean(config.ExpectingPowerCutsTopic),
	}
}
