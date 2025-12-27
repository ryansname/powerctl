package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/chzyer/readline"
)

// WatchSpec represents a topic to watch with optional time window and percentile
type WatchSpec struct {
	Topic      string // Full topic path
	Minutes    int    // 0 = current, 1/5/15 = time window
	Percentile int    // 0 = current, 1/50/66/99 = percentile
}

// String returns a unique key for this watch spec
func (w WatchSpec) String() string {
	if w.Minutes == 0 && w.Percentile == 0 {
		return w.Topic
	}
	return fmt.Sprintf("%s -m %d -p %d", w.Topic, w.Minutes, w.Percentile)
}

// ShortName returns a short column header for this watch
func (w WatchSpec) ShortName() string {
	// Extract just the sensor name from the topic path
	// e.g., "homeassistant/sensor/solar_1_power/state" -> "solar_1_power"
	parts := strings.Split(w.Topic, "/")
	name := w.Topic
	if len(parts) >= 3 {
		name = parts[len(parts)-2] // Second to last part is usually the sensor name
	}

	if w.Minutes == 0 && w.Percentile == 0 {
		return name
	}
	return fmt.Sprintf("%s %dm p%d", name, w.Minutes, w.Percentile)
}

// GetValue extracts the value from DisplayData based on the watch spec
func (w WatchSpec) GetValue(data DisplayData) string {
	// Check if it's a string topic first
	if strVal := data.GetString(w.Topic); strVal != "" {
		return strVal
	}

	// Check if it's a boolean topic
	if boolData, ok := data.TopicData[w.Topic].(*BooleanTopicData); ok {
		if boolData.Current {
			return "on"
		}
		return "off"
	}

	// Treat as float topic
	floatData := data.GetFloat(w.Topic)
	if floatData == nil {
		return "-"
	}

	var value float64
	if w.Minutes == 0 && w.Percentile == 0 {
		value = floatData.Current
	} else {
		// Get the appropriate percentile and time window
		var tw TimeWindows
		switch w.Percentile {
		case 1:
			tw = floatData.P1
		case 50:
			tw = floatData.P50
		case 66:
			tw = floatData.P66
		case 99:
			tw = floatData.P99
		default:
			tw = floatData.P50 // Default to median
		}

		switch w.Minutes {
		case 1:
			value = tw._1
		case 5:
			value = tw._5
		case 15:
			value = tw._15
		default:
			value = tw._15 // Default to 15 minutes
		}
	}

	return formatDebugValue(value)
}

// formatDebugValue formats a float with smart precision
func formatDebugValue(v float64) string {
	if v >= 100 || v <= -100 {
		return fmt.Sprintf("%.0f", v)
	}
	return fmt.Sprintf("%.2f", v)
}

// ANSI color codes for highlighting changes
const (
	ansiReset  = "\033[0m"
	ansiYellow = "\033[33m" // Yellow for changed values
)

// readlineWriter wraps log output to work with readline
type readlineWriter struct {
	rl *readline.Instance
}

func (w *readlineWriter) Write(p []byte) (n int, err error) {
	if w.rl != nil {
		w.rl.Clean()
	}
	n, err = os.Stderr.Write(p)
	if w.rl != nil {
		w.rl.Refresh()
	}
	return n, err
}

// Global readline writer for log output
var rlWriter = &readlineWriter{}

// DebugState manages the list of watched topics
type DebugState struct {
	watches       []WatchSpec
	headerPrinted bool
	columnWidths  []int
	latestData    *DisplayData
	rl            *readline.Instance
	prevValues    map[string]string // Track previous value per watch for change highlighting
}

// NewDebugState creates a new debug state
func NewDebugState() *DebugState {
	return &DebugState{
		watches:       make([]WatchSpec, 0),
		headerPrinted: false,
		prevValues:    make(map[string]string),
	}
}

// AddWatch adds a watch and re-sorts the list
func (s *DebugState) AddWatch(spec WatchSpec) {
	// Check for duplicate
	for _, w := range s.watches {
		if w.String() == spec.String() {
			log.Printf("Already watching: %s", spec.String())
			return
		}
	}

	s.watches = append(s.watches, spec)
	sort.Slice(s.watches, func(i, j int) bool {
		return s.watches[i].ShortName() < s.watches[j].ShortName()
	})
	s.headerPrinted = false
	log.Printf("Watching: %s", spec.String())
}

// RemoveWatch removes an exact match watch
func (s *DebugState) RemoveWatch(spec WatchSpec) bool {
	for i, w := range s.watches {
		if w.String() == spec.String() {
			s.watches = slices.Delete(s.watches, i, i+1)
			s.headerPrinted = false
			log.Printf("Unwatched: %s", spec.String())
			return true
		}
	}
	return false
}

