package main

import (
	"context"
	"log"
	"time"

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
	var lastCommandSent time.Time
	commandCooldown := 60 * time.Second

	var autoDisableTimer *time.Timer
	var autoDisableChan <-chan time.Time

	for {
		select {
		case data := <-dataChan:
			enabled := data.GetBoolean(TopicExpectingPowerCutsState)
			soc := data.GetFloat(PowerwallSOCTopic).Current
			discharging := data.GetBoolean(TopicPW2DischargeState)

			// Start/stop 24h auto-disable timer when switch toggles
			if enabled && autoDisableTimer == nil {
				autoDisableTimer = time.NewTimer(24 * time.Hour)
				autoDisableChan = autoDisableTimer.C
			} else if !enabled && autoDisableTimer != nil {
				autoDisableTimer.Stop()
				autoDisableTimer = nil
				autoDisableChan = nil
			}

			if time.Since(lastCommandSent) < commandCooldown {
				continue
			}

			shouldDischarge := enabled && hysteresis.Update(soc) > 0

			if shouldDischarge && !discharging {
				log.Printf("Expecting power cuts: SOC %.1f%% >= 90%%, enabling PW2 discharge\n", soc)
				sender.Send(MQTTMessage{
					Topic:   TopicPW2DischargeState,
					Payload: []byte("ON"),
					QoS:     1,
				})
				lastCommandSent = time.Now()
			} else if !shouldDischarge && discharging {
				log.Printf("Expecting power cuts: disabling PW2 discharge (enabled=%v, SOC=%.1f%%)\n", enabled, soc)
				sender.Send(MQTTMessage{
					Topic:   TopicPW2DischargeState,
					Payload: []byte("OFF"),
					QoS:     1,
				})
				lastCommandSent = time.Now()
			}

		case <-autoDisableChan:
			log.Println("Expecting power cuts: auto-disabling after 24 hours")
			sender.Send(MQTTMessage{
				Topic:   TopicExpectingPowerCutsState,
				Payload: []byte("OFF"),
				QoS:     1,
			})
			autoDisableTimer = nil
			autoDisableChan = nil

		case <-ctx.Done():
			if autoDisableTimer != nil {
				autoDisableTimer.Stop()
			}
			log.Println("Expecting power cuts worker stopped")
			return
		}
	}
}
