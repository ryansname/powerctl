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

// SafeGo launches a goroutine with panic recovery.
// If the goroutine panics, the context is cancelled and the panic is logged.
func SafeGo(ctx context.Context, cancel context.CancelFunc, name string, fn func(ctx context.Context)) {
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

	fmt.Print("\033[H\033[2J") // Clear screen
	fmt.Println("================================================================================")
	fmt.Println("Power Monitoring Statistics")
	fmt.Println("================================================================================")
	fmt.Println()

	// Display float sensors with statistics
	for _, topic := range floatTopics {
		data := topicData[topic].(*FloatTopicData)
		fmt.Printf("Topic: %s\n", topic)
		fmt.Printf("  Current:  %-10.2f\n", data.Current)
		fmt.Printf("            %-12s %-12s %-12s\n", "Average", "Min", "Max")
		fmt.Printf("  1  min:   %-12.2f %-12.2f %-12.2f\n", data.Average._1, data.Min._1, data.Max._1)
		fmt.Printf("  5  min:   %-12.2f %-12.2f %-12.2f\n", data.Average._5, data.Min._5, data.Max._5)
		fmt.Printf("  15 min:   %-12.2f %-12.2f %-12.2f\n", data.Average._15, data.Min._15, data.Max._15)
		fmt.Println()
	}

	// Display string sensors
	if len(stringTopics) > 0 {
		fmt.Println("String Sensors:")
		fmt.Println()
		for _, topic := range stringTopics {
			data := topicData[topic].(*StringTopicData)
			fmt.Printf("Topic: %s\n", topic)
			fmt.Printf("  Current:  %-20s\n", data.Current)
			fmt.Println()
		}
	}

	fmt.Println("================================================================================")
}

// mqttWorker manages MQTT connection and forwards messages to a channel
func mqttWorker(ctx context.Context, broker string, topics []string, username, password string, msgChan chan<- SensorMessage) {
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
		"homeassistant/sensor/powerhouse_inverter_5_switch_0_energy/state",
		"homeassistant/sensor/powerhouse_inverter_6_switch_0_energy/state",
		"homeassistant/sensor/powerhouse_inverter_7_switch_0_energy/state",
		"homeassistant/sensor/powerhouse_inverter_8_switch_0_energy/state",
		"homeassistant/sensor/powerhouse_inverter_9_switch_0_energy/state",
		"homeassistant/sensor/solar_3_solar_energy/state",
		"homeassistant/sensor/solar_4_solar_energy/state",
		"homeassistant/sensor/solar_3_charge_state/state",
		"homeassistant/sensor/solar_4_charge_state/state",
	}

	// Create message channel for communication between workers
	msgChan := make(chan SensorMessage, 10)
	displayChan := make(chan DisplayData, 10)

	// Launch display worker
	SafeGo(ctx, cancel, "display-worker", func(ctx context.Context) {
		displayWorker(ctx, displayChan)
	})
	log.Println("Display worker started")

	// Launch stats worker
	SafeGo(ctx, cancel, "stats-worker", func(ctx context.Context) {
		statsWorker(ctx, msgChan, displayChan)
	})
	log.Println("Stats worker started")

	// Launch MQTT worker
	SafeGo(ctx, cancel, "mqtt-worker", func(ctx context.Context) {
		mqttWorker(ctx, "homeassistant.lan", topics, mqttUsername, mqttPassword, msgChan)
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