// RemoveWatchFuzzy removes a watch by topic, either exact or single match
func (s *DebugState) RemoveWatchFuzzy(topic string) bool {
	// First try exact match (current value only)
	for i, w := range s.watches {
		if w.Topic == topic && w.Minutes == 0 && w.Percentile == 0 {
			s.watches = slices.Delete(s.watches, i, i+1)
			s.headerPrinted = false
			log.Printf("Unwatched: %s", topic)
			return true
		}
	}

	// Find all matches for this topic
	var matches []int
	for i, w := range s.watches {
		if w.Topic == topic {
			matches = append(matches, i)
		}
	}

	// If exactly one match, remove it
	if len(matches) == 1 {
		removed := s.watches[matches[0]]
		s.watches = slices.Delete(s.watches, matches[0], matches[0]+1)
		s.headerPrinted = false
		log.Printf("Unwatched: %s", removed.String())
		return true
	}

	if len(matches) > 1 {
		log.Printf("Multiple watches for %s, use full spec to unwatch", topic)
		return false
	}

	log.Printf("No watch found for: %s", topic)
	return false
}

// RemoveAll removes all watches
func (s *DebugState) RemoveAll() {
	s.watches = s.watches[:0]
	s.headerPrinted = false
	log.Println("All watches removed")
}

// UpdateData stores the latest DisplayData for use by list command
func (s *DebugState) UpdateData(data DisplayData) {
	s.latestData = &data
}

// SetReadline sets the readline instance for proper output handling
func (s *DebugState) SetReadline(rl *readline.Instance) {
	s.rl = rl
}

// print outputs a line, handling readline prompt properly
func (s *DebugState) print(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	if s.rl != nil {
		// Clean prompt, print, refresh prompt
		s.rl.Clean()
		fmt.Println(line)
		s.rl.Refresh()
	} else {
		fmt.Println(line)
	}
}

// ListTopics prints all available topics
func (s *DebugState) ListTopics() {
	if s.latestData == nil {
		log.Println("No data received yet")
		return
	}

	// Collect and sort topic names
	topics := make([]string, 0, len(s.latestData.TopicData))
	for topic := range s.latestData.TopicData {
		topics = append(topics, topic)
	}
	sort.Strings(topics)

	s.print("Available topics (%d):", len(topics))
	for _, topic := range topics {
		// Show type indicator
		var typeStr string
		switch s.latestData.TopicData[topic].(type) {
		case *FloatTopicData:
			typeStr = "[float]"
		case *StringTopicData:
			typeStr = "[string]"
		case *BooleanTopicData:
			typeStr = "[bool]"
		default:
			typeStr = "[?]"
		}
		s.print("  %s %s", typeStr, topic)
	}
}

// PrintHeader prints the column headers
func (s *DebugState) PrintHeader() {
	if len(s.watches) == 0 {
		return
	}

	// Calculate column widths
	s.columnWidths = make([]int, len(s.watches))
	for i, w := range s.watches {
		s.columnWidths[i] = len(w.ShortName())
	}

	// Build header line
	parts := make([]string, 0, len(s.watches))
	for i, w := range s.watches {
		parts = append(parts, fmt.Sprintf("%*s", s.columnWidths[i], w.ShortName()))
	}
	s.print("%s", strings.Join(parts, " | "))
	s.headerPrinted = true
	s.prevValues = make(map[string]string) // Reset previous values when header changes
}

// PrintRow prints the current values for all watches (only if changed)
func (s *DebugState) PrintRow(data DisplayData) {
	if len(s.watches) == 0 {
		return
	}

	if !s.headerPrinted {
		s.PrintHeader()
	}

	// Build row and check for changes
	parts := make([]string, 0, len(s.watches))
	anyChanged := false
	newValues := make(map[string]string, len(s.watches))

	for i, w := range s.watches {
		value := w.GetValue(data)
		key := w.String()
		newValues[key] = value

		width := s.columnWidths[i]
		if len(value) > width {
			width = len(value)
			s.columnWidths[i] = width
		}

		// Check if this value changed
		prevValue, hasPrev := s.prevValues[key]
		changed := !hasPrev || prevValue != value
		if changed {
			anyChanged = true
			// Highlight changed value in yellow
			parts = append(parts, fmt.Sprintf("%s%*s%s", ansiYellow, width, value, ansiReset))
		} else {
			parts = append(parts, fmt.Sprintf("%*s", width, value))
		}
	}

	// Only print if at least one value changed
	if anyChanged {
		s.print("%s", strings.Join(parts, " | "))
		s.prevValues = newValues
	}
}

// parseWatchSpec parses watch command arguments into a WatchSpec
func parseWatchSpec(args []string) (*WatchSpec, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("usage: watch <topic> [-m <1|5|15>] [-p <1|50|66|99>]")
	}

	spec := &WatchSpec{
		Topic:      args[0],
		Minutes:    0,
		Percentile: 0,
	}

	// Parse optional -m and -p flags
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "-m":
			if i+1 >= len(args) {
				return nil, fmt.Errorf("-m requires a value (1, 5, or 15)")
			}
			i++
			m, err := strconv.Atoi(args[i])
			if err != nil || (m != 1 && m != 5 && m != 15) {
				return nil, fmt.Errorf("-m must be 1, 5, or 15")
			}
			spec.Minutes = m
		case "-p":
			if i+1 >= len(args) {
				return nil, fmt.Errorf("-p requires a value (1, 50, 66, or 99)")
			}
			i++
			p, err := strconv.Atoi(args[i])
			if err != nil || (p != 1 && p != 50 && p != 66 && p != 99) {
				return nil, fmt.Errorf("-p must be 1, 50, 66, or 99")
			}
			spec.Percentile = p
		default:
			return nil, fmt.Errorf("unknown option: %s", args[i])
		}
	}

	// If minutes specified but not percentile, default to P50
	if spec.Minutes > 0 && spec.Percentile == 0 {
		spec.Percentile = 50
	}
	// If percentile specified but not minutes, default to 15
	if spec.Percentile > 0 && spec.Minutes == 0 {
		spec.Minutes = 15
	}

	return spec, nil
}

