package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

func createBatteryEntity(
	outgoingChan chan<- MQTTMessage,
	batteryName string,   // "Battery 2" or "Battery 3"
	capacityKWh float64,  // 10.0 or 15.0
	manufacturer string,  // "SunnyTech Solar" or "Micromall"
	entityName, entityClass, entityMeasure, jsonKey, stateClass string,
	displayPrecision int,
) error {
	type Config struct {
		Name             string `json:"name,omitempty"`
		DeviceClass      string `json:"device_class"`
		StateTopic       string `json:"state_topic"`
		UnitOfMeasure    string `json:"unit_of_measurement,omitempty"`
		ValueTemplate    string `json:"value_template"`
		UniqueId         string `json:"unique_id"`
		ExpireAfter      uint   `json:"expire_after,omitempty"`
		StateClass       string `json:"state_class,omitempty"`
		DisplayPrecision int    `json:"suggested_display_precision,omitempty"`
		Device           struct {
			Identifiers  []string `json:"identifiers"`
			Name         string   `json:"name"`
			Manufacturer string   `json:"manufacturer,omitempty"`
			Model        string   `json:"model,omitempty"`
		} `json:"device"`
	}

	// Create unique device identifier from battery name
	deviceId := strings.ReplaceAll(strings.ToLower(batteryName), " ", "_")

	config := Config{}
	config.Name = entityName
	config.DeviceClass = entityClass
	config.StateTopic = "homeassistant/sensor/" + deviceId + "/state"
	config.UnitOfMeasure = entityMeasure
	config.ValueTemplate = "{{ value_json." + jsonKey + "}}"
	config.UniqueId = deviceId + "_" + jsonKey
	config.ExpireAfter = 60 * 30 // 30 minutes
	config.StateClass = stateClass
	config.DisplayPrecision = displayPrecision
	config.Device.Identifiers = []string{deviceId}
	config.Device.Name = batteryName
	config.Device.Manufacturer = manufacturer
	config.Device.Model = fmt.Sprintf("%.0f kWh", capacityKWh)

	configTopic := "homeassistant/sensor/" + deviceId + "_" + jsonKey + "/config"

	payloadString, err := json.Marshal(config)
	if err != nil {
		return err
	}

	outgoingChan <- MQTTMessage{
		Topic:   configTopic,
		Payload: payloadString,
		QoS:     2,
		Retain:  true,
	}

	return nil
}
