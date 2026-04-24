package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

type lastSentInfo struct {
	payload []byte
	sentAt  time.Time
}

const resendInterval = 5 * time.Minute

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
func (s *MQTTSender) CallService(domain, service, entityID string, data map[string]any) {
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

// CreateBatterySOCEntityFromCerbo creates a battery SOC entity that reads directly
// from a Cerbo GX MQTT topic ({"value": N} format) instead of powerctl state.
func (s *MQTTSender) CreateBatterySOCEntityFromCerbo(
	batteryName string,
	capacityKWh float64,
	manufacturer string,
	cerboSOCTopic string,
) error {
	type haDeviceConfig struct {
		Identifiers  []string `json:"identifiers"`
		Name         string   `json:"name"`
		Manufacturer string   `json:"manufacturer,omitempty"`
		Model        string   `json:"model,omitempty"`
	}

	type haEntityConfig struct {
		Name             string         `json:"name,omitempty"`
		DeviceClass      string         `json:"device_class"`
		StateTopic       string         `json:"state_topic"`
		UnitOfMeasure    string         `json:"unit_of_measurement,omitempty"`
		ValueTemplate    string         `json:"value_template"`
		UniqueId         string         `json:"unique_id"`
		ExpireAfter      uint           `json:"expire_after,omitempty"`
		StateClass       string         `json:"state_class,omitempty"`
		DisplayPrecision int            `json:"suggested_display_precision,omitempty"`
		Device           haDeviceConfig `json:"device"`
	}

	deviceId := strings.ReplaceAll(strings.ToLower(batteryName), " ", "_")

	config := haEntityConfig{
		Name:             "State of Charge",
		DeviceClass:      "battery",
		StateTopic:       cerboSOCTopic,
		UnitOfMeasure:    "%",
		ValueTemplate:    "{{ value_json.value }}",
		UniqueId:         deviceId + "_percentage",
		ExpireAfter:      60 * 30,
		StateClass:       "measurement",
		DisplayPrecision: 1,
		Device: haDeviceConfig{
			Identifiers:  []string{deviceId},
			Name:         batteryName,
			Manufacturer: manufacturer,
			Model:        fmt.Sprintf("%.0f kWh", capacityKWh),
		},
	}

	payload, err := json.Marshal(config)
	if err != nil {
		return err
	}

	s.Send(MQTTMessage{
		Topic:   "homeassistant/sensor/" + deviceId + "_percentage/config",
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
		Payload: []byte(strconv.FormatFloat(value, 'f', 1, 64)),
		QoS:     0,
		Retain:  false,
	})
}

// createSwitch creates a Home Assistant switch via MQTT discovery.
// NOTE: The stateTopic must also be added to selfPublishedBoolTopics in stats.go
// and expectedTopics in main.go, or startup will block/error on first run.
func (s *MQTTSender) createSwitch(uniqueID, name, icon, stateTopic string) error {
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
		Name:         name,
		StateTopic:   stateTopic,
		CommandTopic: "powerctl/switch/" + uniqueID + "/set",
		UniqueId:     uniqueID,
		Icon:         icon,
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
		Topic:   "homeassistant/switch/" + uniqueID + "/config",
		Payload: payload,
		QoS:     2,
		Retain:  true,
	})

	return nil
}

// CreatePowerctlSwitch creates the powerctl_enabled switch via MQTT discovery
func (s *MQTTSender) CreatePowerctlSwitch() error {
	return s.createSwitch("powerctl_enabled", "Enabled", "mdi:power", TopicPowerctlEnabledState)
}

// CreatePowerhouseInvertersSwitch creates the powerctl_inverter_enabled switch via MQTT discovery
func (s *MQTTSender) CreatePowerhouseInvertersSwitch() error {
	return s.createSwitch("powerctl_inverter_enabled", "Inverter Enabled", "mdi:power-plug", TopicPowerhouseInvertersEnabledState)
}

// CreatePW2DischargeSwitch creates the powerctl_pw2_discharge switch via MQTT discovery
func (s *MQTTSender) CreatePW2DischargeSwitch() error {
	return s.createSwitch("powerctl_pw2_discharge", "PW2 Discharge", "mdi:battery-arrow-down", TopicPW2DischargeState)
}

// CreateExpectingPowerCutsSwitch creates the expecting power cuts switch via MQTT discovery
func (s *MQTTSender) CreateExpectingPowerCutsSwitch() error {
	return s.createSwitch("powerctl_expecting_power_cuts", "Expecting Power Cuts", "mdi:transmission-tower-off", TopicExpectingPowerCutsState)
}

