package main

import (
	"context"
	"log"

	"github.com/ryansname/powerctl/src/governor"
)

// TopicPowerhouseBlowerTemp is the temperature sensor for the powerhouse blower.
const TopicPowerhouseBlowerTemp = "homeassistant/sensor/powerhouse_blower_temperature/state"

// TopicPowerhouseBlowerSwitch0State is the state topic for blower switch 0.
const TopicPowerhouseBlowerSwitch0State = "homeassistant/switch/powerhouse_blower_switch_0/state"

func powerhouseCoolingWorker(ctx context.Context, dataChan <-chan DisplayData, sender *MQTTSender) {
	const (
		switchBlower0 = "switch.powerhouse_blower_switch_0"
		switchBlower1 = "switch.powerhouse_blower_switch_1"
		coolOn        = 34.0
		coolOff       = 27.0
	)

	// RollingMinMax uses 60 1-minute buckets (~960 bytes fixed).
	// For the first ~59 minutes, Max() reflects a "since-startup max" rather
	// than a true 1-hour window — this is acceptable warm-up behavior.
	tracker := governor.NewRollingMinMax()

	log.Println("Powerhouse cooling worker started")

	for {
		select {
		case <-ctx.Done():
			log.Println("Powerhouse cooling worker stopped")
			return
		case data := <-dataChan:
			// Update called before Max() so tracker always has current temp on first tick.
			// statsWorker guarantees temperature topic has a real value before first broadcast.
			tracker.Update(data.GetFloat(TopicPowerhouseBlowerTemp).Current)
			tempMax := tracker.Max()
			cooling := data.GetBoolean(TopicPowerhouseBlowerSwitch0State)

			if !cooling && tempMax > coolOn {
				sender.CallService("switch", "turn_off", switchBlower1, nil)
				sender.CallService("switch", "turn_on", switchBlower0, nil)
				log.Printf("powerhouseCoolingWorker: cooling ON (1hr max %.1f°C)", tempMax)
			} else if cooling && tempMax < coolOff {
				sender.CallService("switch", "turn_off", switchBlower0, nil)
				log.Printf("powerhouseCoolingWorker: cooling OFF (1hr max %.1f°C)", tempMax)
			}
		}
	}
}
