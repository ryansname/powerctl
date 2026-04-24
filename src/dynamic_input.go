package main

import "time"


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
	Tariff                 Tariff
	Rebate                 bool
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
		c.Battery3SOCTopic,
		c.GridStatusTopic,
		c.ACFrequencyTopic,
		c.PowerwallSOCTopic,
		c.DynamicAutoTopic,
		c.MultiplusSetpointCmdTopic,
	}
	topics = append(topics, c.Inverter1to9PowerTopics...)
	return topics
}

// ExtractDynamicInput extracts values from DisplayData for the dynamic controller.
func ExtractDynamicInput(data DisplayData, config DynamicInputConfig) DynamicInput {
	return DynamicInput{
		HouseLoad:            data.GetFloat(config.HouseLoadTopic).Current,
		Solar1Power:          data.GetFloat(config.Solar1PowerTopic).Current,
		Solar2Power:          data.GetFloat(config.Solar2PowerTopic).Current,
		Inverter1to9Power:    -data.SumTopics(config.Inverter1to9PowerTopics),
		MultiplusACPower:     data.GetFloat(config.MultiplusACPowerTopic).Current,
		Battery3SOC:          data.GetFloat(config.Battery3SOCTopic).Current,
		GridAvailable:        data.GetBoolean(config.GridStatusTopic),
		ACFreqP100_5Min:      data.GetPercentile(config.ACFrequencyTopic, P100, Window5Min),
		PowerwallSOC:         data.GetFloat(config.PowerwallSOCTopic).Current,
		DynamicAutoEnabled:   data.GetBoolean(config.DynamicAutoTopic),
		MultiplusSetpointCmd: data.GetFloat(config.MultiplusSetpointCmdTopic).Current,
		Tariff:               CurrentTariff(time.Now()),
		Rebate:               InRebateWindow(time.Now()),
	}
}
