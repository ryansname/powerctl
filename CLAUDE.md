# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

`powerctl` is a Go-based MQTT client that monitors Home Assistant sensors and tracks battery state of charge. It connects to a Home Assistant MQTT broker, subscribes to sensor topics, calculates real-time statistics, and monitors battery charge levels with automatic Home Assistant integration.

**Key Features:**
- Real-time statistics with time-weighted averaging over 1, 5, and 15 minute intervals
- Battery monitoring with loss accounting (conversion losses + BMS/controller overhead)
- Automatic Home Assistant MQTT discovery integration
- Bidirectional MQTT communication (subscribe to sensors, publish battery states)

**Currently monitors:**
- Powerhouse inverters 1-9 energy sensors
- Solar 3, 4, and 5 energy sensors and charge states
- Battery voltages for calibration

**Battery Monitoring:**
- Battery 2: 10 kWh (SunnyTech Solar) - Solar 5 inflow, Inverters 1-4 outflow
- Battery 3: 15 kWh (Micromall) - Solar 3 & 4 inflow, Inverters 5-9 outflow

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

2. **statsWorker** (stats.go:140-290)
   - Receives SensorMessage structs via a channel
   - Maintains separate state for each topic (readings, current, previous values)
   - Calculates real-time statistics using time-weighted averaging per topic
   - 1, 5, and 15 minute time-weighted averages, minimums, and maximums
   - Automatically cleans up readings older than 15 minutes every 30 seconds
   - Updates are debounced (max 1 update per second)
   - **Startup readiness**: Waits for all expected topics before sending data
   - Logs missing topics every 30 seconds until all received
   - Sends DisplayData to broadcastWorker

3. **broadcastWorker** (broadcast_worker.go)
   - Implements the actor pattern for fan-out
   - Receives DisplayData from statsWorker
   - Broadcasts to multiple downstream workers using non-blocking sends
   - Isolates fan-out logic, making it easy to add new downstream workers
   - Logs warnings when worker channels are full (but continues processing)

4. **displayWorker** (main.go:42-106)
   - Downstream worker that receives DisplayData via a channel
   - Formats and prints statistics for all topics to stdout
   - Currently commented out - battery monitoring provides primary output

5. **batteryMonitorWorker** (battery_monitor.go:50-230)
   - Monitors battery state of charge using energy flow tracking
   - Tracks available energy in Wh as primary state (percentage is derived)
   - **Energy accounting**:
     - Adds inflow energy (solar charging)
     - Subtracts outflow energy with 2% conversion loss
     - Subtracts 50W constant BMS/controller overhead
   - **Calibration points**:
     - 100% when Float Charging + voltage > 53.5V
     - 0% when voltage < 51V
   - **Initialization**: Estimates % from voltage and charge state
   - **Home Assistant integration**: Creates and publishes to MQTT discovery
   - Publishes state updates (percentage + available_wh) to Home Assistant

6. **mqttSenderWorker** (mqtt_sender.go)
   - Dedicated worker for outgoing MQTT messages
   - Receives MQTTMessage structs via channel (100-message buffer)
   - Handles message queuing automatically
   - Publishes to MQTT broker with configurable QoS and retain
   - Logs publish failures
   - Launched automatically when MQTT connection is established

7. **mqttWorker** (main.go:109-168)
   - Connects to Home Assistant MQTT broker at `homeassistant.lan:1883`
   - Subscribes to multiple sensor topics simultaneously
   - Forwards received messages to statsWorker via channel
   - Launches mqttSenderWorker when connected
   - Handles reconnection automatically via paho.mqtt client options

### Data Structures

**MQTT Communication:**
- **SensorMessage**: Incoming MQTT message with topic and value
- **MQTTMessage**: Outgoing MQTT message with topic, payload, QoS, and retain flag

**Statistics:**
- **Reading**: Timestamped sensor value
- **FloatTopicData**: Holds current value and statistics for a numeric sensor topic
- **StringTopicData**: Holds current value for a string sensor topic
- **DisplayData**: Container for topic data broadcast to downstream workers
- **TimeWindows**: Holds values across 1, 5, and 15 minute windows (accessed as `._1`, `._5`, `._15`)

**Battery Monitoring:**
- **BatteryConfig**: Configuration for a battery (name, capacity, topics, manufacturer)
- **BatteryState**: Runtime state tracking available Wh, previous totals, and initialization status

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

**Incoming (from MQTT):**
```
MQTT Broker → mqttWorker → SensorMessage → statsWorker → DisplayData → broadcastWorker → (fan-out)
                             channel                       channel                         ├─→ displayWorker → stdout
                                                                                           ├─→ batteryMonitor (Battery 2)
                                                                                           └─→ batteryMonitor (Battery 3)
```

