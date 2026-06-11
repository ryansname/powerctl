package main

import (
	"context"
	"encoding/json"
	"log"
	"math"
)

// Tank ADC sensor and calibration topics (HA statestream).
const (
	TopicHeaderTankADC  = "homeassistant/sensor/header_tank_2_adc/state"
	TopicStorageTankADC = "homeassistant/sensor/storage_tanks_adc_2/state"

	TopicHeaderTankFullVoltage   = "homeassistant/input_number/header_tank_full_voltage/state"
	TopicHeaderTankEmptyVoltage  = "homeassistant/input_number/header_tank_empty_voltage/state"
	TopicStorageTankFullVoltage  = "homeassistant/input_number/storage_tank_full_voltage/state"
	TopicStorageTankEmptyVoltage = "homeassistant/input_number/storage_tank_empty_voltage/state"
)

// Powerctl-owned tank level state topics. Header is also subscribed so the pump
// controller reads the same values HA displays.
const (
	TopicHeaderTankLevelsState  = "powerctl/sensor/header_tank/state"
	TopicStorageTankLevelsState = "powerctl/sensor/storage_tanks/state"
)

// tankLevelSentinel marks "no data yet" in pre-seeded tank level payloads.
// Real (unclamped) readings never get anywhere near it.
const tankLevelSentinel = -1000.0

// minCalibrationRange guards against degenerate full/empty calibration (and division by zero).
const minCalibrationRange = 0.1

// TankTopics returns the statestream topics the tank levels worker needs.
func TankTopics() []string {
	return []string{
		TopicHeaderTankADC,
		TopicStorageTankADC,
		TopicHeaderTankFullVoltage,
		TopicHeaderTankEmptyVoltage,
		TopicStorageTankFullVoltage,
		TopicStorageTankEmptyVoltage,
	}
}

// HeaderTankLevels is the JSON payload published to TopicHeaderTankLevelsState.
type HeaderTankLevels struct {
	PercentFull float64 `json:"percent_full"`
}

// StorageTankLevels is the JSON payload published to TopicStorageTankLevelsState.
// One sensor measures three stacked tanks; per-tank values are derived from the overall level.
type StorageTankLevels struct {
	PercentFull      float64 `json:"percent_full"`
	Tank1PercentFull float64 `json:"tank_1_percent_full"`
	Tank2PercentFull float64 `json:"tank_2_percent_full"`
	Tank3PercentFull float64 `json:"tank_3_percent_full"`
}

// TankLevelInput holds the smoothed ADC voltages and calibration voltages.
type TankLevelInput struct {
	HeaderADC  float64 // 5-minute trimean; < 0 means no data yet
	StorageADC float64

	HeaderFullVoltage   float64
	HeaderEmptyVoltage  float64
	StorageFullVoltage  float64
	StorageEmptyVoltage float64
}

// adcTrimean returns Tukey's trimean (P25 + 2·P50 + P75)/4 of the topic over the
// 5-minute window: smoother than a plain median while still ignoring spikes that
// occupy under a quarter of the window. A negative P25 means the startup sentinel
// (or persistent garbage) still carries weight, so the value is not yet trustworthy
// and must not be blended in — report "no data" instead.
func adcTrimean(data DisplayData, topic string) float64 {
	p25 := data.GetPercentile(topic, P25, Window5Min)
	if p25 < 0 {
		return -1
	}
	p50 := data.GetPercentile(topic, P50, Window5Min)
	p75 := data.GetPercentile(topic, P75, Window5Min)
	return (p25 + 2*p50 + p75) / 4
}

// ExtractTankLevelInput reads tank worker inputs from DisplayData.
func ExtractTankLevelInput(data DisplayData) TankLevelInput {
	return TankLevelInput{
		HeaderADC:           adcTrimean(data, TopicHeaderTankADC),
		StorageADC:          adcTrimean(data, TopicStorageTankADC),
		HeaderFullVoltage:   data.GetFloat(TopicHeaderTankFullVoltage).Current,
		HeaderEmptyVoltage:  data.GetFloat(TopicHeaderTankEmptyVoltage).Current,
		StorageFullVoltage:  data.GetFloat(TopicStorageTankFullVoltage).Current,
		StorageEmptyVoltage: data.GetFloat(TopicStorageTankEmptyVoltage).Current,
	}
}

// TankLevelOutput holds computed tank fill percentages. Invalid groups must not be published.
type TankLevelOutput struct {
	HeaderValid bool
	Header      HeaderTankLevels

	StorageValid bool
	Storage      StorageTankLevels
}

func round1(v float64) float64 {
	return math.Round(v*10) / 10
}

func clamp01(v float64) float64 {
	return math.Max(0, math.Min(100, v))
}

// ComputeTankLevels converts smoothed ADC voltages into fill percentages using
// linear interpolation between the empty and full calibration voltages.
func ComputeTankLevels(in TankLevelInput) TankLevelOutput {
	var out TankLevelOutput

	headerRange := in.HeaderFullVoltage - in.HeaderEmptyVoltage
	if in.HeaderADC >= 0 && headerRange >= minCalibrationRange {
		out.HeaderValid = true
		// Header is published unclamped (parity with the original Node-RED flow).
		out.Header.PercentFull = round1((in.HeaderADC - in.HeaderEmptyVoltage) / headerRange * 100)
	}

	storageRange := in.StorageFullVoltage - in.StorageEmptyVoltage
	if in.StorageADC >= 0 && storageRange >= minCalibrationRange {
		out.StorageValid = true
		raw := (in.StorageADC - in.StorageEmptyVoltage) / storageRange * 100
		out.Storage = StorageTankLevels{
			PercentFull:      round1(clamp01(raw)),
			Tank1PercentFull: round1(clamp01((raw - 66.6) * 3)),
			Tank2PercentFull: round1(clamp01((raw - 33.3) * 3)),
			Tank3PercentFull: round1(clamp01(raw * 3)),
		}
	}

	return out
}

// tankLevelsWorker computes water tank fill percentages from smoothed ADC voltages and
// publishes them as powerctl-owned HA sensors. Invalid groups (sensor offline since startup,
// degenerate calibration) are not published, so the HA entities expire to unavailable.
func tankLevelsWorker(ctx context.Context, dataChan <-chan DisplayData, sender *MQTTSender) {
	log.Println("Tank levels worker started")

	for {
		select {
		case data := <-dataChan:
			out := ComputeTankLevels(ExtractTankLevelInput(data))

			// The sender dedupes unchanged payloads per topic, so publishing
			// every tick is change-only on the wire (with a 5-minute keepalive
			// resend that feeds the entities' expire_after).
			if out.HeaderValid {
				payload, _ := json.Marshal(out.Header)
				sender.Send(MQTTMessage{
					Topic:   TopicHeaderTankLevelsState,
					Payload: payload,
					QoS:     0,
				})
			}
			if out.StorageValid {
				payload, _ := json.Marshal(out.Storage)
				sender.Send(MQTTMessage{
					Topic:   TopicStorageTankLevelsState,
					Payload: payload,
					QoS:     0,
				})
			}

		case <-ctx.Done():
			log.Println("Tank levels worker stopped")
			return
		}
	}
}
