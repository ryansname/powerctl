package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func makeFloatTopic(v float64) *FloatTopicData { return &FloatTopicData{Current: v} }
func makeBoolTopic(v bool, raw string) *BooleanTopicData {
	return &BooleanTopicData{Current: v, Raw: raw}
}
func makeStringTopic(v string) *StringTopicData { return &StringTopicData{Current: v} }

const (
	testTopicB2SOC    = "b2soc"
	testTopicB2Charge = "b2charge"
	testTopicB2Volt   = "b2volt"
	testTopicB2Energy = "b2energy"
	testTopicGrid     = "grid"
	testTopicB3SOC    = "b3soc"
	testTopicSolar1   = "solar1"
	testTopicSolar2   = "solar2"
	testTopicLoad     = "load"
	testTopicPWSOC    = "pwsoc"
)

func makeBaselineDisplayData() (DisplayData, BaselineInputConfig) {
	freqTopic := "freq"
	config := BaselineInputConfig{
		Battery2SOCTopic:         testTopicB2SOC,
		Battery2ChargeStateTopic: testTopicB2Charge,
		Battery2VoltageTopic:     testTopicB2Volt,
		Battery2EnergyTopic:      testTopicB2Energy,
		Solar1PowerTopic:         testTopicSolar1,
		Solar2PowerTopic:         testTopicSolar2,
		HouseLoadTopic:           testTopicLoad,
		GridStatusTopic:          testTopicGrid,
		ACFrequencyTopic:         freqTopic,
		ForecastRemainingTopic:   "forecastwh",
		DetailedForecastTopic:    "forecast",
		InverterStateTopics:      []string{"inv1", "inv2", "inv3"},
		Battery3SOCTopic:         testTopicB3SOC,
		PowerwallSOCTopic:        testTopicPWSOC,
		ExpectingPowerCutsTopic:  "powercuts",
	}

	freqKey := PercentileKey{Topic: freqTopic, Percentile: P100, Window: Window5Min}
	solar1P90Key := PercentileKey{Topic: config.Solar1PowerTopic, Percentile: P90, Window: Window15Min}
	data := DisplayData{
		TopicData: map[string]any{
			testTopicB2SOC:    makeFloatTopic(87.5),
			testTopicB2Charge: makeStringTopic("Float Charging"),
			testTopicB2Volt:   makeFloatTopic(52.1),
			testTopicB2Energy: makeFloatTopic(8500),
			testTopicSolar1:   makeFloatTopic(1200),
			testTopicSolar2:   makeFloatTopic(800),
			testTopicLoad:     makeFloatTopic(1500),
			testTopicGrid:     makeBoolTopic(true, "on"),
			freqTopic:         makeFloatTopic(50.02),
			"forecastwh":      makeFloatTopic(12000),
			"forecast":        makeStringTopic("[]"),
			"inv1":            makeBoolTopic(true, "on"),
			"inv2":            makeBoolTopic(false, "off"),
			"inv3":            makeBoolTopic(true, "on"),
			testTopicB3SOC:    makeFloatTopic(72.0),
			testTopicPWSOC:    makeFloatTopic(45.0),
			"powercuts":       makeBoolTopic(false, "off"),
		},
		Percentiles: map[PercentileKey]float64{
			freqKey:      50.15,
			solar1P90Key: 900.0,
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
	assert.InDelta(t, 900.0, input.Solar1P90_15Min, 0.001)
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
		HouseLoadTopic:          testTopicLoad,
		Solar1PowerTopic:        testTopicSolar1,
		Solar2PowerTopic:        testTopicSolar2,
		Inverter1to9PowerTopics: []string{"inv1p", "inv2p", "inv3p"},
		MultiplusACPowerTopic:   "multiplusac",
		Battery3SOCTopic:        testTopicB3SOC,
		GridStatusTopic:         testTopicGrid,
		ACFrequencyTopic:        freqTopic,
		PowerwallSOCTopic:       testTopicPWSOC,
	}

	freqKey := PercentileKey{Topic: freqTopic, Percentile: P100, Window: 5 * time.Minute}
	data := DisplayData{
		TopicData: map[string]any{
			testTopicLoad:   makeFloatTopic(2000),
			testTopicSolar1: makeFloatTopic(1500),
			testTopicSolar2: makeFloatTopic(600),
			"inv1p":         makeFloatTopic(-255),
			"inv2p":         makeFloatTopic(-255),
			"inv3p":         makeFloatTopic(-510),
			"multiplusac":   makeFloatTopic(-800),
			testTopicB3SOC:  makeFloatTopic(65.0),
			testTopicGrid:   makeBoolTopic(true, "on"),
			freqTopic:       makeFloatTopic(50.0),
			testTopicPWSOC:  makeFloatTopic(88.0),
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
	data.TopicData[testTopicGrid] = makeBoolTopic(false, "off")
	input := ExtractDynamicInput(data, config)
	assert.False(t, input.GridAvailable)
}
