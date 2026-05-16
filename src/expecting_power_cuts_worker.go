package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/ryansname/powerctl/src/governor"
)

// TopicExpectingPowerCutsState is the state topic for the expecting power cuts switch.
const TopicExpectingPowerCutsState = "homeassistant/switch/powerctl_expecting_power_cuts/state"

// PowerwallSOCTopic is the Powerwall 2 state of charge topic.
const PowerwallSOCTopic = "homeassistant/sensor/home_sweet_home_charge/state"

// TopicHotWaterCylinderState is the state topic for the hot water cylinder switch.
const TopicHotWaterCylinderState = "homeassistant/switch/hot_water_cylinder/state"

// powerCutVoteSource is the source name this worker uses on the discharge vote channel.
const powerCutVoteSource = "power-cut"

// expectingPowerCutsWorker prepares the house for an anticipated power cut:
// raises PW2 backup reserve, turns off the hot water cylinder, and votes for
// PW2 discharge when SOC is high (hysteresis: on at >=90%, off at <=85%).
// Discharge is requested via the arbiter vote channel rather than by writing
// the discharge switch directly.
func expectingPowerCutsWorker(
	ctx context.Context,
	dataChan <-chan DisplayData,
	voteChan chan<- DischargeRequest,
	sender *MQTTSender,
) {
	log.Println("Expecting power cuts worker started")

	hysteresis := governor.NewSteppedHysteresis(1, true, 90, 90, 85, 85)
	var lastCommandSent time.Time
	commandCooldown := 60 * time.Second

	var autoDisableTimer *time.Timer
	var autoDisableChan <-chan time.Time
	hotWaterTurnedOff := false
	var lastVote DischargeVote = -1
	var lastVoteReason string

	for {
		select {
		case data := <-dataChan:
			enabled := data.GetBoolean(TopicExpectingPowerCutsState)
			soc := data.GetFloat(PowerwallSOCTopic).Current

			if enabled && autoDisableTimer == nil {
				autoDisableTimer = time.NewTimer(24 * time.Hour)
				autoDisableChan = autoDisableTimer.C
			} else if !enabled && autoDisableTimer != nil {
				autoDisableTimer.Stop()
				autoDisableTimer = nil
				autoDisableChan = nil
			}

			// Vote on discharge every tick (sticky in the arbiter, but cheap to re-send).
			want := VoteNoOpinion
			reason := "disarmed"
			if enabled {
				if hysteresis.Update(soc) > 0 {
					want = VoteOn
					reason = fmt.Sprintf("SOC %.1f%% >= 90%%", soc)
				} else {
					reason = fmt.Sprintf("armed, SOC %.1f%% below 90%%", soc)
				}
			}
			if want != lastVote || reason != lastVoteReason {
				voteChan <- DischargeRequest{Source: powerCutVoteSource, Want: want, Reason: reason}
				lastVote = want
				lastVoteReason = reason
			}

			if time.Since(lastCommandSent) < commandCooldown {
				continue
			}

			backupReserve := data.GetFloat(TopicPW2BackupReserve).Current
			hotWaterOn := data.GetBoolean(TopicHotWaterCylinderState)

			if enabled && backupReserve < 50 {
				log.Println("Power cut prep: setting PW2 backup reserve to 50%")
				setBackupReserve(sender, 50)
				lastCommandSent = time.Now()
			} else if !enabled && backupReserve >= 50 {
				log.Println("Power cut prep over: restoring PW2 backup reserve to 10%")
				setBackupReserve(sender, 10)
				lastCommandSent = time.Now()
			}

			if enabled && hotWaterOn {
				if !hotWaterTurnedOff {
					log.Println("Power cut prep: turning off hot water cylinder")
					sender.CallService("switch", "turn_off", "switch.hot_water_cylinder", nil)
					hotWaterTurnedOff = true
					lastCommandSent = time.Now()
				} else {
					// Someone manually turned it back on — don't fight them
					hotWaterTurnedOff = false
				}
			} else if !enabled && hotWaterTurnedOff {
				log.Println("Power cut prep over: turning on hot water cylinder")
				sender.CallService("switch", "turn_on", "switch.hot_water_cylinder", nil)
				hotWaterTurnedOff = false
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
