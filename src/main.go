package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/joho/godotenv"
)

// SensorMessage represents an MQTT message with topic and value
type SensorMessage struct {
	Topic string
	Value string
}

// DisplayData holds all data needed for display
type DisplayData struct {
	TopicData map[string]any
}

// GetFloat extracts FloatTopicData from DisplayData
// Returns a zero-valued FloatTopicData if topic doesn't exist or isn't a float topic
func (d *DisplayData) GetFloat(topic string) *FloatTopicData {
	if td, ok := d.TopicData[topic].(*FloatTopicData); ok {
		return td
	}
	return &FloatTopicData{}
}

// GetString extracts a string value from DisplayData
func (d *DisplayData) GetString(topic string) string {
	if td, ok := d.TopicData[topic].(*StringTopicData); ok {
		return td.Current
	}
	return ""
}

// SumTopics calculates the sum of all specified topics
func (d *DisplayData) SumTopics(topics []string) float64 {
	var total float64
	for _, topic := range topics {
		total += d.GetFloat(topic).Current
	}
	return total
}

// buildTopicsList creates the MQTT subscription list from battery configs
func buildTopicsList(batteries []BatteryConfig) []string {
	var topics []string //nolint:prealloc // small slice, not worth preallocating
	for _, b := range batteries {
		topics = append(topics, b.InflowTopics...)
		topics = append(topics, b.OutflowTopics...)
		topics = append(topics, b.ChargeStateTopic)
		topics = append(topics, b.BatteryVoltageTopic)
		topics = append(topics, b.CalibrationTopics.Inflows, b.CalibrationTopics.Outflows)
	}
	return topics
}

// SafeGo launches a goroutine with panic recovery.
// If the goroutine panics, the context is cancelled and the panic is logged.
func SafeGo(
	ctx context.Context,
	cancel context.CancelFunc,
	name string,
	fn func(ctx context.Context),
) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("Panic in %s: %v\n", name, r)
				cancel()
			}
		}()
		fn(ctx)
	}()
}

