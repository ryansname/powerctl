package main

import (
	"time"

	"github.com/ryansname/powerctl/src/governor"
)

// DynamicInputConfig holds the topics needed to extract DynamicInput from DisplayData.
type DynamicInputConfig struct {
	HouseLoadTopic            string
	Solar1PowerTopic          string
	Solar2PowerTopic          string
	Inverter1to9PowerTopics   []string
	MultiplusACPowerTopic     string
	MultiplusACInVoltageTopic string
	// InverterVoltageTopics are the per-inverter Shelly AC voltage sensors on the
	// powerhouse bus. Averaged (skipping absent ones) into a slow but accurate
	// reference that calibrates the Multiplus's own coarse reading.
	InverterVoltageTopics     []string
	SitePowerTopic            string
	Battery3SOCTopic          string
	GridStatusTopic           string
	ACFrequencyTopic          string
	PowerwallSOCTopic         string
	DynamicAutoTopic          string
	MultiplusSetpointCmdTopic string
	CarChargingEnabledTopic   string
	CarChargingActiveTopic    string
	CarBatterySOCTopic        string
	CarBattery3CutoffTopic    string
	Solar34PowerTopics        []string
	Battery3DCCurrentTopic    string
	Battery3CCLTopic          string
	Battery3CVLTopic          string
	Battery3VoltageTopic      string
	Solar3BatteryCurrentTopic string
	Solar4BatteryCurrentTopic string
	PowerhouseNetPowerTopic   string
	ForecastRemainingTopic    string
	DetailedForecastTopic     string
	Battery3CapacityWh        float64 // static config, not a topic
	SolarMultiplier           float64 // static config, not a topic
}

// DynamicInput holds extracted values for the dynamic inverter controller.
type DynamicInput struct {
	HouseLoad             float64
	Solar1Power           float64
	Solar2Power           float64
	Inverter1to9Power     float64
	MultiplusACPower      float64
	Battery3SOC           float64
	GridAvailable         bool
	ACFreqP100_5Min       float64
	PowerwallSOC          float64
	DynamicAutoEnabled    bool
	MultiplusSetpointCmd  float64
	CarChargingEnabled    bool
	CarChargingActive     bool
	CarBatterySOC         float64
	CarBattery3Cutoff     float64
	Tariff                Tariff
	Rebate                bool
	Solar34Power          float64 // combined Solar 3 + Solar 4 generation (W)
	Battery3ChargeCurrent float64 // A, actual battery charge current (always >= 0)
	Battery3CCL           float64 // A, BMS charge current limit
	Battery3CVL           float64 // V, BMS charge voltage limit
	Battery3Voltage       float64 // V
	Solar3BatteryCurrent  float64 // A
	Solar4BatteryCurrent  float64 // A
	PowerhouseNetPower    float64 // W, actual flow across the powerhouse↔house cable
	// MultiplusACInVoltage is grid voltage as the Multiplus measures it: fast
	// (~2s) but quantised to a ~0.9875V lattice, so it is smoothed and offset
	// against InverterVoltageAvg before use.
	MultiplusACInVoltage float64 // V
	// InverterVoltageAvg is the mean of the powerhouse Shelly voltage sensors,
	// skipping any that are absent. Slow (~60s, all reporting in one burst) but
	// precise (0.1V) and spatially averaged, so it serves as the accuracy
	// reference rather than as a control input. Zero when none are reporting.
	InverterVoltageAvg float64 // V
	// SitePower is whole-house flow at the grid meter: POSITIVE = importing,
	// NEGATIVE = exporting (the convention sankey/config.go already encodes, where
	// "Selling Back" is this sensor multiplied by -1). Includes the Multiplus's own
	// output; exportLimitConstraint strips that back out.
	SitePower           float64 // W, grid meter (+ve import, -ve export)
	ForecastRemainingWh float64 // Wh of solar forecast remaining today (statsWorker converts kWh→Wh)
	DetailedForecast    governor.ForecastPeriods
	Battery3CapacityWh  float64 // static config
	SolarMultiplier     float64 // static config; scales Solcast forecast to B3 arrays
}

// Tariff classifies the current time-of-use band for Vector's residential plan.
type Tariff int

const (
	TariffNight   Tariff = iota // 23:00-07:00 every day
	TariffOffpeak               // Weekdays 11:00-17:00, 21:00-23:00; weekends 07:00-23:00
	TariffPeak                  // Weekdays 07:00-11:00, 17:00-21:00
)

func (t Tariff) String() string {
	switch t {
	case TariffNight:
		return "Night"
	case TariffOffpeak:
		return "Offpeak"
	case TariffPeak:
		return "Peak"
	default:
		return "?"
	}
}

// CurrentTariff classifies t into Vector's Night/Offpeak/Peak band (local time).
func CurrentTariff(t time.Time) Tariff {
	t = t.Local()
	h := t.Hour()
	if h < 7 || h >= 23 {
		return TariffNight
	}
	wd := t.Weekday()
	if wd == time.Saturday || wd == time.Sunday {
		return TariffOffpeak
	}
	if (h >= 7 && h < 11) || (h >= 17 && h < 21) {
		return TariffPeak
	}
	return TariffOffpeak
}

// InRebateWindow reports whether t falls in Vector's 5.24c/kWh export rebate window:
// mornings 07:00-11:00 in Jun/Jul/Aug, evenings 17:00-22:00 in May-Sep. Local time.
// The rebate applies every day but is nulled by the base off-peak rate on weekends.
func InRebateWindow(t time.Time) bool {
	t = t.Local()
	h := t.Hour()
	m := t.Month()
	if h >= 7 && h < 11 {
		return m == time.June || m == time.July || m == time.August
	}
	if h >= 17 && h < 22 {
		return m >= time.May && m <= time.September
	}
	return false
}

