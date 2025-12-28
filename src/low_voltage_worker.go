package main

import (
	"context"
	"log"
	"time"
)

// lowVoltageWorker monitors battery voltage and turns off inverters when voltage drops too low
func lowVoltageWorker(
	ctx context.Context,
	dataChan <-chan DisplayData,
	config LowVoltageConfig,
	sender *MQTTSender,
) {
	log.Printf("%s low voltage protection worker started (threshold: %.2fV)\n",
		config.Name, config.LowVoltageThreshold)

	invertersOff := false
	var resetTimer *time.Timer
	var resetTimerC <-chan time.Time

	for {
		select {
		case data := <-dataChan:
			voltage15MinP1 := data.GetFloat(config.BatteryVoltageTopic).P1._15

			if voltage15MinP1 < config.LowVoltageThreshold && !invertersOff {
				log.Printf("%s: LOW VOLTAGE (%.2fV < %.2fV) - turning off %d inverters\n",
					config.Name, voltage15MinP1, config.LowVoltageThreshold,
					len(config.InverterSwitchIDs))

				for _, entityID := range config.InverterSwitchIDs {
					sender.CallService("switch", "turn_off", entityID, nil)
					log.Printf("%s: Sent turn_off command for %s\n", config.Name, entityID)
				}

				invertersOff = true
				resetTimer = time.NewTimer(16 * time.Minute)
				resetTimerC = resetTimer.C
			}

		case <-resetTimerC:
			log.Printf("%s: Reset timer fired, re-enabling low voltage protection\n", config.Name)
			invertersOff = false
			resetTimer = nil
			resetTimerC = nil

		case <-ctx.Done():
			if resetTimer != nil {
				resetTimer.Stop()
			}
			log.Printf("%s low voltage protection worker stopped\n", config.Name)
			return
		}
	}
}
