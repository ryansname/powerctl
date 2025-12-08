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
func mqttSenderWorker(
	ctx context.Context,
	outgoingChan <-chan MQTTMessage,
	clientChan <-chan mqtt.Client,
) {
	log.Println("MQTT sender worker started")

	var client mqtt.Client
	var messageQueue []MQTTMessage

	for {
		select {
		case newClient := <-clientChan:
			log.Println("MQTT sender worker received new client")
			client = newClient

			// Process any queued messages now that we have a client
			if client != nil && client.IsConnected() {
				queuedCount := len(messageQueue)
				for _, msg := range messageQueue {
					token := client.Publish(msg.Topic, msg.QoS, msg.Retain, msg.Payload)
					token.Wait()
					if token.Error() != nil {
						log.Printf("Failed to publish queued message to %s: %v\n", msg.Topic, token.Error())
					}
				}
				messageQueue = nil // Clear the queue
				if queuedCount > 0 {
					log.Printf("MQTT sender worker processed %d queued messages\n", queuedCount)
				}
			}

		case msg := <-outgoingChan:
			if client != nil && client.IsConnected() {
				// We have a client, publish immediately
				token := client.Publish(msg.Topic, msg.QoS, msg.Retain, msg.Payload)
				token.Wait()
				if token.Error() != nil {
					log.Printf("Failed to publish to %s: %v\n", msg.Topic, token.Error())
				}
			} else {
				// No client yet, queue the message
				messageQueue = append(messageQueue, msg)
				log.Printf("MQTT sender worker queued message (total queued: %d)\n", len(messageQueue))
			}

		case <-ctx.Done():
			log.Println("MQTT sender worker stopped")
			return
		}
	}
}