// CreateDynamicAutoSwitch creates the powerctl_dynamic_auto switch via MQTT discovery.
// When on, the dynamic controller calculates the setpoint automatically.
// When off, the user controls the setpoint via the HA number entity.
func (s *MQTTSender) CreateDynamicAutoSwitch() error {
	return s.createSwitch("powerctl_dynamic_auto", "Dynamic Auto", "mdi:robot", TopicDynamicAutoState)
}

// CreateCarChargingSwitch creates the powerctl_car_charging switch via MQTT discovery.
// When on, the dynamic controller pushes Multiplus discharge to its safe maximum to supply
// the car charger from Battery 3 / solar instead of grid.
func (s *MQTTSender) CreateCarChargingSwitch() error {
	return s.createSwitch("powerctl_car_charging", "Car Charging", "mdi:car-electric", TopicCarChargingEnabledState)
}

// CreateCarChargingBattery3CutoffEntity creates the Battery 3 SOC cutoff number entity
// for the car-charging feature. Below this SOC, car charging is suppressed and auto-disabled.
func (s *MQTTSender) CreateCarChargingBattery3CutoffEntity() error {
	type haDeviceConfig struct {
		Identifiers  []string `json:"identifiers"`
		Name         string   `json:"name"`
		Manufacturer string   `json:"manufacturer,omitempty"`
	}

	type haNumberConfig struct {
		Name          string         `json:"name"`
		UniqueId      string         `json:"unique_id"`
		CommandTopic  string         `json:"command_topic"`
		UnitOfMeasure string         `json:"unit_of_measurement"`
		Min           float64        `json:"min"`
		Max           float64        `json:"max"`
		Step          float64        `json:"step"`
		Mode          string         `json:"mode"`
		Icon          string         `json:"icon,omitempty"`
		Device        haDeviceConfig `json:"device"`
	}

	config := haNumberConfig{
		Name:         "Car Charging B3 Cutoff",
		UniqueId:     "powerctl_car_charging_battery3_cutoff",
		CommandTopic: TopicCarChargingBattery3CutoffCmd,
		UnitOfMeasure: "%",
		Min:           0,
		Max:           100,
		Step:          1,
		Mode:          "slider",
		Icon:          "mdi:battery-alert",
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
		Topic:   "homeassistant/number/powerctl_car_charging_battery3_cutoff/config",
		Payload: payload,
		QoS:     2,
		Retain:  true,
	})

	return nil
}

// CreateInverter10ACSetpointEntity creates the Multiplus II AC setpoint number entity via MQTT discovery
func (s *MQTTSender) CreateInverter10ACSetpointEntity() error {
	type haDeviceConfig struct {
		Identifiers []string `json:"identifiers"`
		Name        string   `json:"name"`
		Model       string   `json:"model,omitempty"`
	}

	type haNumberConfig struct {
		Name          string         `json:"name"`
		UniqueId      string         `json:"unique_id"`
		StateTopic    string         `json:"state_topic"`
		ValueTemplate string         `json:"value_template"`
		CommandTopic  string         `json:"command_topic"`
		UnitOfMeasure string         `json:"unit_of_measurement"`
		Min           float64        `json:"min"`
		Max           float64        `json:"max"`
		Step          float64        `json:"step"`
		Mode          string         `json:"mode"`
		Device        haDeviceConfig `json:"device"`
	}

	config := haNumberConfig{
		Name:          "AC Setpoint",
		UniqueId:      "powerhouse_inverter_10_ac_setpoint",
		StateTopic:    TopicMultiplusSetpointRead,
		ValueTemplate: "{{ value_json.value }}",
		CommandTopic:  TopicInverter10SetpointCmd,
		UnitOfMeasure: "W",
		Min:           -3000,
		Max:           3500,
		Step:          10,
		Mode:          "slider",
		Device: haDeviceConfig{
			Identifiers: []string{"powerhouse_inverter_10"},
			Name:        "Powerhouse Inverter 10",
			Model:       "Multiplus II 48/5000/70",
		},
	}

	payload, err := json.Marshal(config)
	if err != nil {
		return err
	}

	s.Send(MQTTMessage{
		Topic:   "homeassistant/number/powerhouse_inverter_10_ac_setpoint/config",
		Payload: payload,
		QoS:     2,
		Retain:  true,
	})

	return nil
}