**Outgoing (to MQTT/Home Assistant):**
```
batteryMonitor → MQTTMessage → mqttOutgoingChan → mqttSenderWorker → MQTT Broker → Home Assistant
(entity creation)   channel      (100 msg buffer)
(state updates)
```

**Flow Details:**
- **statsWorker** waits for all topics, then calculates statistics and broadcasts DisplayData
- **broadcastWorker** fans out to all downstream workers using non-blocking sends
- **batteryMonitors** receive all sensor data, calculate battery state, publish to Home Assistant
- **mqttSenderWorker** handles all outgoing MQTT with automatic queuing
- Each topic's statistics are tracked independently and broadcast together

### Concurrency Model

- Workers are launched using `SafeGo` which wraps goroutines with panic recovery
- Communication between workers uses buffered channels:
  - 10-message buffers for sensor data and display data
  - 100-message buffer for outgoing MQTT messages (mqttOutgoingChan)
- Context is used for lifecycle management and graceful shutdown
- If any worker panics, the entire application shuts down gracefully
- Per-topic state is maintained in a map, allowing dynamic addition of new topics

**Actor Patterns:**
- **Fan-out (broadcastWorker)**: Distributes DisplayData to multiple downstream workers
  - statsWorker only knows about one output channel (to broadcastWorker)
  - broadcastWorker knows about all downstream workers
  - Non-blocking sends prevent slow workers from blocking the pipeline
  - Each worker processes updates independently

- **MQTT Sender (mqttSenderWorker)**: Centralizes outgoing MQTT communication
  - All workers send MQTTMessage structs to outgoing channel
  - Automatic queuing with 100-message buffer
  - Decouples workers from MQTT client management
  - Single point for publish error handling

### Adding Downstream Workers

To add a new downstream worker that processes sensor statistics:

1. Create a worker function that receives `<-chan DisplayData` (see `displayWorker` in main.go:42-52 as an example)
2. In `main.go`, create a channel: `newChan := make(chan DisplayData, 10)`
3. Launch the worker: `SafeGo(ctx, cancel, "worker-name", func(ctx context.Context) { yourWorker(ctx, newChan) })`
4. Add the channel to the `downstreamChans` slice (before launching broadcastWorker)

Example:
```go
// Create channel
controlChan := make(chan DisplayData, 10)

// Launch worker
SafeGo(ctx, cancel, "control-worker", func(ctx context.Context) {
    controlWorker(ctx, controlChan)
})

// Add to downstreamChans
downstreamChans := []chan<- DisplayData{
    displayChan,
    controlChan,  // <-- Add here
}
```

Example downstream workers could:
- Control smart switches based on power thresholds
- Send alerts when values exceed limits
- Log data to files or databases
- Expose metrics via HTTP endpoints
- Make HTTP requests to Home Assistant API to control devices

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

**Topics monitored** (defined in main.go:191-218):

Battery 2 (10 kWh) outflows:
- `homeassistant/sensor/powerhouse_inverter_1_switch_0_energy/state`
- `homeassistant/sensor/powerhouse_inverter_2_switch_0_energy/state`
- `homeassistant/sensor/powerhouse_inverter_3_switch_0_energy/state`
- `homeassistant/sensor/powerhouse_inverter_4_switch_0_energy/state`

Battery 3 (15 kWh) outflows:
- `homeassistant/sensor/powerhouse_inverter_5_switch_0_energy/state`
- `homeassistant/sensor/powerhouse_inverter_6_switch_0_energy/state`
- `homeassistant/sensor/powerhouse_inverter_7_switch_0_energy/state`
- `homeassistant/sensor/powerhouse_inverter_8_switch_0_energy/state`
- `homeassistant/sensor/powerhouse_inverter_9_switch_0_energy/state`

Battery monitoring:
- `homeassistant/sensor/solar_5_solar_energy/state` (Battery 2 inflow)
- `homeassistant/sensor/solar_5_charge_state/state`
- `homeassistant/sensor/solar_5_battery_voltage/state`
- `homeassistant/sensor/solar_3_solar_energy/state` (Battery 3 inflow)
- `homeassistant/sensor/solar_4_solar_energy/state` (Battery 3 inflow)
- `homeassistant/sensor/solar_3_charge_state/state`
- `homeassistant/sensor/solar_3_battery_voltage/state`

**MQTT Publishing:**

Battery entities are auto-created via Home Assistant MQTT discovery:
- Config topics: `homeassistant/sensor/battery_2_[percentage|available_wh]/config`
- Config topics: `homeassistant/sensor/battery_3_[percentage|available_wh]/config`
- State topics: `homeassistant/sensor/battery_2/state` (JSON with percentage + available_wh)
- State topics: `homeassistant/sensor/battery_3/state` (JSON with percentage + available_wh)

**Setup**: Copy `.env.example` to `.env` and fill in your credentials.

**Adding topics**: Edit the `topics` slice in main.go to add or remove monitored sensors.
