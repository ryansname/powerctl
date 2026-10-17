package main

import (
	"context"
	"encoding/json"
	"log"
	"time"
)

const (
	TopicCerboKeepalive         = "powerhouse_3/R/keepalive"
	TopicMultiplusSetpointWrite = "powerhouse_3/W/vebus/276/Hub4/L1/AcPowerSetpoint"
	TopicMultiplusSetpointRead  = "powerhouse_3/N/vebus/276/Hub4/L1/AcPowerSetpoint"
	TopicMultiplusACPower       = "powerhouse_3/N/vebus/276/Ac/ActiveIn/L1/P"
	// TopicMultiplusACInVoltage is the grid voltage the Multiplus measures. It
	// drives the voltage discharge limit: it updates every ~2s, against ~60s for
	// the Shelly inverter sensors, and that latency is what sets how fast the
	// limit is allowed to move (see voltageShedRateWps).
	TopicMultiplusACInVoltage  = "powerhouse_3/N/vebus/276/Ac/ActiveIn/L1/V"
	TopicMultiplusDCCurrent    = "powerhouse_3/N/vebus/276/Dc/0/Current"
	TopicCerboBatterySOC       = "powerhouse_3/N/system/0/Dc/Battery/Soc"
	TopicInverter10SetpointCmd = "powerctl/number/powerhouse_inverter_10_ac_setpoint/set"

	TopicSolarcharger278MppMode = "powerhouse_3/N/solarcharger/278/MppOperationMode"
	TopicSolarcharger279MppMode = "powerhouse_3/N/solarcharger/279/MppOperationMode"
	TopicCerboBatteryDCPower    = "powerhouse_3/N/system/0/Dc/Battery/Power"
	TopicCerboBatteryCurrent    = "powerhouse_3/N/system/0/Dc/Battery/Current"
	TopicCerboBatteryCCL        = "powerhouse_3/N/battery/512/Info/MaxChargeCurrent"
	TopicCerboBatteryCVL        = "powerhouse_3/N/battery/512/Info/MaxChargeVoltage"

	// DoNotFeedInOvervoltage is ours to set in ESS external control (mode 3); the
	// Cerbo's own "feed in excess DC-coupled PV" setting no longer drives it. Its
	// default of 0 feeds in: the solar chargers are aimed 1V above CVL and the
	// Multiplus, sitting in absorption, inverts whatever lifts the bus past CVL —
	// regardless of its setpoint, and uncapped by any powerctl limit. With solar
	// over what the AC side can absorb that ramped to 3kW and tripped the fleet.
	// At 1 the solar chargers regulate at CVL and the setpoint stays in charge.
	TopicMultiplusNoOvervoltageFeedInWrite = "powerhouse_3/W/vebus/276/Hub4/DoNotFeedInOvervoltage"
	TopicMultiplusNoOvervoltageFeedInRead  = "powerhouse_3/N/vebus/276/Hub4/DoNotFeedInOvervoltage"
)

func cerboKeepaliveWorker(ctx context.Context, sender *MQTTSender) {
	keepalivePayload, err := json.Marshal([]string{
		"N/system/0/Dc/Battery/Power",
		"N/system/0/Dc/Battery/Soc",
		"N/system/0/Dc/Battery/Current",
		"N/battery/512/Info/MaxChargeCurrent",
		"N/battery/512/Info/MaxChargeVoltage",
		"N/vebus/276/Hub4/L1/AcPowerSetpoint",
		"N/vebus/276/Ac/ActiveIn/L1/P",
		// Without this the Cerbo stops publishing voltage and the discharge limit
		// silently freezes on a stale reading — the keepalive list is the only
		// thing keeping these paths alive.
		"N/vebus/276/Ac/ActiveIn/L1/V",
		"N/vebus/276/Dc/0/Current",
		"N/vebus/276/Hub4/DoNotFeedInOvervoltage",
	})
	if err != nil {
		log.Fatalf("cerbo-keepalive: failed to marshal keepalive payload: %v", err)
	}

	noFeedInPayload, err := json.Marshal(map[string]int{haServiceValueKey: 1})
	if err != nil {
		log.Fatalf("cerbo-keepalive: failed to marshal no-feed-in payload: %v", err)
	}

	send := func() {
		sender.Send(MQTTMessage{Topic: TopicCerboKeepalive, Payload: keepalivePayload, QoS: 0})
		// Re-asserted on every tick rather than once: a Cerbo or Multiplus restart
		// resets it to the feed-in default. See TopicMultiplusNoOvervoltageFeedInWrite.
		sender.Send(MQTTMessage{Topic: TopicMultiplusNoOvervoltageFeedInWrite, Payload: noFeedInPayload, QoS: 0})
	}

	log.Println("Cerbo keepalive worker started")
	send()

	ticker := time.NewTicker(50 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("Cerbo keepalive worker stopped")
			return
		case <-ticker.C:
			send()
		}
	}
}
