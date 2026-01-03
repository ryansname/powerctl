package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
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

	// Calculate available energy, clamped to valid range
	available := capacityWh + energyIn - energyOutWithLosses
	return max(0, min(available, capacityWh))
}

// batterySOCWorker reads calibration from DisplayData and performs energy accounting
func batterySOCWorker(
	ctx context.Context,
	dataChan <-chan DisplayData,
	config BatterySOCConfig,
	sender *MQTTSender,
) {
	log.Printf("%s SOC worker started\n", config.Name)

	capacityWh := config.CapacityKWh * 1000 // Convert kWh to Wh

	for {
		select {
		case data := <-dataChan:
			// Extract calibration data from statestream topics (totals when battery was last at 100%)
			calibInflows := data.GetFloat(config.CalibrationTopics.Inflows).Current
			calibOutflows := data.GetFloat(config.CalibrationTopics.Outflows).Current

			// Calculate current inflow and outflow totals
			inflowTotal := data.SumTopics(config.InflowEnergyTopics)
			outflowTotal := data.SumTopics(config.OutflowEnergyTopics)

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
			deviceId := strings.ReplaceAll(strings.ToLower(config.Name), " ", "_")
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

			sender.Send(MQTTMessage{
				Topic:   stateTopic,
				Payload: payloadBytes,
				QoS:     0,
				Retain:  false,
			})

		case <-ctx.Done():
			log.Printf("%s SOC worker stopped\n", config.Name)
			return
		}
	}
}
