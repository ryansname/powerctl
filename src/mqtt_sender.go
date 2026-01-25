package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
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
func (s *MQTTSender) CallService(domain, service, entityID string, data map[string]string) {
	payload := map[string]any{
		"domain":  domain,
		"service": service,
	}
	if entityID != "" {
		payload["entity_id"] = entityID
	}
	if data != nil {
		payload["data"] = data
	}
	payloadBytes, _ := json.Marshal(payload)

	s.ch <- MQTTMessage{
		Topic:   "nodered/proxy/call_service",
		Payload: payloadBytes,
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
		StateTopic:          "powerctl/sensor/" + deviceId + "/state",
		JsonAttributesTopic: "powerctl/sensor/" + deviceId + "/attributes",
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

// CreateDebugSensor creates a simple debug sensor via MQTT discovery
func (s *MQTTSender) CreateDebugSensor(sensorID, name, unit string, precision int) error {
	type haDeviceConfig struct {
		Identifiers  []string `json:"identifiers"`
		Name         string   `json:"name"`
		Manufacturer string   `json:"manufacturer,omitempty"`
	}

	type haEntityConfig struct {
		Name             string         `json:"name"`
		StateTopic       string         `json:"state_topic"`
		UnitOfMeasure    string         `json:"unit_of_measurement,omitempty"`
		UniqueId         string         `json:"unique_id"`
		StateClass       string         `json:"state_class,omitempty"`
		DisplayPrecision int            `json:"suggested_display_precision,omitempty"`
		Device           haDeviceConfig `json:"device"`
	}

	config := haEntityConfig{
		Name:             name,
		StateTopic:       "powerctl/sensor/" + sensorID + "/state",
		UnitOfMeasure:    unit,
		UniqueId:         sensorID,
		StateClass:       "measurement",
		DisplayPrecision: precision,
		Device: haDeviceConfig{
			Identifiers:  []string{"powerctl"},
			Name:         "Powerctl",
			Manufacturer: "DIY",
		},
	}

	configTopic := "homeassistant/sensor/" + sensorID + "/config"

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

// PublishDebugSensor publishes a value to a debug sensor.
// Uses powerctl/ prefix to avoid conflicts with HA statestream.
func (s *MQTTSender) PublishDebugSensor(sensorID string, value float64) {
	s.Send(MQTTMessage{
		Topic:   "powerctl/sensor/" + sensorID + "/state",
		Payload: []byte(strconv.FormatFloat(value, 'f', -1, 64)),
		QoS:     0,
		Retain:  false,
	})
}

// CreatePowerctlSwitch creates the powerctl_enabled switch via MQTT discovery
func (s *MQTTSender) CreatePowerctlSwitch() error {
	type haDeviceConfig struct {
		Identifiers  []string `json:"identifiers"`
		Name         string   `json:"name"`
		Manufacturer string   `json:"manufacturer,omitempty"`
	}

	type haSwitchConfig struct {
		Name         string         `json:"name"`
		StateTopic   string         `json:"state_topic"`
		CommandTopic string         `json:"command_topic"`
		UniqueId     string         `json:"unique_id"`
		Icon         string         `json:"icon,omitempty"`
		Optimistic   bool           `json:"optimistic"`
		Device       haDeviceConfig `json:"device"`
	}

	config := haSwitchConfig{
		Name:         "Enabled",
		StateTopic:   TopicPowerctlEnabledState,
		CommandTopic: "powerctl/switch/powerctl_enabled/set",
		UniqueId:     "powerctl_enabled",
		Icon:         "mdi:power",
		Optimistic:   true,
		Device: haDeviceConfig{
			Identifiers:  []string{"powerctl"},
			Name:         "Powerctl",
			Manufacturer: "Custom",
		},
	}

	payload, err := json.Marshal(config)
	if err != nil {
		return err
	}

	s.Send(MQTTMessage{
		Topic:   "homeassistant/switch/powerctl_enabled/config",
		Payload: payload,
		QoS:     2,
		Retain:  true,
	})

	return nil
}

// isDiscoveryTopic checks if a topic is an MQTT discovery config topic
func isDiscoveryTopic(topic string) bool {
	return strings.HasSuffix(topic, "/config")
}

// TopicPowerctlEnabledState is the state topic for the powerctl_enabled switch.
// Statestream publishes here, powerctl reads to check enabled state.
const TopicPowerctlEnabledState = "homeassistant/switch/powerctl_enabled/state"

// mqttSenderWorker handles outgoing MQTT messages with queuing and filtering
func mqttSenderWorker(
	ctx context.Context,
	outgoingChan <-chan MQTTMessage,
	clientChan <-chan mqtt.Client,
	dataChan <-chan DisplayData,
	forceEnable bool,
) {
	log.Println("MQTT sender worker started")

	var client mqtt.Client
	var messageQueue []MQTTMessage
	enabled := true // Default to enabled

	for {
		select {
		case data := <-dataChan:
			// Read enabled state using GetBoolean (parsed by statsWorker)
			newEnabled := data.GetBoolean(TopicPowerctlEnabledState)
			if newEnabled != enabled {
				log.Printf("Powerctl enabled: %v\n", newEnabled)
				enabled = newEnabled
			}

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
			// Check if message should be published
			isEnabled := forceEnable || enabled || isDiscoveryTopic(msg.Topic)
			if !isEnabled {
				log.Printf("Powerctl disabled, dropping message to %s\n", msg.Topic)
				continue
			}

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
