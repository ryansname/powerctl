package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

// MQTTMessage represents an outgoing MQTT message
type MQTTMessage struct {
	Topic   string
	Payload []byte
	QoS     byte
	Retain  bool
}

// MQTTSender wraps a channel for sending MQTT messages with helper methods
type MQTTSender struct {
	ch chan<- MQTTMessage
}

// NewMQTTSender creates a new MQTTSender wrapping the given channel
func NewMQTTSender(ch chan<- MQTTMessage) *MQTTSender {
	return &MQTTSender{ch: ch}
}

// Send sends a raw MQTTMessage
func (s *MQTTSender) Send(msg MQTTMessage) {
	s.ch <- msg
}

// CallService sends a Home Assistant service call via the Node-RED proxy
func (s *MQTTSender) CallService(domain, service, entityID string) {
	payload, _ := json.Marshal(map[string]string{
		"domain":    domain,
		"service":   service,
		"entity_id": entityID,
	})

	s.ch <- MQTTMessage{
		Topic:   "nodered/proxy/call_service",
		Payload: payload,
		QoS:     1,
		Retain:  false,
	}
}

// CreateBatteryEntity creates a Home Assistant battery entity via MQTT discovery
func (s *MQTTSender) CreateBatteryEntity(
	batteryName string,
	capacityKWh float64,
	manufacturer string,
	entityName, entityClass, entityMeasure, jsonKey string,
	displayPrecision int,
) error {
	type haDeviceConfig struct {
		Identifiers  []string `json:"identifiers"`
		Name         string   `json:"name"`
		Manufacturer string   `json:"manufacturer,omitempty"`
		Model        string   `json:"model,omitempty"`
	}

	type haEntityConfig struct {
		Name                string         `json:"name,omitempty"`
		DeviceClass         string         `json:"device_class"`
		StateTopic          string         `json:"state_topic"`
		JsonAttributesTopic string         `json:"json_attributes_topic,omitempty"`
		UnitOfMeasure       string         `json:"unit_of_measurement,omitempty"`
		ValueTemplate       string         `json:"value_template"`
		UniqueId            string         `json:"unique_id"`
		ExpireAfter         uint           `json:"expire_after,omitempty"`
		StateClass          string         `json:"state_class,omitempty"`
		DisplayPrecision    int            `json:"suggested_display_precision,omitempty"`
		Device              haDeviceConfig `json:"device"`
	}

	deviceId := strings.ReplaceAll(strings.ToLower(batteryName), " ", "_")

	config := haEntityConfig{
		Name:                entityName,
		DeviceClass:         entityClass,
		StateTopic:          "homeassistant/sensor/" + deviceId + "/state",
		JsonAttributesTopic: "homeassistant/sensor/" + deviceId + "/attributes",
		UnitOfMeasure:       entityMeasure,
		ValueTemplate:       "{{ value_json." + jsonKey + "}}",
		UniqueId:            deviceId + "_" + jsonKey,
		ExpireAfter:         60 * 30, // 30 minutes
		StateClass:          "measurement",
		DisplayPrecision:    displayPrecision,
		Device: haDeviceConfig{
			Identifiers:  []string{deviceId},
			Name:         batteryName,
			Manufacturer: manufacturer,
			Model:        fmt.Sprintf("%.0f kWh", capacityKWh),
		},
	}

	configTopic := "homeassistant/sensor/" + deviceId + "_" + jsonKey + "/config"

	payload, err := json.Marshal(config)
	if err != nil {
		return err
	}

	s.Send(MQTTMessage{
		Topic:   configTopic,
		Payload: payload,
		QoS:     2,
		Retain:  true,
	})

	return nil
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
