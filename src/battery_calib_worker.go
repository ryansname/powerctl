package main

import (
	"context"
	"encoding/json"
	"strings"
)

// batteryCalibWorker monitors voltage and charge state to publish calibration data
func batteryCalibWorker(
	ctx context.Context,
	dataChan <-chan DisplayData,
	config BatteryCalibConfig,
	sender *MQTTSender,
) {
	for {
		select {
		case data := <-dataChan:
			voltage := data.GetFloat(config.BatteryVoltageTopic).Current
			chargeState := data.GetString(config.ChargeStateTopic)

			isFloatCharging := strings.Contains(chargeState, config.FloatChargeState)
			isCalibrated := isFloatCharging && voltage >= config.HighVoltageThreshold

			if isCalibrated {
				inflows := data.SumTopics(config.InflowTopics)
				outflows := data.SumTopics(config.OutflowTopics)

				deviceId := strings.ReplaceAll(strings.ToLower(config.Name), " ", "_")
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

		case <-ctx.Done():
			return
		}
	}
}