func main() {
	log.Println("Starting powerctl...")

	// Load .env file
	if err := godotenv.Load(); err != nil {
		log.Printf("Warning: Error loading .env file: %v\n", err)
	}

	// Get MQTT credentials from environment
	mqttUsername := os.Getenv("MQTT_USERNAME")
	mqttPassword := os.Getenv("MQTT_PASSWORD")

	if mqttUsername == "" || mqttPassword == "" {
		log.Fatal("MQTT_USERNAME and MQTT_PASSWORD must be set in .env file")
	}

	// Create context for lifecycle management
	ctx, cancel := context.WithCancel(context.Background())

	// Define battery configurations
	battery2 := BatteryConfig{
		Name:         "Battery 2",
		CapacityKWh:  10.0,
		Manufacturer: "SunnyTech Solar",
		InflowTopics: []string{
			"homeassistant/sensor/solar_5_total_energy/state",
		},
		OutflowTopics: []string{
			"homeassistant/sensor/powerhouse_inverter_1_switch_0_energy/state",
			"homeassistant/sensor/powerhouse_inverter_2_switch_0_energy/state",
			"homeassistant/sensor/powerhouse_inverter_3_switch_0_energy/state",
			"homeassistant/sensor/powerhouse_inverter_4_switch_0_energy/state",
		},
		ChargeStateTopic:    "homeassistant/sensor/solar_5_charge_state/state",
		BatteryVoltageTopic: "homeassistant/sensor/solar_5_battery_voltage/state",
		CalibrationTopics: CalibrationTopics{
			Inflows:  "homeassistant/sensor/battery_2_state_of_charge/calibration_inflows",
			Outflows: "homeassistant/sensor/battery_2_state_of_charge/calibration_outflows",
		},
		HighVoltageThreshold: 53.6,
		FloatChargeState:     "Float Charging",
		ConversionLossRate:   0.10,
		InverterSwitchIDs: []string{
			"switch.powerhouse_inverter_1_switch_0",
			"switch.powerhouse_inverter_2_switch_0",
			"switch.powerhouse_inverter_3_switch_0",
			"switch.powerhouse_inverter_4_switch_0",
		},
	}

	battery3 := BatteryConfig{
		Name:         "Battery 3",
		CapacityKWh:  15.0,
		Manufacturer: "Micromall",
		InflowTopics: []string{
			"homeassistant/sensor/solar_3_total_energy/state",
			"homeassistant/sensor/solar_4_total_energy/state",
		},
		OutflowTopics: []string{
			"homeassistant/sensor/powerhouse_inverter_5_switch_0_energy/state",
			"homeassistant/sensor/powerhouse_inverter_6_switch_0_energy/state",
			"homeassistant/sensor/powerhouse_inverter_7_switch_0_energy/state",
			"homeassistant/sensor/powerhouse_inverter_8_switch_0_energy/state",
			"homeassistant/sensor/powerhouse_inverter_9_switch_0_energy/state",
		},
		ChargeStateTopic:    "homeassistant/sensor/solar_3_charge_state/state",
		BatteryVoltageTopic: "homeassistant/sensor/solar_3_battery_voltage/state",
		CalibrationTopics: CalibrationTopics{
			Inflows:  "homeassistant/sensor/battery_3_state_of_charge/calibration_inflows",
			Outflows: "homeassistant/sensor/battery_3_state_of_charge/calibration_outflows",
		},
		HighVoltageThreshold: 53.6,
		FloatChargeState:     "Float Charging",
		ConversionLossRate:   0.10,
		InverterSwitchIDs: []string{
			"switch.powerhouse_inverter_5_switch_0",
			"switch.powerhouse_inverter_6_switch_0",
			"switch.powerhouse_inverter_7_switch_0",
			"switch.powerhouse_inverter_8_switch_0",
			"switch.powerhouse_inverter_9_switch_0",
		},
	}

	batteries := []BatteryConfig{battery2, battery3}

	// Build topics list from battery configs
	topics := buildTopicsList(batteries)

	// Create channels for communication between workers
	msgChan := make(chan SensorMessage, 10)
	statsChan := make(chan DisplayData, 10)
	mqttOutgoingChan := make(chan MQTTMessage, 100) // Larger buffer for queuing
	mqttClientChan := make(chan mqtt.Client, 1)     // Buffered to prevent blocking onConnect

	// Launch MQTT sender worker (receives client updates via channel)
	SafeGo(ctx, cancel, "mqtt-sender-worker", func(ctx context.Context) {
		mqttSenderWorker(ctx, mqttOutgoingChan, mqttClientChan)
	})
	log.Println("MQTT sender worker started")

	// Create MQTT sender for workers
	mqttSender := NewMQTTSender(mqttOutgoingChan)

	// Create Home Assistant battery entities
	log.Println("Creating Home Assistant entities...")

	for _, b := range batteries {
		err := mqttSender.CreateBatteryEntity(
			b.Name, b.CapacityKWh, b.Manufacturer,
			"State of Charge", "battery", "%", "percentage", 1,
		)
		if err != nil {
			cancel()
			log.Fatalf("Failed to create %s State of Charge entity: %v", b.Name, err)
		}

		err = mqttSender.CreateBatteryEntity(
			b.Name, b.CapacityKWh, b.Manufacturer,
			"Available Energy", "energy", "Wh", "available_wh", 0,
		)
		if err != nil {
			cancel()
			log.Fatalf("Failed to create %s Available Energy entity: %v", b.Name, err)
		}
	}

	log.Println("Home Assistant entities created")

	// Launch stats worker (produces statistics)
	SafeGo(ctx, cancel, "stats-worker", func(ctx context.Context) {
		statsWorker(ctx, msgChan, statsChan, topics)
	})
	log.Println("Stats worker started")

	// Low voltage threshold for protection
	lowVoltageThreshold := 50.75

	// Launch battery workers and collect downstream channels
	var downstreamChans []chan<- DisplayData //nolint:prealloc // small slice
	for _, b := range batteries {
		calibChan := make(chan DisplayData, 10)
		socChan := make(chan DisplayData, 10)
		lowVoltageChan := make(chan DisplayData, 10)
		downstreamChans = append(downstreamChans, calibChan, socChan, lowVoltageChan)

		// Launch calibration worker
		calibConfig := b.CalibConfig()
		SafeGo(ctx, cancel, b.Name+"-calib", func(ctx context.Context) {
			batteryCalibWorker(ctx, calibChan, calibConfig, mqttSender)
		})
		log.Printf("%s calibration worker started\n", b.Name)

		// Launch SOC worker
		socConfig := b.SOCConfig()
		SafeGo(ctx, cancel, b.Name+"-soc", func(ctx context.Context) {
			batterySOCWorker(ctx, socChan, socConfig, mqttSender)
		})
		log.Printf("%s SOC worker started\n", b.Name)

		// Launch low voltage protection worker
		lowVoltageConfig := b.LowVoltageProtectionConfig(lowVoltageThreshold)
		SafeGo(ctx, cancel, b.Name+"-low-voltage", func(ctx context.Context) {
			lowVoltageWorker(ctx, lowVoltageChan, lowVoltageConfig, mqttSender)
		})
	}

	// Launch broadcast worker (fans out to all downstream workers)
	SafeGo(ctx, cancel, "broadcast-worker", func(ctx context.Context) {
		broadcastWorker(ctx, statsChan, downstreamChans)
	})
	log.Println("Broadcast worker started")

	// Launch MQTT worker
	SafeGo(ctx, cancel, "mqtt-worker", func(ctx context.Context) {
		mqttWorker(ctx, "homeassistant.lan", topics, mqttUsername, mqttPassword, msgChan, mqttClientChan)
	})
	log.Println("MQTT worker started")

	// Wait for interrupt signal or context cancellation (from panic)
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	select {
	case <-sigChan:
		log.Println("\nShutting down...")
	case <-ctx.Done():
		log.Println("\nShutting down due to error...")
	}
	cancel()
}
