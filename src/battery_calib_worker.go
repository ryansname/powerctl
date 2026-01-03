package main

import (
	"context"
	"encoding/json"
	"log"
	"strings"
	"time"
)

// batteryCalibWorker monitors voltage and charge state to publish calibration data
func batteryCalibWorker(
	ctx context.Context,
	dataChan <-chan DisplayData,
	config BatteryCalibConfig,
	sender *MQTTSender,
) {
	var lastSoftCapTime time.Time
	const softCapCooldown = 2 * time.Second

	for {
		select {
		case data := <-dataChan:
			voltage := data.GetFloat(config.BatteryVoltageTopic).Current
			chargeState := data.GetString(config.ChargeStateTopic)

			isFloatCharging := strings.Contains(chargeState, config.FloatChargeState)

			if isFloatCharging {
				// In Float Charging mode - only do 100% calibration if:
				// 1. Voltage is high enough
				// 2. Power flow is balanced (within 50W) - prevents false triggers during solar spikes
				if voltage >= config.HighVoltageThreshold {
					inflowPower := data.SumTopics(config.InflowPowerTopics)
					outflowPower := data.SumTopics(config.OutflowPowerTopics)
					netPower := inflowPower - outflowPower

					const powerBalanceThreshold = 50.0
					if netPower >= -powerBalanceThreshold && netPower <= powerBalanceThreshold {
						inflows := data.SumTopics(config.InflowEnergyTopics)
						outflows := data.SumTopics(config.OutflowEnergyTopics)
						publishCalibration(sender, config.Name, inflows, outflows)
					}
				}
				// Otherwise do nothing - don't soft cap during Float Charging
			} else {
				// NOT in Float Charging - check for 99.5% soft cap (with cooldown)
				currentSOC := data.GetFloat(config.SOCTopic).Current
				calibInflows := data.GetFloat(config.CalibrationTopics.Inflows).Current
				calibOutflows := data.GetFloat(config.CalibrationTopics.Outflows).Current

				if time.Since(lastSoftCapTime) >= softCapCooldown && currentSOC >= 99.5 {
					// Fudge: reduce calibOutflows slightly to bring SOC down
					// Preserve original calibInflows, only adjust outflows
					fudgedOutflows := calibOutflows - 0.005 // subtract 0.005 kWh

					publishCalibration(sender, config.Name, calibInflows, fudgedOutflows)
					lastSoftCapTime = time.Now()
					log.Printf("%s: Adjusting calibration to reduce displayed SOC (%.1f%% -> 99.5%%)",
						config.Name, currentSOC)
				}
			}

		case <-ctx.Done():
			return
		}
	}
}

// publishCalibration publishes calibration reference points to MQTT
func publishCalibration(sender *MQTTSender, name string, inflows, outflows float64) {
	deviceId := strings.ReplaceAll(strings.ToLower(name), " ", "_")
	payload, _ := json.Marshal(map[string]interface{}{
		"calibration_inflows":  inflows,
		"calibration_outflows": outflows,
	})

	sender.Send(MQTTMessage{
		Topic:   "homeassistant/sensor/" + deviceId + "/attributes",
		Payload: payload,
		QoS:     1,
		Retain:  true,
	})
}
