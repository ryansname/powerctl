package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
)

// calculateAvailableWh computes available energy from calibration reference point
func calculateAvailableWh(
	capacityWh float64,
	calibInflows, calibOutflows float64,
	inflowTotal, outflowTotal float64,
	conversionLossRate float64,
) float64 {
	// Energy in since calibration (kWh to Wh)
	energyIn := (inflowTotal - calibInflows) * 1000

	// Energy out since calibration with conversion losses (kWh to Wh)
	energyOut := (outflowTotal - calibOutflows) * 1000
	energyOutWithLosses := energyOut * (1.0 + conversionLossRate)

	// Calculate available energy
	available := capacityWh + energyIn - energyOutWithLosses

	// Clamp to valid range
	if available < 0 {
		return 0
	}
	if available > capacityWh {
		return capacityWh
	}
	return available
}

// batterySOCWorker reads calibration from DisplayData and performs energy accounting
func batterySOCWorker(
	ctx context.Context,
	dataChan <-chan DisplayData,
	config BatterySOCConfig,
	outgoingChan chan<- MQTTMessage,
) {
	log.Printf("%s SOC worker started\n", config.Name)

	capacityWh := config.CapacityKWh * 1000 // Convert kWh to Wh

	for {
		select {
		case data := <-dataChan:
			// Extract calibration data from statestream topics (totals when battery was last at 100%)
			calibInflows := data.GetFloat(config.CalibrationTopics.Inflows)
			calibOutflows := data.GetFloat(config.CalibrationTopics.Outflows)

			// Calculate current inflow and outflow totals
			inflowTotal := data.SumTopics(config.InflowTopics)
			outflowTotal := data.SumTopics(config.OutflowTopics)

			// Calculate available energy from calibration point
			availableWh := calculateAvailableWh(
				capacityWh,
				calibInflows,
				calibOutflows,
				inflowTotal,
				outflowTotal,
				config.ConversionLossRate,
			)

			// Calculate percentage
			percentage := (availableWh / capacityWh) * 100

			// Publish state to MQTT
			deviceId := "battery_" + string(config.Name[len(config.Name)-1]) // "Battery 2" -> "battery_2"
			stateTopic := fmt.Sprintf("homeassistant/sensor/%s/state", deviceId)

			statePayload := map[string]interface{}{
				"percentage":   percentage,
				"available_wh": availableWh,
			}

			payloadBytes, err := json.Marshal(statePayload)
			if err != nil {
				log.Printf("%s: Failed to marshal state payload: %v\n", config.Name, err)
				continue
			}

			outgoingChan <- MQTTMessage{
				Topic:   stateTopic,
				Payload: payloadBytes,
				QoS:     0,
				Retain:  false,
			}

		case <-ctx.Done():
			log.Printf("%s SOC worker stopped\n", config.Name)
			return
		}
	}
}
