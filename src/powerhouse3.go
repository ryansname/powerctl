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
	TopicInverter10SetpointCmd  = "powerctl/number/powerhouse_inverter_10_ac_setpoint/set"
)

func cerboKeepaliveWorker(ctx context.Context, sender *MQTTSender) {
	keepalivePayload, err := json.Marshal([]string{
		"N/system/0/Dc/Battery/Power",
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

func inverter10SetpointWorker(ctx context.Context, statsChan <-chan DisplayData, sender *MQTTSender) {
	log.Println("Inverter 10 setpoint worker started")

	var setpoint float64
	var data DisplayData

	select {
	case <-ctx.Done():
		return
	case data = <-statsChan:
		setpoint = data.GetFloat(TopicInverter10SetpointCmd).Current
	}

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("Inverter 10 setpoint worker stopped")
			return
		case data = <-statsChan:
			setpoint = data.GetFloat(TopicInverter10SetpointCmd).Current
		case <-ticker.C:
			payload, _ := json.Marshal(map[string]float64{"value": setpoint})
			sender.Send(MQTTMessage{Topic: TopicMultiplusSetpointWrite, Payload: payload, QoS: 0})
		}
	}
}
