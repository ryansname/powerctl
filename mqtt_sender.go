package main

import (
	"context"
	"log"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

// MQTTMessage represents an outgoing MQTT message
type MQTTMessage struct {
	Topic   string
	Payload []byte
	QoS     byte
	Retain  bool
}

// mqttSenderWorker handles outgoing MQTT messages with queuing
func mqttSenderWorker(ctx context.Context, outgoingChan <-chan MQTTMessage, client mqtt.Client) {
	log.Println("MQTT sender worker started")

	for {
		select {
		case msg := <-outgoingChan:
			token := client.Publish(msg.Topic, msg.QoS, msg.Retain, msg.Payload)
			token.Wait()
			if token.Error() != nil {
				log.Printf("Failed to publish to %s: %v\n", msg.Topic, token.Error())
			}

		case <-ctx.Done():
			log.Println("MQTT sender worker stopped")
			return
		}
	}
}