// Topics returns all MQTT topics needed by the dynamic controller.
func (c DynamicInputConfig) Topics() []string {
	topics := []string{
		c.HouseLoadTopic,
		c.Solar1PowerTopic,
		c.Solar2PowerTopic,
		c.MultiplusACPowerTopic,
		c.MultiplusACInVoltageTopic,
		c.SitePowerTopic,
		c.Battery3SOCTopic,
		c.GridStatusTopic,
		c.ACFrequencyTopic,
		c.PowerwallSOCTopic,
		c.DynamicAutoTopic,
		c.MultiplusSetpointCmdTopic,
		c.CarChargingEnabledTopic,
		c.CarChargingActiveTopic,
		c.CarBatterySOCTopic,
		c.CarBattery3CutoffTopic,
		c.Battery3DCCurrentTopic,
		c.Battery3CCLTopic,
		c.Battery3CVLTopic,
		c.Battery3VoltageTopic,
		c.Solar3BatteryCurrentTopic,
		c.Solar4BatteryCurrentTopic,
		c.PowerhouseNetPowerTopic,
		c.ForecastRemainingTopic,
		c.DetailedForecastTopic,
	}
	topics = append(topics, c.Inverter1to9PowerTopics...)
	topics = append(topics, c.Solar34PowerTopics...)
	topics = append(topics, c.InverterVoltageTopics...)
	return topics
}

// averageNonZero means the given topics, skipping any reading exactly 0.
//
// Skipping zeros is a safety requirement, not tidiness: an absent or unavailable
// sensor reads 0, and folding that into the mean would drag it tens of volts
// below the real bus voltage — masking an over-voltage precisely when a sensor
// has dropped out. Returns 0 when nothing is reporting, which callers treat as
// "no reading" rather than as a valid low voltage.
func averageNonZero(data DisplayData, topics []string) float64 {
	sum, n := 0.0, 0
	for _, topic := range topics {
		if v := data.GetFloat(topic).Current; v != 0 {
			sum += v
			n++
		}
	}
	if n == 0 {
		return 0
	}
	return sum / float64(n)
}

// ExtractDynamicInput extracts values from DisplayData for the dynamic controller.
func ExtractDynamicInput(data DisplayData, config DynamicInputConfig) DynamicInput {
	gridAvailable := data.GetBoolean(config.GridStatusTopic)
	dynamicAutoEnabled := data.GetBoolean(config.DynamicAutoTopic)
	carChargingEnabled := data.GetBoolean(config.CarChargingEnabledTopic)
	carChargingActive := data.GetBoolean(config.CarChargingActiveTopic)

	var forecast governor.ForecastPeriods
	data.GetJSON(config.DetailedForecastTopic, &forecast)

	return DynamicInput{
		HouseLoad:             data.GetFloat(config.HouseLoadTopic).Current,
		Solar1Power:           data.GetFloat(config.Solar1PowerTopic).Current,
		Solar2Power:           data.GetFloat(config.Solar2PowerTopic).Current,
		Inverter1to9Power:     -data.SumTopics(config.Inverter1to9PowerTopics),
		MultiplusACPower:      data.GetFloat(config.MultiplusACPowerTopic).Current,
		Battery3SOC:           data.GetFloat(config.Battery3SOCTopic).Current,
		GridAvailable:         gridAvailable,
		ACFreqP100_5Min:       data.GetPercentile(config.ACFrequencyTopic, P100, Window5Min),
		PowerwallSOC:          data.GetFloat(config.PowerwallSOCTopic).Current,
		DynamicAutoEnabled:    dynamicAutoEnabled,
		MultiplusSetpointCmd:  data.GetFloat(config.MultiplusSetpointCmdTopic).Current,
		CarChargingEnabled:    carChargingEnabled,
		CarChargingActive:     carChargingActive,
		CarBatterySOC:         data.GetFloat(config.CarBatterySOCTopic).Current,
		CarBattery3Cutoff:     data.GetFloat(config.CarBattery3CutoffTopic).Current,
		Tariff:                CurrentTariff(time.Now()),
		Rebate:                InRebateWindow(time.Now()),
		Solar34Power:          data.SumTopics(config.Solar34PowerTopics),
		Battery3ChargeCurrent: max(0, data.GetFloat(config.Battery3DCCurrentTopic).Current),
		Battery3CCL:           data.GetFloat(config.Battery3CCLTopic).Current,
		Battery3CVL:           data.GetFloat(config.Battery3CVLTopic).Current,
		Battery3Voltage:       data.GetFloat(config.Battery3VoltageTopic).Current,
		Solar3BatteryCurrent:  data.GetFloat(config.Solar3BatteryCurrentTopic).Current,
		Solar4BatteryCurrent:  data.GetFloat(config.Solar4BatteryCurrentTopic).Current,
		PowerhouseNetPower:    data.GetFloat(config.PowerhouseNetPowerTopic).Current,
		// Already in W: statsWorker converts this topic via kiloToBaseUnitTopics.
		SitePower:            data.GetFloat(config.SitePowerTopic).Current,
		MultiplusACInVoltage: data.GetFloat(config.MultiplusACInVoltageTopic).Current,
		InverterVoltageAvg:   averageNonZero(data, config.InverterVoltageTopics),
		// Already in Wh: statsWorker converts this topic via kiloToBaseUnitTopics.
		ForecastRemainingWh: data.GetFloat(config.ForecastRemainingTopic).Current,
		DetailedForecast:    forecast,
		Battery3CapacityWh:  config.Battery3CapacityWh,
		SolarMultiplier:     config.SolarMultiplier,
	}
}
