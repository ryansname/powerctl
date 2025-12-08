package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"time"

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

// GetFloat extracts a float value from DisplayData
func (d *DisplayData) GetFloat(topic string) float64 {
	if td, ok := d.TopicData[topic].(*FloatTopicData); ok {
		return td.Current
	}
	return 0
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
		total += d.GetFloat(topic)
	}
	return total
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

// displayWorker receives display data and renders it to stdout
func displayWorker(ctx context.Context, displayChan <-chan DisplayData) {
	for {
		select {
		case data := <-displayChan:
			displayAllStats(data.TopicData)
		case <-ctx.Done():
			return
		}
	}
}

// displayAllStats formats and prints statistics for all topics to stdout
func displayAllStats(topicData map[string]any) {
	// Separate float and string topics and get sorted topic names
	var floatTopics []string
	var stringTopics []string
	for topic, data := range topicData {
		switch data.(type) {
		case *FloatTopicData:
			floatTopics = append(floatTopics, topic)
		case *StringTopicData:
			stringTopics = append(stringTopics, topic)
		}
	}
	sort.Strings(floatTopics)
	sort.Strings(stringTopics)

	// fmt.Print("\033[H\033[2J") // Clear screen
	// fmt.Println("Power Monitoring")
	// fmt.Println()

	// // Display float sensors - just current and 5-min average
	// if len(floatTopics) > 0 {
	// 	fmt.Printf("%-35s %10s %10s\n", "Sensor", "Current", "5m Avg")
	// 	fmt.Println(strings.Repeat("-", 57))
	// 	for _, topic := range floatTopics {
	// 		data := topicData[topic].(*FloatTopicData)
	// 		fmt.Printf("%-35s %10.2f %10.2f\n", extractShortName(topic), data.Current, data.Average._5)
	// 	}
	// 	fmt.Println()
	// }

	// // Display string sensors - compact format
	// if len(stringTopics) > 0 {
	// 	fmt.Printf("%-35s %s\n", "Sensor", "Value")
	// 	fmt.Println(strings.Repeat("-", 57))
	// 	for _, topic := range stringTopics {
	// 		data := topicData[topic].(*StringTopicData)
	// 		fmt.Printf("%-35s %s\n", extractShortName(topic), data.Current)
	// 	}
	// 	fmt.Println()
	// }
}

