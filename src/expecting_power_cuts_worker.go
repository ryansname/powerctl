package main

import (
	"context"
	"log"

	"github.com/ryansname/powerctl/src/governor"
)

// TopicExpectingPowerCutsState is the state topic for the expecting power cuts switch.
const TopicExpectingPowerCutsState = "homeassistant/switch/powerctl_expecting_power_cuts/state"

// PowerwallSOCTopic is the Powerwall 2 state of charge topic.
const PowerwallSOCTopic = "homeassistant/sensor/home_sweet_home_charge/state"

// expectingPowerCutsWorker monitors Powerwall SOC and automatically toggles
// the PW2 discharge switch when the expecting power cuts switch is enabled.
// Uses hysteresis: enables discharge at >=90% SOC, disables at <=85% SOC.
func expectingPowerCutsWorker(
	ctx context.Context,
	dataChan <-chan DisplayData,
	sender *MQTTSender,
) {
	log.Println("Expecting power cuts worker started")

	// 1-step ascending hysteresis: on at 90%, off at 85%
	hysteresis := governor.NewSteppedHysteresis(1, true, 90, 90, 85, 85)
	discharging := false

	for {
		select {
		case data := <-dataChan:
			enabled := data.GetBoolean(TopicExpectingPowerCutsState)
			soc := data.GetFloat(PowerwallSOCTopic).Current

			shouldDischarge := enabled && hysteresis.Update(soc) > 0

			if shouldDischarge && !discharging {
				log.Printf("Expecting power cuts: SOC %.1f%% >= 90%%, enabling PW2 discharge\n", soc)
				sender.Send(MQTTMessage{
					Topic:   "powerctl/switch/powerctl_pw2_discharge/set",
					Payload: []byte("ON"),
					QoS:     1,
				})
				discharging = true
			} else if !shouldDischarge && discharging {
				log.Printf("Expecting power cuts: disabling PW2 discharge (enabled=%v, SOC=%.1f%%)\n", enabled, soc)
				sender.Send(MQTTMessage{
					Topic:   "powerctl/switch/powerctl_pw2_discharge/set",
					Payload: []byte("OFF"),
					QoS:     1,
				})
				discharging = false
			}

		case <-ctx.Done():
			log.Println("Expecting power cuts worker stopped")
			return
		}
	}
}
