package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func makeFloatTopic(v float64) *FloatTopicData   { return &FloatTopicData{Current: v} }
func makeBoolTopic(v bool, raw string) *BooleanTopicData {
	return &BooleanTopicData{Current: v, Raw: raw}
}
func makeStringTopic(v string) *StringTopicData { return &StringTopicData{Current: v} }

func makeBaselineDisplayData() (DisplayData, BaselineInputConfig) {
	freqTopic := "freq"
	config := BaselineInputConfig{
		Battery2SOCTopic:         "b2soc",
		Battery2ChargeStateTopic: "b2charge",
		Battery2VoltageTopic:     "b2volt",
		Battery2EnergyTopic:      "b2energy",
		Solar1PowerTopic:         "solar1",
		Solar2PowerTopic:         "solar2",
		HouseLoadTopic:           "load",
		GridStatusTopic:          "grid",
		ACFrequencyTopic:         freqTopic,
		ForecastRemainingTopic:   "forecastwh",
		DetailedForecastTopic:    "forecast",
		InverterStateTopics:      []string{"inv1", "inv2", "inv3"},
		Battery3SOCTopic:         "b3soc",
		PowerwallSOCTopic:        "pwsoc",
		ExpectingPowerCutsTopic:  "powercuts",
	}

	freqKey := PercentileKey{Topic: freqTopic, Percentile: P100, Window: Window5Min}
	data := DisplayData{
		TopicData: map[string]any{
			"b2soc":     makeFloatTopic(87.5),
			"b2charge":  makeStringTopic("Float Charging"),
			"b2volt":    makeFloatTopic(52.1),
			"b2energy":  makeFloatTopic(8500),
			"solar1":    makeFloatTopic(1200),
			"solar2":    makeFloatTopic(800),
			"load":      makeFloatTopic(1500),
			"grid":      makeBoolTopic(true, "on"),
			freqTopic:   makeFloatTopic(50.02),
			"forecastwh": makeFloatTopic(12000),
			"forecast":  makeStringTopic("[]"),
			"inv1":      makeBoolTopic(true, "on"),
			"inv2":      makeBoolTopic(false, "off"),
			"inv3":      makeBoolTopic(true, "on"),
			"b3soc":     makeFloatTopic(72.0),
			"pwsoc":     makeFloatTopic(45.0),
			"powercuts": makeBoolTopic(false, "off"),
		},
		Percentiles: map[PercentileKey]float64{
			freqKey: 50.15,
		},
	}
	return data, config
}

func TestExtractBaselineInput(t *testing.T) {
	data, config := makeBaselineDisplayData()
	input := ExtractBaselineInput(data, config)

	assert.InDelta(t, 87.5, input.Battery2SOC, 0.001)
	assert.Equal(t, "Float Charging", input.Battery2ChargeState)
	assert.InDelta(t, 52.1, input.Battery2Voltage, 0.001)
	assert.InDelta(t, 8500.0, input.Battery2EnergyWh, 0.001)
	assert.InDelta(t, 1200.0, input.Solar1Power, 0.001)
	assert.InDelta(t, 800.0, input.Solar2Power, 0.001)
	assert.InDelta(t, 1500.0, input.HouseLoad, 0.001)
	assert.True(t, input.GridAvailable)
	assert.InDelta(t, 50.02, input.ACFrequency, 0.001)
	assert.InDelta(t, 50.15, input.ACFreqP100_5Min, 0.001)
	assert.InDelta(t, 12000.0, input.ForecastRemainingWh, 0.001)
	assert.Empty(t, input.DetailedForecast)
	assert.Equal(t, []bool{true, false, true}, input.InverterStates)
	assert.InDelta(t, 72.0, input.Battery3SOC, 0.001)
	assert.InDelta(t, 45.0, input.PowerwallSOC, 0.001)
	assert.False(t, input.ExpectingPowerCuts)
}

func TestExtractBaselineInput_ExpectingPowerCuts(t *testing.T) {
	data, config := makeBaselineDisplayData()
	data.TopicData["powercuts"] = makeBoolTopic(true, "on")
	input := ExtractBaselineInput(data, config)
	assert.True(t, input.ExpectingPowerCuts)
}

func makeDynamicDisplayData() (DisplayData, DynamicInputConfig) {
	freqTopic := "freq"
	config := DynamicInputConfig{
		HouseLoadTopic:          "load",
		Solar1PowerTopic:        "solar1",
		Solar2PowerTopic:        "solar2",
		Inverter1to9PowerTopics: []string{"inv1p", "inv2p", "inv3p"},
		MultiplusACPowerTopic:   "multiplusac",
		Battery3SOCTopic:        "b3soc",
		GridStatusTopic:         "grid",
		ACFrequencyTopic:        freqTopic,
		PowerwallSOCTopic:       "pwsoc",
	}

	freqKey := PercentileKey{Topic: freqTopic, Percentile: P100, Window: 5 * time.Minute}
	data := DisplayData{
		TopicData: map[string]any{
			"load":       makeFloatTopic(2000),
			"solar1":     makeFloatTopic(1500),
			"solar2":     makeFloatTopic(600),
			"inv1p":      makeFloatTopic(255),
			"inv2p":      makeFloatTopic(255),
			"inv3p":      makeFloatTopic(510),
			"multiplusac": makeFloatTopic(-800),
			"b3soc":      makeFloatTopic(65.0),
			"grid":       makeBoolTopic(true, "on"),
			freqTopic:    makeFloatTopic(50.0),
			"pwsoc":      makeFloatTopic(88.0),
		},
		Percentiles: map[PercentileKey]float64{
			freqKey: 50.1,
		},
	}
	return data, config
}

func TestExtractDynamicInput(t *testing.T) {
	data, config := makeDynamicDisplayData()
	input := ExtractDynamicInput(data, config)

	assert.InDelta(t, 2000.0, input.HouseLoad, 0.001)
	assert.InDelta(t, 1500.0, input.Solar1Power, 0.001)
	assert.InDelta(t, 600.0, input.Solar2Power, 0.001)
	assert.InDelta(t, 1020.0, input.Inverter1to9Power, 0.001) // 255+255+510
	assert.InDelta(t, -800.0, input.MultiplusACPower, 0.001)
	assert.InDelta(t, 65.0, input.Battery3SOC, 0.001)
	assert.True(t, input.GridAvailable)
	assert.InDelta(t, 50.1, input.ACFreqP100_5Min, 0.001)
	assert.InDelta(t, 88.0, input.PowerwallSOC, 0.001)
}

func TestExtractDynamicInput_GridOff(t *testing.T) {
	data, config := makeDynamicDisplayData()
	data.TopicData["grid"] = makeBoolTopic(false, "off")
	input := ExtractDynamicInput(data, config)
	assert.False(t, input.GridAvailable)
}