// mqttWorker manages MQTT connection and forwards messages to a channel
func mqttWorker(
	ctx context.Context,
	broker string,
	topics []string,
	username, password string,
	msgChan chan<- SensorMessage,
	clientChan chan<- mqtt.Client,
) {
	// Connect to MQTT broker
	opts := mqtt.NewClientOptions()
	opts.AddBroker(fmt.Sprintf("tcp://%s:1883", broker))
	opts.SetClientID("powerctl")
	opts.SetUsername(username)
	opts.SetPassword(password)
	opts.SetAutoReconnect(true)
	opts.SetConnectRetryInterval(5 * time.Second)

	// Set up connection lost handler
	opts.SetConnectionLostHandler(func(client mqtt.Client, err error) {
		log.Printf("MQTT connection lost: %v\n", err)
	})

	// Set up connection handler
	opts.SetOnConnectHandler(func(client mqtt.Client) {
		log.Printf("Connected to MQTT broker at %s\n", broker)

		// Send the new client to the sender worker
		select {
		case clientChan <- client:
			log.Println("Sent new MQTT client to sender worker")
		case <-ctx.Done():
			return
		}

		// Subscribe to all topics
		for _, topic := range topics {
			token := client.Subscribe(topic, 0, func(client mqtt.Client, msg mqtt.Message) {
				// Forward message to stats worker via channel
				sensorMsg := SensorMessage{
					Topic: msg.Topic(),
					Value: string(msg.Payload()),
				}
				select {
				case msgChan <- sensorMsg:
				case <-ctx.Done():
					return
				}
			})

			if token.Wait() && token.Error() != nil {
				log.Printf("Failed to subscribe to topic %s: %v\n", topic, token.Error())
			} else {
				log.Printf("Subscribed to topic: %s\n", topic)
			}
		}
	})

	client := mqtt.NewClient(opts)

	// Connect to broker
	log.Printf("Connecting to MQTT broker at %s...\n", broker)
	if token := client.Connect(); token.Wait() && token.Error() != nil {
		log.Printf("Failed to connect to MQTT broker: %v\n", token.Error())
		return
	}

	// Keep worker alive until context is done
	<-ctx.Done()

	// Cleanup
	if client.IsConnected() {
		client.Disconnect(250)
		log.Println("Disconnected from MQTT broker")
	}
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
	defer cancel()

	// Define topics to monitor
	topics := []string{
		// Battery 2 outflows (inverters 1-4)
		"homeassistant/sensor/powerhouse_inverter_1_switch_0_energy/state",
		"homeassistant/sensor/powerhouse_inverter_2_switch_0_energy/state",
		"homeassistant/sensor/powerhouse_inverter_3_switch_0_energy/state",
		"homeassistant/sensor/powerhouse_inverter_4_switch_0_energy/state",
		// Battery 3 outflows (inverters 5-9)
		"homeassistant/sensor/powerhouse_inverter_5_switch_0_energy/state",
		"homeassistant/sensor/powerhouse_inverter_6_switch_0_energy/state",
		"homeassistant/sensor/powerhouse_inverter_7_switch_0_energy/state",
		"homeassistant/sensor/powerhouse_inverter_8_switch_0_energy/state",
		"homeassistant/sensor/powerhouse_inverter_9_switch_0_energy/state",
		// Battery 2 inflow and monitoring
		"homeassistant/sensor/solar_5_total_energy/state",
		"homeassistant/sensor/solar_5_charge_state/state",
		"homeassistant/sensor/solar_5_battery_voltage/state",
		// Battery 3 inflows and monitoring
		"homeassistant/sensor/solar_3_total_energy/state",
		"homeassistant/sensor/solar_4_total_energy/state",
		"homeassistant/sensor/solar_3_charge_state/state",
		"homeassistant/sensor/solar_3_battery_voltage/state",
		// Battery 2 calibration statestream topics
		"homeassistant/sensor/battery_2_state_of_charge/calibration_inflows",
		"homeassistant/sensor/battery_2_state_of_charge/calibration_outflows",
		// Battery 3 calibration statestream topics
		"homeassistant/sensor/battery_3_state_of_charge/calibration_inflows",
		"homeassistant/sensor/battery_3_state_of_charge/calibration_outflows",
	}

	// Create channels for communication between workers
	msgChan := make(chan SensorMessage, 10)
	statsChan := make(chan DisplayData, 10)
	displayChan := make(chan DisplayData, 10)
	battery2CalibChan := make(chan DisplayData, 10)
	battery3CalibChan := make(chan DisplayData, 10)
	battery2SOCChan := make(chan DisplayData, 10)
	battery3SOCChan := make(chan DisplayData, 10)
	mqttOutgoingChan := make(chan MQTTMessage, 100) // Larger buffer for queuing
	mqttClientChan := make(chan mqtt.Client, 1)     // Buffered to prevent blocking onConnect

	// Launch MQTT sender worker (receives client updates via channel)
	SafeGo(ctx, cancel, "mqtt-sender-worker", func(ctx context.Context) {
		mqttSenderWorker(ctx, mqttOutgoingChan, mqttClientChan)
	})
	log.Println("MQTT sender worker started")

	// Create Home Assistant battery entities
	log.Println("Creating Home Assistant entities...")

	// Battery 2 entities
	err := createBatteryEntity(
		mqttOutgoingChan, "Battery 2", 10.0, "SunnyTech Solar",
		"State of Charge", "battery", "%", "percentage", "measurement", 1,
	)
	if err != nil {
		log.Fatalf("Failed to create Battery 2 State of Charge entity: %v", err)
	}

	err = createBatteryEntity(
		mqttOutgoingChan, "Battery 2", 10.0, "SunnyTech Solar",
		"Available Energy", "energy", "Wh", "available_wh", "measurement", 0,
	)
	if err != nil {
		log.Fatalf("Failed to create Battery 2 Available Energy entity: %v", err)
	}

	// Battery 3 entities
	err = createBatteryEntity(
		mqttOutgoingChan, "Battery 3", 15.0, "Micromall",
		"State of Charge", "battery", "%", "percentage", "measurement", 1,
	)
	if err != nil {
		log.Fatalf("Failed to create Battery 3 State of Charge entity: %v", err)
	}

	err = createBatteryEntity(
		mqttOutgoingChan, "Battery 3", 15.0, "Micromall",
		"Available Energy", "energy", "Wh", "available_wh", "measurement", 0,
	)
	if err != nil {
		log.Fatalf("Failed to create Battery 3 Available Energy entity: %v", err)
	}

	log.Println("Home Assistant entities created")

	// Launch stats worker (produces statistics)
	SafeGo(ctx, cancel, "stats-worker", func(ctx context.Context) {
		statsWorker(ctx, msgChan, statsChan, topics)
	})
	log.Println("Stats worker started")

	// Launch downstream workers
	SafeGo(ctx, cancel, "display-worker", func(ctx context.Context) {
		displayWorker(ctx, displayChan)
	})
	log.Println("Display worker started")

	// Configure and launch battery 2 calibration worker
	battery2CalibConfig := BatteryCalibConfig{
		Name:                "Battery 2",
		ChargeStateTopic:    "homeassistant/sensor/solar_5_charge_state/state",
		BatteryVoltageTopic: "homeassistant/sensor/solar_5_battery_voltage/state",
		InflowTopics:        []string{"homeassistant/sensor/solar_5_total_energy/state"},
		OutflowTopics: []string{
			"homeassistant/sensor/powerhouse_inverter_1_switch_0_energy/state",
			"homeassistant/sensor/powerhouse_inverter_2_switch_0_energy/state",
			"homeassistant/sensor/powerhouse_inverter_3_switch_0_energy/state",
			"homeassistant/sensor/powerhouse_inverter_4_switch_0_energy/state",
		},
		HighVoltageThreshold: 53.6,
		FloatChargeState:     "Float Charging",
	}
	SafeGo(ctx, cancel, "battery-2-calib", func(ctx context.Context) {
		batteryCalibWorker(ctx, battery2CalibChan, battery2CalibConfig, mqttOutgoingChan)
	})
	log.Println("Battery 2 calibration worker started")

	// Configure and launch battery 2 SOC worker
	battery2SOCConfig := BatterySOCConfig{
		Name:         "Battery 2",
		CapacityKWh:  10.0,
		InflowTopics: []string{"homeassistant/sensor/solar_5_total_energy/state"},
		OutflowTopics: []string{
			"homeassistant/sensor/powerhouse_inverter_1_switch_0_energy/state",
			"homeassistant/sensor/powerhouse_inverter_2_switch_0_energy/state",
			"homeassistant/sensor/powerhouse_inverter_3_switch_0_energy/state",
			"homeassistant/sensor/powerhouse_inverter_4_switch_0_energy/state",
		},
		CalibrationTopics: CalibrationTopics{
			Inflows:  "homeassistant/sensor/battery_2_state_of_charge/calibration_inflows",
			Outflows: "homeassistant/sensor/battery_2_state_of_charge/calibration_outflows",
		},
		ConversionLossRate: 0.02,
	}
	SafeGo(ctx, cancel, "battery-2-soc", func(ctx context.Context) {
		batterySOCWorker(ctx, battery2SOCChan, battery2SOCConfig, mqttOutgoingChan)
	})
	log.Println("Battery 2 SOC worker started")

	// Configure and launch battery 3 calibration worker
	battery3CalibConfig := BatteryCalibConfig{
		Name:                "Battery 3",
		ChargeStateTopic:    "homeassistant/sensor/solar_3_charge_state/state",
		BatteryVoltageTopic: "homeassistant/sensor/solar_3_battery_voltage/state",
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
		HighVoltageThreshold: 53.6,
		FloatChargeState:     "Float Charging",
	}
	SafeGo(ctx, cancel, "battery-3-calib", func(ctx context.Context) {
		batteryCalibWorker(ctx, battery3CalibChan, battery3CalibConfig, mqttOutgoingChan)
	})
	log.Println("Battery 3 calibration worker started")

	// Configure and launch battery 3 SOC worker
	battery3SOCConfig := BatterySOCConfig{
		Name:        "Battery 3",
		CapacityKWh: 15.0,
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
		CalibrationTopics: CalibrationTopics{
			Inflows:  "homeassistant/sensor/battery_3_state_of_charge/calibration_inflows",
			Outflows: "homeassistant/sensor/battery_3_state_of_charge/calibration_outflows",
		},
		ConversionLossRate: 0.02,
	}
	SafeGo(ctx, cancel, "battery-3-soc", func(ctx context.Context) {
		batterySOCWorker(ctx, battery3SOCChan, battery3SOCConfig, mqttOutgoingChan)
	})
	log.Println("Battery 3 SOC worker started")

	// Collect all downstream worker channels for fan-out
	downstreamChans := []chan<- DisplayData{
		displayChan,
		battery2CalibChan,
		battery3CalibChan,
		battery2SOCChan,
		battery3SOCChan,
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
}