// handleDebugCommand processes a debug command
func handleDebugCommand(cmd string, state *DebugState) {
	parts := strings.Fields(cmd)
	if len(parts) == 0 {
		return
	}

	switch parts[0] {
	case "watch":
		spec, err := parseWatchSpec(parts[1:])
		if err != nil {
			log.Printf("Error: %v", err)
			return
		}
		state.AddWatch(*spec)

	case "unwatch":
		if len(parts) < 2 {
			log.Println("Usage: unwatch <topic> [-m <minutes>] [-p <percentile>] | unwatch --all")
			return
		}
		if parts[1] == "--all" {
			state.RemoveAll()
			return
		}
		// Try to parse as full spec
		spec, err := parseWatchSpec(parts[1:])
		if err != nil {
			log.Printf("Error: %v", err)
			return
		}
		// If no -m/-p specified, use fuzzy match
		if spec.Minutes == 0 && spec.Percentile == 0 {
			state.RemoveWatchFuzzy(spec.Topic)
		} else if !state.RemoveWatch(*spec) {
			log.Printf("No watch found for: %s", spec.String())
		}

	case "list":
		state.ListTopics()

	case "help":
		fmt.Println("Commands:")
		fmt.Println("  list                             - List all available topics")
		fmt.Println("  watch <topic>                    - Watch current value")
		fmt.Println("  watch <topic> -m <1|5|15>        - Watch time window (defaults to p50)")
		fmt.Println("  watch <topic> -p <1|50|66|99>    - Watch percentile (defaults to 15m)")
		fmt.Println("  watch <topic> -m 15 -p 66        - Watch specific window and percentile")
		fmt.Println("  unwatch <topic>                  - Remove watch (exact or fuzzy match)")
		fmt.Println("  unwatch <topic> -m 15 -p 66      - Remove specific watch")
		fmt.Println("  unwatch --all                    - Remove all watches")
		fmt.Println("  help                             - Show this help")

	default:
		log.Printf("Unknown command: %s (try 'help')", parts[0])
	}
}

// readlineLoop runs the readline loop, sending commands to the channel
func readlineLoop(
	ctx context.Context,
	cancel context.CancelFunc,
	rl *readline.Instance,
	commandChan chan<- string,
) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		line, err := rl.Readline()
		if errors.Is(err, readline.ErrInterrupt) {
			cancel() // Ctrl+C pressed, shutdown the app
			return
		}
		if err != nil {
			return // EOF or other error
		}
		line = strings.TrimSpace(line)
		if line != "" {
			commandChan <- line
		}
	}
}

// getHistoryFilePath returns the path for debug history file
func getHistoryFilePath() string {
	cacheDir := os.Getenv("XDG_CACHE_HOME")
	if cacheDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "" // No history if we can't find home
		}
		cacheDir = filepath.Join(home, ".cache")
	}
	powerctlCache := filepath.Join(cacheDir, "powerctl")
	// Create directory if it doesn't exist
	_ = os.MkdirAll(powerctlCache, 0750)
	return filepath.Join(powerctlCache, "debug_history")
}

// debugWorker provides interactive introspection of DisplayData
func debugWorker(ctx context.Context, cancel context.CancelFunc, dataChan <-chan DisplayData) {
	// Create readline instance with prompt and persistent history
	rl, err := readline.NewEx(&readline.Config{
		Prompt:      "> ",
		HistoryFile: getHistoryFilePath(),
	})
	if err != nil {
		log.Printf("Debug worker: readline init failed: %v", err)
		return
	}
	defer func() {
		_ = rl.Close()
		rlWriter.rl = nil // Clear readline reference on exit
	}()

	// Redirect log output through readline-aware writer
	rlWriter.rl = rl
	log.SetOutput(rlWriter)

	log.Println("Debug worker started (type 'help' for commands)")

	commandChan := make(chan string, 10)
	state := NewDebugState()
	state.SetReadline(rl)

	go readlineLoop(ctx, cancel, rl, commandChan)

	for {
		select {
		case cmd := <-commandChan:
			handleDebugCommand(cmd, state)
		case data := <-dataChan:
			state.UpdateData(data)
			if len(state.watches) > 0 {
				state.PrintRow(data)
			}
		case <-ctx.Done():
			log.Println("Debug worker stopped")
			return
		}
	}
}
