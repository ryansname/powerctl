package main

import (
	"context"
	"strconv"
	"time"
)

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
	Average TimeWindows
	Min     TimeWindows
	Max     TimeWindows
}

// StringTopicData holds current value for a string topic
type StringTopicData struct {
	Current string
}

// calculateTimeWeightedStats computes time-weighted statistics for a time window
// Each reading is weighted by the duration it was active (time until next reading)
func calculateTimeWeightedStats(readings Readings, windowDuration time.Duration) (avg, min, max float64) {
	if len(readings) == 0 {
		return 0, 0, 0
	}

	now := time.Now()
	cutoff := now.Add(-windowDuration)

	// Filter readings within the window
	var windowReadings Readings
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
func calculateStats(data *FloatTopicData, readings Readings) {
	if len(readings) == 0 {
		return
	}

	// Calculate time-weighted statistics for each window
	avg1, min1, max1 := calculateTimeWeightedStats(readings, 1*time.Minute)
	avg5, min5, max5 := calculateTimeWeightedStats(readings, 5*time.Minute)
	avg15, min15, max15 := calculateTimeWeightedStats(readings, 15*time.Minute)

	data.Average = TimeWindows{_1: avg1, _5: avg5, _15: avg15}
	data.Min = TimeWindows{_1: min1, _5: min5, _15: min15}
	data.Max = TimeWindows{_1: max1, _5: max5, _15: max15}
}

// cloneTopicData creates a deep copy of topicData for safe concurrent access
func cloneTopicData(topicData map[string]any) map[string]any {
	clone := make(map[string]any, len(topicData))
	for topic, data := range topicData {
		switch d := data.(type) {
		case *FloatTopicData:
			clone[topic] = &FloatTopicData{
				Current: d.Current,
				Average: d.Average,
				Min:     d.Min,
				Max:     d.Max,
			}
		case *StringTopicData:
			clone[topic] = &StringTopicData{
				Current: d.Current,
			}
		}
	}
	return clone
}

// statsWorker receives messages, maintains statistics, and sends them for display
func statsWorker(ctx context.Context, msgChan <-chan SensorMessage, displayChan chan<- DisplayData) {
	// Map of topic -> data (can be *FloatTopicData or *StringTopicData)
	topicData := make(map[string]any)
	// Map of topic -> readings (for float topics only, internal to stats worker)
	topicReadings := make(map[string]Readings)

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
				// Handle as float topic
				var data *FloatTopicData
				if existing, exists := topicData[msg.Topic]; exists {
					data = existing.(*FloatTopicData)
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
				// Handle as string topic
				var data *StringTopicData
				if existing, exists := topicData[msg.Topic]; exists {
					data = existing.(*StringTopicData)
				} else {
					data = &StringTopicData{}
					topicData[msg.Topic] = data
				}

				// Update current value
				data.Current = msg.Value
			}

			// Debounce: send immediately if enough time has passed, otherwise schedule
			timeSinceLastSend := time.Since(lastSendTime)
			if timeSinceLastSend >= time.Second {
				// Send immediately
				select {
				case displayChan <- DisplayData{TopicData: cloneTopicData(topicData)}:
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
			// Timer fired, send the pending update
			select {
			case displayChan <- DisplayData{TopicData: cloneTopicData(topicData)}:
				lastSendTime = time.Now()
			case <-ctx.Done():
				return
			}
			debounceTimer = nil
			debounceTimerC = nil

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
