package main

import (
	"context"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Topics that report in kW or kWh and need conversion to W or Wh (multiply by 1000)
// This centralizes unit conversion so downstream workers always see W and Wh
var kiloToBaseUnitTopics = map[string]bool{
	// Power sensors (kW → W)
	"homeassistant/sensor/home_sweet_home_battery_power_2/state": true,
	"homeassistant/sensor/home_sweet_home_site_power/state":      true,
	"homeassistant/sensor/home_sweet_home_load_power_2/state":    true,
	// Energy sensors (kWh → Wh)
	"homeassistant/sensor/home_sweet_home_tg118095000r1a_battery_remaining/state": true,
	"homeassistant/sensor/solcast_pv_forecast_forecast_today/state":               true,
}

// Reading represents a timestamped sensor reading
type Reading struct {
	Value     float64
	Timestamp time.Time
}

// Readings is a collection of timestamped readings
type Readings []Reading

// TimeWindows holds values across 1, 5, and 15 minute windows
type TimeWindows struct {
	_1  float64
	_5  float64
	_15 float64
}

// FloatTopicData holds current value and statistics for a float topic
type FloatTopicData struct {
	Current float64
	P1      TimeWindows // 1st percentile (filters out low outliers)
	P50     TimeWindows // 50th percentile (median)
	P66     TimeWindows // 66th percentile
	P99     TimeWindows // 99th percentile (filters out high outliers)
}

// StringTopicData holds current value for a string topic
type StringTopicData struct {
	Current string
}

// BooleanTopicData holds current value for a boolean topic (on/off switches)
type BooleanTopicData struct {
	Current bool
}

// weightedValue represents a value with its duration weight for percentile calculation
type weightedValue struct {
	value    float64
	duration float64
}

// calculateTimeWeightedPercentiles returns P1, P50, P66, and P99 in a single pass
// where each value is weighted by how long it persisted.
// The pairs slice must be sorted by value in ascending order.
func calculateTimeWeightedPercentiles(pairs []weightedValue, totalDuration float64) (p1, p50, p66, p99 float64) {
	if len(pairs) == 0 {
		return 0, 0, 0, 0
	}
	if len(pairs) == 1 {
		v := pairs[0].value
		return v, v, v, v
	}

	// Target durations for each percentile
	target1 := totalDuration * 0.01
	target50 := totalDuration * 0.50
	target66 := totalDuration * 0.66
	target99 := totalDuration * 0.99

	// Walk through sorted pairs once, capturing values as we cross thresholds
	var cumulative float64
	var found1, found50, found66, found99 bool

	for _, pair := range pairs {
		cumulative += pair.duration

		if !found1 && cumulative >= target1 {
			p1 = pair.value
			found1 = true
		}
		if !found50 && cumulative >= target50 {
			p50 = pair.value
			found50 = true
		}
		if !found66 && cumulative >= target66 {
			p66 = pair.value
			found66 = true
		}
		if !found99 && cumulative >= target99 {
			p99 = pair.value
			found99 = true
			break // All found, no need to continue
		}
	}

	// Fallback to last value for any not found (shouldn't happen)
	lastValue := pairs[len(pairs)-1].value
	if !found1 {
		p1 = lastValue
	}
	if !found50 {
		p50 = lastValue
	}
	if !found66 {
		p66 = lastValue
	}
	if !found99 {
		p99 = lastValue
	}

	return p1, p50, p66, p99
}

// calculateTimeWeightedStats computes time-weighted statistics for a time window
// Each reading is weighted by the duration it was active (time until next reading)
// Returns: p1 (1st percentile), p50 (median), p66, p99 (99th percentile)
func calculateTimeWeightedStats(readings Readings, windowDuration time.Duration, now time.Time) (p1, p50, p66, p99 float64) {
	if len(readings) == 0 {
		return 0, 0, 0, 0
	}

	// Capture last reading for fallback (guaranteed non-empty from check above)
	lastReading := readings[len(readings)-1]

	cutoff := now.Add(-windowDuration)

	// Filter readings within the window
	var windowReadings Readings
	for _, r := range readings {
		if r.Timestamp.After(cutoff) {
			windowReadings = append(windowReadings, r)
		}
	}

	// If 0 or 1 readings in window, use the most recent reading (last known value)
	// Single reading has zero duration so can't compute time-weighted percentiles
	if len(windowReadings) <= 1 {
		v := lastReading.Value
		return v, v, v, v
	}

	// Build weighted value pairs
	pairs := make([]weightedValue, 0, len(windowReadings))
	var totalDuration float64

	for i := 0; i < len(windowReadings); i++ {
		value := windowReadings[i].Value

		// Calculate duration this reading was active
		var duration float64
		if i < len(windowReadings)-1 {
			// Duration until next reading
			duration = windowReadings[i+1].Timestamp.Sub(windowReadings[i].Timestamp).Seconds()
		} else {
			// Last reading: duration from reading until now
			duration = now.Sub(windowReadings[i].Timestamp).Seconds()
		}

		pairs = append(pairs, weightedValue{value: value, duration: duration})
		totalDuration += duration
	}

	// Sort pairs by value for percentile calculation
	sort.Slice(pairs, func(i, j int) bool {
		return pairs[i].value < pairs[j].value
	})

	// Calculate time-weighted percentiles in a single pass
	p1, p50, p66, p99 = calculateTimeWeightedPercentiles(pairs, totalDuration)

	return p1, p50, p66, p99
}

// calculateStats computes time-weighted statistics for different time windows
func calculateStats(data *FloatTopicData, readings Readings) {
	if len(readings) == 0 {
		return
	}

	// Calculate time-weighted statistics for each window
	now := time.Now()
	p1_1, p50_1, p66_1, p99_1 := calculateTimeWeightedStats(readings, 1*time.Minute, now)
	p1_5, p50_5, p66_5, p99_5 := calculateTimeWeightedStats(readings, 5*time.Minute, now)
	p1_15, p50_15, p66_15, p99_15 := calculateTimeWeightedStats(readings, 15*time.Minute, now)

	data.P1 = TimeWindows{_1: p1_1, _5: p1_5, _15: p1_15}
	data.P50 = TimeWindows{_1: p50_1, _5: p50_5, _15: p50_15}
	data.P66 = TimeWindows{_1: p66_1, _5: p66_5, _15: p66_15}
	data.P99 = TimeWindows{_1: p99_1, _5: p99_5, _15: p99_15}
}

// cloneTopicData creates a deep copy of topicData for safe concurrent access
func cloneTopicData(topicData map[string]any) map[string]any {
	clone := make(map[string]any, len(topicData))
	for topic, data := range topicData {
		switch d := data.(type) {
		case *FloatTopicData:
			clone[topic] = &FloatTopicData{
				Current: d.Current,
				P1:      d.P1,
				P50:     d.P50,
				P66:     d.P66,
				P99:     d.P99,
			}
		case *StringTopicData:
			clone[topic] = &StringTopicData{
				Current: d.Current,
			}
		case *BooleanTopicData:
			clone[topic] = &BooleanTopicData{
				Current: d.Current,
			}
		}
	}
	return clone
}

// Topics that should be initialized to 0.0 if not received within timeout
// These are self-published topics that won't exist on first startup
var selfPublishedFloatTopics = []string{
	"homeassistant/sensor/battery_2_available_energy/state",
	"homeassistant/sensor/battery_2_state_of_charge/state",
	"homeassistant/sensor/battery_3_available_energy/state",
	"homeassistant/sensor/battery_3_state_of_charge/state",
}

// Boolean topics that should be initialized to true if not received within timeout
var selfPublishedBoolTopics = []string{
	"homeassistant/switch/powerctl_enabled/state",
}

// statsWorker receives messages, maintains statistics, and sends to output channel
func statsWorker(ctx context.Context, msgChan <-chan SensorMessage, outputChan chan<- DisplayData, expectedTopics []string) {
	// Map of topic -> data (can be *FloatTopicData or *StringTopicData)
	topicData := make(map[string]any)
	// Map of topic -> readings (for float topics only, internal to stats worker)
	topicReadings := make(map[string]Readings)

	// Ready state tracking
	allTopicsReceived := false
	startupCheckTicker := time.NewTicker(30 * time.Second)
	defer startupCheckTicker.Stop()

	// Timer to initialize self-published topics if not received
	selfPublishedTimer := time.NewTimer(20 * time.Second)
	defer selfPublishedTimer.Stop()

	// Debouncing state
	var lastSendTime time.Time
	var debounceTimer *time.Timer
	var debounceTimerC <-chan time.Time

	// Cleanup ticker to remove old readings beyond 15 minutes
	cleanupTicker := time.NewTicker(30 * time.Second)
	defer cleanupTicker.Stop()

	// Cleanup debounce timer on exit
	defer func() {
		if debounceTimer != nil {
			debounceTimer.Stop()
		}
	}()

	for {
		select {
		case msg := <-msgChan:
			// Try to parse as float first
			value, err := strconv.ParseFloat(msg.Value, 64)
			if err == nil {
				// Apply kW/kWh to W/Wh conversion if needed
				if kiloToBaseUnitTopics[msg.Topic] {
					value *= 1000
				}

				// Handle as float topic
				var data *FloatTopicData
				if existing, exists := topicData[msg.Topic]; exists {
					// Check if existing data is actually FloatTopicData
					if floatData, ok := existing.(*FloatTopicData); ok {
						data = floatData
					} else {
						// Type mismatch: topic was previously string, now numeric
						log.Printf("ERROR: Topic %s type changed from string to float (value=%s)\n", msg.Topic, msg.Value)
						return
					}
				} else {
					data = &FloatTopicData{}
					topicData[msg.Topic] = data
				}

				// Update current value
				data.Current = value

				// Add new reading to internal storage
				reading := Reading{
					Value:     value,
					Timestamp: time.Now(),
				}
				topicReadings[msg.Topic] = append(topicReadings[msg.Topic], reading)

				// Calculate and store statistics
				calculateStats(data, topicReadings[msg.Topic])
			} else {
				// Check if value is a boolean (case-insensitive "on" or "off")
				lowerValue := strings.ToLower(msg.Value)
				if lowerValue == "on" || lowerValue == "off" {
					// Handle as boolean topic
					var data *BooleanTopicData
					if existing, exists := topicData[msg.Topic]; exists {
						if boolData, ok := existing.(*BooleanTopicData); ok {
							data = boolData
						} else {
							log.Printf("ERROR: Topic %s type changed to boolean (value=%s)\n", msg.Topic, msg.Value)
							return
						}
					} else {
						data = &BooleanTopicData{}
						topicData[msg.Topic] = data
					}
					data.Current = (lowerValue == "on")
				} else {
					// Handle as string topic
					var data *StringTopicData
					if existing, exists := topicData[msg.Topic]; exists {
						// Check if existing data is actually StringTopicData
						if stringData, ok := existing.(*StringTopicData); ok {
							data = stringData
						} else {
							// Type mismatch: topic was previously float, now string
							log.Printf("ERROR: Topic %s type changed from float to string (value=%s)\n", msg.Topic, msg.Value)
							return
						}
					} else {
						data = &StringTopicData{}
						topicData[msg.Topic] = data
					}

					// Update current value
					data.Current = msg.Value
				}
			}

			// Check if we've received all expected topics
			if !allTopicsReceived && len(topicData) == len(expectedTopics) {
				allTopicsReceived = true
				startupCheckTicker.Stop()
				log.Printf("Stats worker ready: received data for all %d topics\n", len(expectedTopics))
			}

			// Only send updates if we've received all topics
			if !allTopicsReceived {
				continue
			}

			// Debounce: send immediately if enough time has passed, otherwise schedule
			timeSinceLastSend := time.Since(lastSendTime)
			if timeSinceLastSend >= time.Second {
				// Send immediately
				select {
				case outputChan <- DisplayData{TopicData: cloneTopicData(topicData)}:
					lastSendTime = time.Now()
				case <-ctx.Done():
					return
				}
			} else if debounceTimer == nil {
				// Schedule a send for later
				remainingTime := time.Second - timeSinceLastSend
				debounceTimer = time.NewTimer(remainingTime)
				debounceTimerC = debounceTimer.C
			}

		case <-debounceTimerC:
			// Timer fired, send the pending update (only if ready)
			if allTopicsReceived {
				select {
				case outputChan <- DisplayData{TopicData: cloneTopicData(topicData)}:
					lastSendTime = time.Now()
				case <-ctx.Done():
					return
				}
			}
			debounceTimer = nil
			debounceTimerC = nil

		case <-startupCheckTicker.C:
			log.Printf("Startup check: received %d/%d topics\n", len(topicData), len(expectedTopics))

			// Periodically check for missing topics
			if allTopicsReceived {
				continue
			}

			receivedTopics := make(map[string]bool)
			for topic := range topicData {
				receivedTopics[topic] = true
			}

			var missingTopics []string
			for _, topic := range expectedTopics {
				if !receivedTopics[topic] {
					missingTopics = append(missingTopics, topic)
				}
			}

			if len(missingTopics) > 0 {
				log.Printf("WARNING: Still waiting for topics. Missing %d/%d:\n",
					len(missingTopics), len(expectedTopics))
				for _, topic := range missingTopics {
					log.Printf("  - %s\n", topic)
				}
			}

		case <-selfPublishedTimer.C:
			// Initialize self-published float topics to 0.0 if not yet received
			for _, topic := range selfPublishedFloatTopics {
				if _, exists := topicData[topic]; !exists {
					log.Printf("Initializing missing self-published topic to 0.0: %s\n", topic)
					topicData[topic] = &FloatTopicData{Current: 0.0}
					topicReadings[topic] = Readings{{Value: 0.0, Timestamp: time.Now()}}
				}
			}
			// Initialize self-published boolean topics to true if not yet received
			for _, topic := range selfPublishedBoolTopics {
				if _, exists := topicData[topic]; !exists {
					log.Printf("Initializing missing self-published topic to true: %s\n", topic)
					topicData[topic] = &BooleanTopicData{Current: true}
				}
			}

		case <-cleanupTicker.C:
			// Remove readings older than 15 minutes for float topics
			// Always keep at least one reading (the most recent) for last known value
			cutoff := time.Now().Add(-15 * time.Minute)
			for topic, readings := range topicReadings {
				if len(readings) == 0 {
					continue
				}

				newReadings := make(Readings, 0, len(readings))
				for _, r := range readings {
					if r.Timestamp.After(cutoff) {
						newReadings = append(newReadings, r)
					}
				}

				// If all readings were removed, keep the most recent one
				if len(newReadings) == 0 {
					newReadings = append(newReadings, readings[len(readings)-1])
				}

				topicReadings[topic] = newReadings
			}

		case <-ctx.Done():
			return
		}
	}
}
