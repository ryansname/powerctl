package main

import (
	"context"
	"log"
)

// Topics for power excess calculation
const (
	TopicBattery1Energy = "homeassistant/sensor/home_sweet_home_tg118095000r1a_battery_remaining/state"
	TopicBattery2Energy = "homeassistant/sensor/battery_2_available_energy/state"
	TopicBattery3Energy = "homeassistant/sensor/battery_3_available_energy/state"
	TopicSolar1Power    = "homeassistant/sensor/solar_1_power/state"
)

// PowerExcessTopics returns all topics needed for power excess calculation
func PowerExcessTopics() []string {
	return []string{
		TopicBattery1Energy,
		TopicBattery2Energy,
		TopicBattery3Energy,
		TopicSolar1Power,
	}
}

// powerExcessCalculator calculates excess power available for dump loads
func powerExcessCalculator(
	ctx context.Context,
	dataChan <-chan DisplayData,
	excessChan chan<- float64,
) {
	log.Println("Power excess calculator started")

	for {
		select {
		case data := <-dataChan:
			excessWatts := 0.0

			// Tesla battery remaining: If 5min avg above 4kWh -> Add 1000W
			teslaRemaining := data.GetPercentile(TopicBattery1Energy, P50, Window5Min)
			if teslaRemaining > 4000 { // Wh (converted from kWh in statsWorker)
				excessWatts += 1000
			}

			// Battery 2 available energy: If 5min avg above 2.5kWh -> Add 450W
			battery2Energy := data.GetPercentile(TopicBattery2Energy, P50, Window5Min)
			if battery2Energy > 2500 { // Wh
				excessWatts += 450
			}

			// Battery 3 available energy: If 5min avg above 3kWh -> Add 450W
			battery3Energy := data.GetPercentile(TopicBattery3Energy, P50, Window5Min)
			if battery3Energy > 3000 { // Wh
				excessWatts += 450
			}

			// Cap battery excess at 900W
			excessWatts = min(excessWatts, 900)

			// Solar 1 power: If 5min avg above 1kW -> Add 1000W
			solar1Power := data.GetPercentile(TopicSolar1Power, P50, Window5Min)
			if solar1Power > 1000 {
				excessWatts += 1000
			}

			// Send excess to downstream worker
			select {
			case excessChan <- excessWatts:
			case <-ctx.Done():
				return
			}

		case <-ctx.Done():
			log.Println("Power excess calculator stopped")
			return
		}
	}
}
