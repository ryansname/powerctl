package main

import (
	"context"
	"log"
)

const (
	MinerWorkmodeEntity = "select.miner1_workmode_set"
	// Topic to read current miner workmode from HA
	TopicMinerWorkmode = "homeassistant/select/miner1_workmode_set/state"

	WorkmodeSuper    = "Super"
	WorkmodeStandard = "Standard"
	WorkmodeEco      = "Eco"
	WorkmodeOff      = "Standby"
)

// dumpLoadEnabler controls dump loads based on excess power
func dumpLoadEnabler(
	ctx context.Context,
	excessChan <-chan float64,
	dataChan <-chan DisplayData,
	sender *MQTTSender,
) {
	log.Println("Dump load enabler started")

	var latestExcess float64
	var latestData DisplayData
	excessReceived := false

	for {
		select {
		case excessWatts := <-excessChan:
			latestExcess = excessWatts
			excessReceived = true

		case data := <-dataChan:
			latestData = data

			// Wait until we've received at least one excess calculation
			if !excessReceived {
				continue
			}

			// TODO: When device_tracker.plb942_location_tracker is "Home"
			// subtract sensor.plb942_charger_power from excess power

			// Determine desired workmode based on excess power
			var desiredWorkmode string
			switch {
			case latestExcess > 1700:
				desiredWorkmode = WorkmodeSuper
			case latestExcess > 1200:
				desiredWorkmode = WorkmodeStandard
			case latestExcess > 800:
				desiredWorkmode = WorkmodeEco
			default:
				desiredWorkmode = WorkmodeOff
			}

			// Read actual workmode from Home Assistant via DisplayData
			currentWorkmode := latestData.GetString(TopicMinerWorkmode)

			// Only send command if workmode differs from actual state
			if desiredWorkmode != currentWorkmode {
				// log.Printf("Dump load: excess=%.0fW, changing workmode %s -> %s\n",
				// 	latestExcess, currentWorkmode, desiredWorkmode)
				// sender.SelectOption(MinerWorkmodeEntity, desiredWorkmode)
			}

		case <-ctx.Done():
			log.Println("Dump load enabler stopped")
			return
		}
	}
}
