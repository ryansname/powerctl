package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sort"
	"strconv"
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

// Reading represents a timestamped sensor reading
type Reading struct {
	Value     float64
	Timestamp time.Time
}

// TimeWindows holds values across 1, 5, and 15 minute windows
type TimeWindows struct {
	_1  float64
	_5  float64
	_15 float64
}

// Statistics holds current and historical statistics
type Statistics struct {
	Topic    string
	Current  float64
	Previous float64
	Average  TimeWindows
	Min      TimeWindows
	Max      TimeWindows
}

// calculateTimeWeightedStats computes time-weighted statistics for a time window
// Each reading is weighted by the duration it was active (time until next reading)
func calculateTimeWeightedStats(readings []Reading, windowDuration time.Duration) (avg, min, max float64) {
	if len(readings) == 0 {
		return 0, 0, 0
	}

	now := time.Now()
	cutoff := now.Add(-windowDuration)

	// Filter readings within the window
	var windowReadings []Reading
	for _, r := range readings {
		if r.Timestamp.After(cutoff) {
			windowReadings = append(windowReadings, r)
		}
	}

	// If no readings in window, use the most recent reading (last known value)
	if len(windowReadings) == 0 {
		lastReading := readings[len(readings)-1]
		return lastReading.Value, lastReading.Value, lastReading.Value
	}

	// Initialize min and max
	min = windowReadings[0].Value
	max = windowReadings[0].Value

	// Calculate time-weighted average and min/max
	var weightedSum float64
	var totalDuration float64

	for i := 0; i < len(windowReadings); i++ {
		value := windowReadings[i].Value

		// Update min/max
		if value < min {
			min = value
		}
		if value > max {
			max = value
		}

		// Calculate duration this reading was active
		var duration float64
		if i < len(windowReadings)-1 {
			// Duration until next reading
			duration = windowReadings[i+1].Timestamp.Sub(windowReadings[i].Timestamp).Seconds()
		} else {
			// Last reading: duration from reading until now
			duration = now.Sub(windowReadings[i].Timestamp).Seconds()
		}

		weightedSum += value * duration
		totalDuration += duration
	}

	if totalDuration > 0 {
		avg = weightedSum / totalDuration
	}

	return avg, min, max
}

// calculateStats computes time-weighted statistics for different time windows
func calculateStats(topic string, readings []Reading, current, previous float64) Statistics {
	stats := Statistics{
		Topic:    topic,
		Current:  current,
		Previous: previous,
	}

	if len(readings) == 0 {
		return stats
	}

	// Calculate time-weighted statistics for each window
	avg1, min1, max1 := calculateTimeWeightedStats(readings, 1*time.Minute)
	avg5, min5, max5 := calculateTimeWeightedStats(readings, 5*time.Minute)
	avg15, min15, max15 := calculateTimeWeightedStats(readings, 15*time.Minute)

	stats.Average = TimeWindows{_1: avg1, _5: avg5, _15: avg15}
	stats.Min = TimeWindows{_1: min1, _5: min5, _15: min15}
	stats.Max = TimeWindows{_1: max1, _5: max5, _15: max15}

	return stats
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

// TopicData holds readings and current/previous values for a topic
type TopicData struct {
	readings []Reading
	current  float64
	previous float64
}

// statsWorker receives messages, maintains statistics, and displays them
func statsWorker(ctx context.Context, msgChan <-chan SensorMessage) {
	// Map of topic -> data
	topicData := make(map[string]*TopicData)

	// Cleanup ticker to remove old readings beyond 15 minutes
	cleanupTicker := time.NewTicker(30 * time.Second)
	defer cleanupTicker.Stop()

	for {
		select {
		case msg := <-msgChan:
			// Parse the value
			value, err := strconv.ParseFloat(msg.Value, 64)
			if err != nil {
				log.Printf("Error parsing value '%s' for topic '%s': %v\n", msg.Value, msg.Topic, err)
				continue
			}

			// Get or create topic data
			data, exists := topicData[msg.Topic]
			if !exists {
				data = &TopicData{}
				topicData[msg.Topic] = data
			}

			// Update current and previous
			data.previous = data.current
			data.current = value

			// Add new reading
			reading := Reading{
				Value:     value,
				Timestamp: time.Now(),
			}
			data.readings = append(data.readings, reading)

			// Calculate statistics for all topics
			var allStats []Statistics
			for topic, td := range topicData {
				stats := calculateStats(topic, td.readings, td.current, td.previous)
				allStats = append(allStats, stats)
			}

			// Display statistics
			displayAllStats(allStats)

		case <-cleanupTicker.C:
			// Remove readings older than 15 minutes for all topics
			// Always keep at least one reading (the most recent) for last known value
			cutoff := time.Now().Add(-15 * time.Minute)
			for _, data := range topicData {
				if len(data.readings) == 0 {
					continue
				}

				newReadings := make([]Reading, 0, len(data.readings))
				for _, r := range data.readings {
					if r.Timestamp.After(cutoff) {
						newReadings = append(newReadings, r)
					}
				}

				// If all readings were removed, keep the most recent one
				if len(newReadings) == 0 {
					newReadings = append(newReadings, data.readings[len(data.readings)-1])
				}

				data.readings = newReadings
			}

		case <-ctx.Done():
			return
		}
	}
}

// displayAllStats formats and prints statistics for all topics to stdout
func displayAllStats(allStats []Statistics) {
	// Sort by topic name for consistent ordering
	sort.Slice(allStats, func(i, j int) bool {
		return allStats[i].Topic < allStats[j].Topic
	})

	fmt.Print("\033[H\033[2J") // Clear screen
	fmt.Println("================================================================================")
	fmt.Println("Power Monitoring Statistics")
	fmt.Println("================================================================================")
	fmt.Println()

	for _, stats := range allStats {
		fmt.Printf("Topic: %s\n", stats.Topic)
		fmt.Printf("  Current:  %-10.2f Previous: %-10.2f\n", stats.Current, stats.Previous)
		fmt.Printf("            %-12s %-12s %-12s\n", "Average", "Min", "Max")
		fmt.Printf("  1  min:   %-12.2f %-12.2f %-12.2f\n", stats.Average._1, stats.Min._1, stats.Max._1)
		fmt.Printf("  5  min:   %-12.2f %-12.2f %-12.2f\n", stats.Average._5, stats.Min._5, stats.Max._5)
		fmt.Printf("  15 min:   %-12.2f %-12.2f %-12.2f\n", stats.Average._15, stats.Min._15, stats.Max._15)
		fmt.Println()
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
	}

	// Create message channel for communication between workers
	msgChan := make(chan SensorMessage, 10)

	// Launch stats worker
	SafeGo(ctx, cancel, "stats-worker", func(ctx context.Context) {
		statsWorker(ctx, msgChan)
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
