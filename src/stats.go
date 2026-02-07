package main

import (
	"context"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Window constants for GetPercentile
const (
	Window5Min  = 5 * time.Minute
	Window15Min = 15 * time.Minute
)

// Percentile constants for GetPercentile
const (
	P1   = 1
	P50  = 50
	P66  = 66
	P90  = 90
	P99  = 99
	P100 = 100
)

// Topics that report in kW or kWh and need conversion to W or Wh (multiply by 1000)
// This centralizes unit conversion so downstream workers always see W and Wh
var kiloToBaseUnitTopics = map[string]bool{
	// Power sensors (kW → W)
	"homeassistant/sensor/home_sweet_home_battery_power_2/state": true,
	"homeassistant/sensor/home_sweet_home_site_power/state":      true,
	"homeassistant/sensor/home_sweet_home_load_power_2/state":    true,
	// Energy sensors (kWh → Wh)
	"homeassistant/sensor/home_sweet_home_tg118095000r1a_battery_remaining/state":  true,
	"homeassistant/sensor/solcast_pv_forecast_forecast_today/state":                true,
	"homeassistant/sensor/solcast_pv_forecast_forecast_remaining_today/state":      true,
}

// PercentileSpec defines a specific percentile and time window combination
type PercentileSpec struct {
	Percentile int           // 1, 50, 66, or 99
	Window     time.Duration // 1, 5, or 15 minutes
}

// requiredPercentiles maps topics to the specific percentile/window combinations they need.
// Topics not in this map will only have their Current value tracked (no percentile calculations).
// This dramatically reduces computation by only calculating what's actually used.
var requiredPercentiles = map[string][]PercentileSpec{
	// Voltage topics - used by lowVoltageWorker for P1._15
	"homeassistant/sensor/solar_5_battery_voltage/state": {{1, 15 * time.Minute}},
	"homeassistant/sensor/solar_3_battery_voltage/state": {{1, 15 * time.Minute}},

	// Load power - used by unifiedInverterEnabler Powerwall Last mode for P66._15, P99._15
	"homeassistant/sensor/home_sweet_home_load_power_2/state": {
		{66, 15 * time.Minute},
		{99, 15 * time.Minute},
	},

	// Solar 1 power - used by power_excess_calculator for P50._5, unifiedInverterEnabler for P90._15
	TopicSolar1Power: {
		{50, 5 * time.Minute},
		{90, 15 * time.Minute},
	},

	// Tesla battery remaining - used by powerExcessCalculator for P50._5
	"homeassistant/sensor/home_sweet_home_tg118095000r1a_battery_remaining/state": {{50, 5 * time.Minute}},

	// Battery available energy - used by powerExcessCalculator for P50._5
	TopicBattery2Energy: {{50, 5 * time.Minute}},
	TopicBattery3Energy: {{50, 5 * time.Minute}},

	// AC frequency - used by unifiedInverterEnabler for high frequency protection (P100._5)
	"homeassistant/sensor/lounge_ac_frequency/state": {{100, 5 * time.Minute}},
}

// Reading represents a timestamped sensor reading
type Reading struct {
	Value     float64
	Timestamp time.Time
}

// Readings is a collection of timestamped readings
type Readings []Reading

// FloatTopicData holds the current value for a float topic
type FloatTopicData struct {
	Current float64
}

// PercentileKey identifies a specific percentile calculation
type PercentileKey struct {
	Topic      string
	Percentile int
	Window     time.Duration
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

// calculateSelectedPercentile calculates a single time-weighted percentile for a window.
// The percentile parameter should be 1, 50, 66, or 99.
func calculateSelectedPercentile(
	pairs []weightedValue,
	totalDuration float64,
	percentile int,
	fallbackValue float64,
) float64 {
	if len(pairs) == 0 {
		return fallbackValue
	}
	if len(pairs) == 1 {
		return pairs[0].value
	}

	target := totalDuration * float64(percentile) / 100.0
	var cumulative float64

	for _, pair := range pairs {
		cumulative += pair.duration
		if cumulative >= target {
			return pair.value
		}
	}

	return pairs[len(pairs)-1].value
}

// prepareWindowData filters readings for a time window and prepares sorted weighted pairs.
// Returns the sorted pairs, total duration, and fallback value for empty windows.
func prepareWindowData(
	readings Readings,
	windowDuration time.Duration,
	now time.Time,
) (pairs []weightedValue, totalDuration float64, fallbackValue float64) {
	if len(readings) == 0 {
		return nil, 0, 0
	}

	// Capture last reading for fallback
	lastReading := readings[len(readings)-1]
	fallbackValue = lastReading.Value

	cutoff := now.Add(-windowDuration)

	// Filter readings within the window
	var windowReadings Readings
	for _, r := range readings {
		if r.Timestamp.After(cutoff) {
			windowReadings = append(windowReadings, r)
		}
	}

	// If 0 or 1 readings in window, use fallback
	if len(windowReadings) <= 1 {
		return nil, 0, fallbackValue
	}

	// Build weighted value pairs
	pairs = make([]weightedValue, 0, len(windowReadings))

	for i := 0; i < len(windowReadings); i++ {
		value := windowReadings[i].Value

		var duration float64
		if i < len(windowReadings)-1 {
			duration = windowReadings[i+1].Timestamp.Sub(windowReadings[i].Timestamp).Seconds()
		} else {
			duration = now.Sub(windowReadings[i].Timestamp).Seconds()
		}

		pairs = append(pairs, weightedValue{value: value, duration: duration})
		totalDuration += duration
	}

	// Sort pairs by value for percentile calculation
	sort.Slice(pairs, func(i, j int) bool {
		return pairs[i].value < pairs[j].value
	})

	return pairs, totalDuration, fallbackValue
}

// calculateRequiredStats calculates only the percentiles specified in the registry for a topic.
// Results are written to the percentiles map.
func calculateRequiredStats(topic string, readings Readings, percentiles map[PercentileKey]float64) {
	specs, needsPercentiles := requiredPercentiles[topic]
	if !needsPercentiles || len(readings) == 0 {
		return
	}

	now := time.Now()

	// Group specs by window to avoid redundant filtering/sorting
	type windowWork struct {
		pairs         []weightedValue
		totalDuration float64
		fallback      float64
	}
	windowCache := make(map[time.Duration]*windowWork)

	for _, spec := range specs {
		// Prepare window data (cached per window duration)
		work, exists := windowCache[spec.Window]
		if !exists {
			pairs, totalDuration, fallback := prepareWindowData(readings, spec.Window, now)
			work = &windowWork{
				pairs:         pairs,
				totalDuration: totalDuration,
				fallback:      fallback,
			}
			windowCache[spec.Window] = work
		}

		// Calculate the specific percentile
		var value float64
		if work.pairs == nil {
			value = work.fallback
		} else {
			value = calculateSelectedPercentile(work.pairs, work.totalDuration, spec.Percentile, work.fallback)
		}

		// Store in the percentiles map
		percentiles[PercentileKey{topic, spec.Percentile, spec.Window}] = value
	}
}

// cloneTopicData creates a deep copy of topicData for safe concurrent access
func cloneTopicData(topicData map[string]any) map[string]any {
	clone := make(map[string]any, len(topicData))
	for topic, data := range topicData {
		switch d := data.(type) {
		case *FloatTopicData:
			clone[topic] = &FloatTopicData{Current: d.Current}
		case *StringTopicData:
			clone[topic] = &StringTopicData{Current: d.Current}
		case *BooleanTopicData:
			clone[topic] = &BooleanTopicData{Current: d.Current}
		}
	}
	return clone
}

// clonePercentiles creates a copy of the percentiles map for safe concurrent access
func clonePercentiles(percentiles map[PercentileKey]float64) map[PercentileKey]float64 {
	clone := make(map[PercentileKey]float64, len(percentiles))
	for k, v := range percentiles {
		clone[k] = v
	}
	return clone
}

// Topics that should be initialized to 0.0 if not received within timeout
// These are self-published topics that won't exist on first startup
var selfPublishedFloatTopics = []string{
	TopicBattery2Energy,
	"homeassistant/sensor/battery_2_state_of_charge/state",
	TopicBattery3Energy,
	"homeassistant/sensor/battery_3_state_of_charge/state",
}

// Boolean topics that should be initialized to true if not received within timeout
var selfPublishedBoolTopics = []string{
	TopicPowerctlEnabledState,
	TopicPowerhouseInvertersEnabledState,
	TopicPW2DischargeState,
	TopicExpectingPowerCutsState,
}

// statsWorker receives messages, maintains statistics, and sends to output channel
func statsWorker(ctx context.Context, msgChan <-chan SensorMessage, outputChan chan<- DisplayData, expectedTopics []string) {
	// Map of topic -> data (can be *FloatTopicData or *StringTopicData)
	topicData := make(map[string]any)
	// Map of topic -> readings (for float topics only, internal to stats worker)
	topicReadings := make(map[string]Readings)
	// Percentiles for registered topics
	percentiles := make(map[PercentileKey]float64)

	// Ready state tracking
	allTopicsReceived := false
	startupCheckTicker := time.NewTicker(30 * time.Second)
	defer startupCheckTicker.Stop()

	// Timer to initialize self-published topics if not received
	selfPublishedTimer := time.NewTimer(20 * time.Second)
	defer selfPublishedTimer.Stop()

	// Cleanup ticker to remove old readings beyond 15 minutes
	cleanupTicker := time.NewTicker(30 * time.Second)
	defer cleanupTicker.Stop()

	// Percentile refresh ticker for live updates and downstream broadcast
	percentileTicker := time.NewTicker(1 * time.Second)
	defer percentileTicker.Stop()

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

				// Add new reading to internal storage (percentiles calculated on ticker)
				reading := Reading{
					Value:     value,
					Timestamp: time.Now(),
				}
				topicReadings[msg.Topic] = append(topicReadings[msg.Topic], reading)
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

		case <-percentileTicker.C:
			// Recalculate percentiles for registered topics (live updates as time passes)
			if !allTopicsReceived {
				continue
			}

			for topic := range requiredPercentiles {
				calculateRequiredStats(topic, topicReadings[topic], percentiles)
			}

			// Send updated data (non-blocking to avoid stalling if downstream is slow)
			select {
			case outputChan <- DisplayData{
				TopicData:   cloneTopicData(topicData),
				Percentiles: clonePercentiles(percentiles),
			}:
			default:
				// Channel full, skip this update
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
