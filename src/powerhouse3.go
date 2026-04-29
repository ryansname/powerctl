package main

import (
	"context"
	"encoding/json"
	"log"
	"time"
)

const (
	TopicCerboKeepalive        = "powerhouse_3/R/keepalive"
	TopicMultiplusSetpointWrite = "powerhouse_3/W/vebus/276/Hub4/L1/AcPowerSetpoint"
	TopicMultiplusSetpointRead  = "powerhouse_3/N/vebus/276/Hub4/L1/AcPowerSetpoint"
	TopicMultiplusACPower       = "powerhouse_3/N/vebus/276/Ac/ActiveIn/L1/P"
	TopicCerboBatterySOC        = "powerhouse_3/N/system/0/Dc/Battery/Soc"
	TopicInverter10SetpointCmd  = "powerctl/number/powerhouse_inverter_10_ac_setpoint/set"

	TopicSolarcharger278MppMode = "powerhouse_3/N/solarcharger/278/MppOperationMode"
	TopicSolarcharger279MppMode = "powerhouse_3/N/solarcharger/279/MppOperationMode"
	TopicCerboBatteryDCPower    = "powerhouse_3/N/system/0/Dc/Battery/Power"
	TopicCerboBatteryCurrent    = "powerhouse_3/N/system/0/Dc/Battery/Current"
	TopicCerboBatteryCCL        = "powerhouse_3/N/system/0/Dc/Battery/MaxChargeCurrent"

	TopicSolar3MpptMode = "homeassistant/sensor/solar_3_mppt_mode/state"
	TopicSolar4MpptMode = "homeassistant/sensor/solar_4_mppt_mode/state"
)

func cerboKeepaliveWorker(ctx context.Context, sender *MQTTSender) {
	keepalivePayload, err := json.Marshal([]string{
		"N/system/0/Dc/Battery/Power",
		"N/system/0/Dc/Battery/Soc",
		"N/system/0/Dc/Battery/Current",
		"N/system/0/Dc/Battery/MaxChargeCurrent",
		"N/vebus/276/Hub4/L1/AcPowerSetpoint",
		"N/vebus/276/Ac/ActiveIn/L1/P",
	})
	if err != nil {
		log.Fatalf("cerbo-keepalive: failed to marshal keepalive payload: %v", err)
	}

	send := func() {
		sender.Send(MQTTMessage{Topic: TopicCerboKeepalive, Payload: keepalivePayload, QoS: 0})
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

