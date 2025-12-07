package main

import (
	"context"
	"encoding/json"
	"log"
	"strings"
	"time"
)

// BatteryConfig holds configuration for a battery monitor
type BatteryConfig struct {
	Name                string
	CapacityKWh         float64
	InflowTopics        []string
	OutflowTopics       []string
	ChargeStateTopic    string
	BatteryVoltageTopic string
}

// BatteryState tracks the current state of a battery
type BatteryState struct {
	AvailableWh      float64 // Primary state: available energy in Wh
	PrevInflowTotal  float64
	PrevOutflowTotal float64
	LastUpdateTime   time.Time // Track time for BMS/controller loss calculation
	Initialized      bool
}

// getInitialPercentage estimates battery percentage based on voltage and charge state
func getInitialPercentage(voltage float64, chargeState string) float64 {
	// If in float charging and high voltage, assume nearly full
	if strings.Contains(chargeState, "Float") && voltage > 53 {
		return 95
	}

	if voltage < 51 {
		return 0
	} else if voltage < 52 {
		return 20
	} else if voltage < 53 {
		return 50
	} else if voltage < 54 {
		return 75
	}
	return 90
}

// batteryMonitorWorker monitors battery charge percentage
func batteryMonitorWorker(ctx context.Context, dataChan <-chan DisplayData, config BatteryConfig, outgoingChan chan<- MQTTMessage, manufacturer string) {
	state := &BatteryState{}

	log.Printf("Battery monitor started: %s (%.0f kWh capacity)\n", config.Name, config.CapacityKWh)

	// Create Home Assistant entities for this battery
	// Create percentage sensor
	err := createBatteryEntity(
		outgoingChan,
		config.Name,
		config.CapacityKWh,
		manufacturer,
		"State of Charge",
		"battery",
		"%",
		"percentage",
		"measurement",
		1,
	)
	if err != nil {
		log.Printf("Failed to create percentage entity for %s: %v\n", config.Name, err)
	}

	// Create energy sensor
	err = createBatteryEntity(
		outgoingChan,
		config.Name,
		config.CapacityKWh,
		manufacturer,
		"Available Energy",
		"energy",
		"Wh",
		"available_wh",
		"measurement",
		0,
	)
	if err != nil {
		log.Printf("Failed to create energy entity for %s: %v\n", config.Name, err)
	}

	log.Printf("%s: Home Assistant entities created\n", config.Name)

	for {
		select {
		case data := <-dataChan:
			// Extract current values
			var voltage float64
			var chargeState string
			var inflowTotal float64
			var outflowTotal float64

			// Get voltage
			if voltData, ok := data.TopicData[config.BatteryVoltageTopic]; ok {
				if floatData, ok := voltData.(*FloatTopicData); ok {
					voltage = floatData.Current
				}
			}

			// Get charge state
			if stateData, ok := data.TopicData[config.ChargeStateTopic]; ok {
				if strData, ok := stateData.(*StringTopicData); ok {
					chargeState = strData.Current
				}
			}

			// Sum up inflows
			for _, topic := range config.InflowTopics {
				if flowData, ok := data.TopicData[topic]; ok {
					if floatData, ok := flowData.(*FloatTopicData); ok {
						inflowTotal += floatData.Current
					}
				}
			}

			// Sum up outflows
			for _, topic := range config.OutflowTopics {
				if flowData, ok := data.TopicData[topic]; ok {
					if floatData, ok := flowData.(*FloatTopicData); ok {
						outflowTotal += floatData.Current
					}
				}
			}

			// Initialize on first valid voltage reading
			if !state.Initialized && voltage > 0 {
				initialPercentage := getInitialPercentage(voltage, chargeState)
				state.AvailableWh = (initialPercentage / 100) * config.CapacityKWh * 1000
				state.PrevInflowTotal = inflowTotal
				state.PrevOutflowTotal = outflowTotal
				state.LastUpdateTime = time.Now()
				state.Initialized = true
				log.Printf("%s: Initialized at %.1f%% (%.0f Wh, voltage: %.1fV, state: %s)\n",
					config.Name, initialPercentage, state.AvailableWh, voltage, chargeState)
			}

			// Update battery available Wh based on energy deltas
			if state.Initialized {
				// Calculate time delta for BMS/controller loss
				now := time.Now()
				timeDelta := now.Sub(state.LastUpdateTime).Hours()

				// Calculate energy changes (in kWh, convert to Wh)
				inflowDelta := (inflowTotal - state.PrevInflowTotal) * 1000    // kWh to Wh
				outflowDelta := (outflowTotal - state.PrevOutflowTotal) * 1000 // kWh to Wh

				// Account for conversion losses (2% on outflows)
				outflowWithLosses := outflowDelta * 1.02

				// Calculate BMS and charge controller losses (50W constant)
				bmsLoss := 50.0 * timeDelta // Wh

				// Update available Wh based on energy flow
				state.AvailableWh += inflowDelta
				state.AvailableWh -= outflowWithLosses
				state.AvailableWh -= bmsLoss

				// Calibration points
				capacityWh := config.CapacityKWh * 1000
				if voltage > 0 {
					// Calibrate to 100% when float charging and high voltage
					if strings.Contains(chargeState, "Float") && voltage > 53.5 {
						if state.AvailableWh != capacityWh {
							log.Printf("%s: Calibrating to 100%% (%.0f Wh) - Float Charging, %.1fV\n",
								config.Name, capacityWh, voltage)
							state.AvailableWh = capacityWh
						}
					}

					// Calibrate to 0% when voltage is critically low
					if voltage < 51 {
						if state.AvailableWh != 0 {
							log.Printf("%s: Calibrating to 0%% (0 Wh) - Low Voltage: %.1fV\n", config.Name, voltage)
							state.AvailableWh = 0
						}
					}
				}

				// Clamp available Wh to 0 to capacity range
				if state.AvailableWh < 0 {
					state.AvailableWh = 0
				} else if state.AvailableWh > capacityWh {
					state.AvailableWh = capacityWh
				}

				// Update previous values
				state.PrevInflowTotal = inflowTotal
				state.PrevOutflowTotal = outflowTotal
				state.LastUpdateTime = now

				// Calculate percentage for display
				percentage := (state.AvailableWh / capacityWh) * 100

				// Send state to Home Assistant
				deviceId := strings.ReplaceAll(strings.ToLower(config.Name), " ", "_")
				stateTopic := "homeassistant/sensor/" + deviceId + "/state"

				statePayload := map[string]interface{}{
					"percentage":   percentage,
					"available_wh": state.AvailableWh,
				}

				payloadBytes, err := json.Marshal(statePayload)
				if err != nil {
					log.Printf("%s: Failed to marshal state payload: %v\n", config.Name, err)
				} else {
					outgoingChan <- MQTTMessage{
						Topic:   stateTopic,
						Payload: payloadBytes,
						QoS:     0,
						Retain:  false,
					}
				}
			}

		case <-ctx.Done():
			log.Printf("Battery monitor stopped: %s\n", config.Name)
			return
		}
	}
}
