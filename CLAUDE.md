# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

`powerctl` is a Go-based MQTT client that monitors multiple Home Assistant sensors simultaneously. It connects to a Home Assistant MQTT broker, subscribes to multiple sensor topics, and displays real-time statistics for each sensor including current values, historical trends, and time-weighted averages, minimums, and maximums over 1, 5, and 15 minute intervals. Time-weighted averaging ensures accurate statistics even when sensor readings arrive at irregular intervals.

Currently monitors:
- Powerhouse inverters 5-9 energy sensors
- Solar 3 & 4 energy sensors

## Development Environment

The project uses Nix for development environment management:

```bash
nix-shell  # Enter development environment with Go, claude-code, and claude-monitor
```

## Development Commands

### Using Make (Recommended)
```bash
make build  # Build the binary
make run    # Build and run the application
make clean  # Remove built binary
```

### Direct Go Commands
```bash
go build -o powerctl .  # Build the binary
./powerctl              # Run the application
```

### Dependencies
```bash
go mod tidy    # Clean up dependencies
go mod verify  # Verify dependencies
```

### Testing
```bash
go test ./...              # Run all tests
go test -v ./...           # Run tests with verbose output
go test ./path/to/package  # Run tests for specific package
go test -run TestName      # Run specific test
```

### Code Quality
```bash
go fmt ./...  # Format code
go vet ./...  # Run Go vet for suspicious code
```

## Architecture

The application uses a goroutine-based architecture with message passing via channels:

### Core Components

1. **SafeGo** (main.go:132-144)
   - Launches goroutines with panic recovery
   - Automatically cancels the application context if a goroutine panics
   - Logs panic information for debugging

2. **statsWorker** (main.go:153-217)
   - Goroutine that receives SensorMessage structs via a channel
   - Maintains separate state for each topic (readings, current, previous values)
   - Calculates real-time statistics using time-weighted averaging per topic
   - Current and previous values for each sensor
   - 1, 5, and 15 minute time-weighted averages, minimums, and maximums
   - Automatically cleans up readings older than 15 minutes every 30 seconds
   - Displays formatted statistics for all topics to stdout (clears screen on each update)

3. **mqttWorker** (main.go:240-300)
   - Goroutine that connects to Home Assistant MQTT broker at `homeassistant.lan:1883`
   - Subscribes to multiple sensor topics simultaneously
   - Forwards received messages with topic information to statsWorker via channel
   - Handles reconnection automatically via paho.mqtt client options

### Data Structures

- **SensorMessage**: MQTT message with topic and value
- **Reading**: Timestamped sensor value
- **TopicData**: Holds readings and current/previous values for a specific topic
- **TimeWindows**: Holds values across 1, 5, and 15 minute windows (accessed as `._1`, `._5`, `._15`)
- **Statistics**: Current, previous, and time-windowed statistics organized by metric type
  - Includes topic name for identification
  - Accessed as `stats.Average._1`, `stats.Min._5`, `stats.Max._15`, etc.

### Statistics Algorithm

The application uses time-weighted averaging to account for irregular message arrival times:
1. Each reading is assigned a weight based on how long it was "active"
2. Weight = duration from this reading until the next reading (or until now for the last reading)
3. Time-weighted average = sum(value × duration) / sum(durations)
4. This ensures that a value that was stable for 30 seconds has more influence than a brief spike
5. Min/Max remain simple - just the minimum and maximum values observed in the window
6. **Last known value preservation**: If no messages arrive in a time window, statistics show the last known value instead of zero
7. At least one reading is always kept (even if older than 15 minutes) to maintain the last known value

### Message Flow

```
MQTT Broker → mqttWorker → SensorMessage channel → statsWorker → statistics (per topic) → stdout
```

Each topic's statistics are tracked independently and displayed together.

### Concurrency Model

- Workers are launched using `SafeGo` which wraps goroutines with panic recovery
- Communication between workers uses a buffered channel of SensorMessage (capacity: 10)
- Context is used for lifecycle management and graceful shutdown
- If any worker panics, the entire application shuts down gracefully
- Per-topic state is maintained in a map, allowing dynamic addition of new topics

### Dependencies

- `github.com/eclipse/paho.mqtt.golang` - MQTT client
- `github.com/joho/godotenv` - Environment variable loading

### Configuration

MQTT credentials are loaded from a `.env` file (see `.env.example` for template):
- `MQTT_USERNAME` - MQTT broker username
- `MQTT_PASSWORD` - MQTT broker password

MQTT connection settings in main():
- Broker: `homeassistant.lan`
- Port: `1883`
- Topics monitored (defined in main.go:323-331):
  - `homeassistant/sensor/powerhouse_inverter_5_switch_0_energy/state`
  - `homeassistant/sensor/powerhouse_inverter_6_switch_0_energy/state`
  - `homeassistant/sensor/powerhouse_inverter_7_switch_0_energy/state`
  - `homeassistant/sensor/powerhouse_inverter_8_switch_0_energy/state`
  - `homeassistant/sensor/powerhouse_inverter_9_switch_0_energy/state`
  - `homeassistant/sensor/solar_3_energy/state`
  - `homeassistant/sensor/solar_4_energy/state`

**Setup**: Copy `.env.example` to `.env` and fill in your credentials.

**Adding topics**: Edit the `topics` slice in main.go to add or remove monitored sensors.