// CreateMultiplusACPowerEntity creates the Multiplus II AC output power sensor entity via MQTT discovery
func (s *MQTTSender) CreateMultiplusACPowerEntity() error {
	type haDeviceConfig struct {
		Identifiers []string `json:"identifiers"`
		Name        string   `json:"name"`
		Model       string   `json:"model,omitempty"`
	}

	type haSensorConfig struct {
		Name          string         `json:"name"`
		UniqueId      string         `json:"unique_id"`
		StateTopic    string         `json:"state_topic"`
		ValueTemplate string         `json:"value_template"`
		UnitOfMeasure string         `json:"unit_of_measurement"`
		DeviceClass   string         `json:"device_class"`
		StateClass    string         `json:"state_class"`
		Device        haDeviceConfig `json:"device"`
	}

	config := haSensorConfig{
		Name:          "AC Power",
		UniqueId:      "powerhouse_inverter_10_ac_power",
		StateTopic:    TopicMultiplusACPower,
		ValueTemplate: "{{ value_json.value }}",
		UnitOfMeasure: "W",
		DeviceClass:   "power",
		StateClass:    "measurement",
		Device: haDeviceConfig{
			Identifiers: []string{"powerhouse_inverter_10"},
			Name:        "Powerhouse Inverter 10",
			Model:       "Multiplus II 48/5000/70",
		},
	}

	payload, err := json.Marshal(config)
	if err != nil {
		return err
	}

	s.Send(MQTTMessage{
		Topic:   "homeassistant/sensor/powerhouse_inverter_10_ac_power/config",
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

// TopicPowerhouseInvertersEnabledState is the state topic for the powerctl_inverter_enabled switch.
// Controls whether unifiedInverterEnabler messages are forwarded.
const TopicPowerhouseInvertersEnabledState = "homeassistant/switch/powerctl_inverter_enabled/state"

// mqttSenderWorker handles outgoing MQTT messages with queuing and filtering
func mqttSenderWorker(
	ctx context.Context,
	outgoingChan <-chan MQTTMessage,
	clientChan <-chan mqtt.Client,
	dataChan <-chan DisplayData,
	forceEnable bool,
	multiplusOnly bool,
) {
	log.Println("MQTT sender worker started")

	var client mqtt.Client
	var messageQueue []MQTTMessage
	enabled := true // Default to enabled
	lastSent := make(map[string]lastSentInfo)

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
					lastSent[msg.Topic] = lastSentInfo{
						payload: bytes.Clone(msg.Payload),
						sentAt:  time.Now(),
					}
				}
				messageQueue = nil // Clear the queue
				if queuedCount > 0 {
					log.Printf("MQTT sender worker processed %d queued messages\n", queuedCount)
				}
			}

		case msg := <-outgoingChan:
			// Multiplus-only isolation: drop everything outside the Cerbo namespace,
			// except discovery config topics which register HA entities.
			if multiplusOnly && !strings.HasPrefix(msg.Topic, "powerhouse_3/") && !isDiscoveryTopic(msg.Topic) {
				continue
			}

			// Check if message should be published
			isEnabled := forceEnable || enabled || isDiscoveryTopic(msg.Topic)
			if !isEnabled {
				log.Printf("Powerctl disabled, dropping message to %s\n", msg.Topic)
				continue
			}

			// Change detection: skip if payload unchanged and recently sent.
			// Service calls and Victron read/write topics are commands that must always be forwarded.
			if msg.Topic != "nodered/proxy/call_service" &&
				!strings.HasPrefix(msg.Topic, "powerhouse_3/W/") &&
				!strings.HasPrefix(msg.Topic, "powerhouse_3/R/") {
				if last, ok := lastSent[msg.Topic]; ok {
					if bytes.Equal(last.payload, msg.Payload) && time.Since(last.sentAt) < resendInterval {
						continue
					}
				}
			}

			if client != nil && client.IsConnected() {
				// We have a client, publish immediately
				token := client.Publish(msg.Topic, msg.QoS, msg.Retain, msg.Payload)
				token.Wait()
				if token.Error() != nil {
					log.Printf("Failed to publish to %s: %v\n", msg.Topic, token.Error())
				}
				lastSent[msg.Topic] = lastSentInfo{
					payload: bytes.Clone(msg.Payload),
					sentAt:  time.Now(),
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
