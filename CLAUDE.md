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
- Battery 2: 9.5 kWh (SunnyTech Solar) - Solar 5 inflow, Inverters 1-4 outflow
- Battery 3: 15 kWh (Micromall) - Solar 3 & 4 inflow, Inverters 5-9 outflow

## Development Environment

The project uses Nix for development environment management:

```bash
nix-shell  # Enter development environment with Go toolchain
```

## Development Commands

### Using Make (Recommended)
```bash
make build  # Build the binary
make run    # Build and run with --force-enable --debug (for local dev)
make check  # Run golangci-lint, tests, and verify vendorHash
make clean  # Remove built binary
```

**Always run `make check` to verify changes before committing.** This runs the linter, tests, and verifies the Nix vendorHash.

### Direct Go Commands
```bash
go build -o powerctl ./src  # Build the binary
./powerctl                  # Run the application
./powerctl --force-enable   # Run with force-enable (ignores powerctl_enabled switch)
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

The application uses a goroutine-based architecture with message passing via channels. All source code is organized in the `src/` directory.

### Core Components

1. **SafeGo** (src/main.go:53-69)
   - Launches goroutines with panic recovery
   - Automatically cancels the application context if a goroutine panics
   - Logs panic information for debugging

2. **statsWorker** (src/stats.go:310-520)
   - Receives SensorMessage structs via a channel
   - Maintains separate state for each topic (readings, current values)
   - **Selective percentile calculation**: Only calculates percentiles for topics in `requiredPercentiles` registry
   - Automatically cleans up readings older than 15 minutes every 30 seconds
   - **1-second ticker**: Recalculates percentiles and broadcasts DisplayData at regular intervals
   - **Unit normalization**: Converts kW→W and kWh→Wh for Tesla/HA sensors (see `kiloToBaseUnitTopics` map)
   - **Startup readiness**: Waits for all expected topics before sending data
   - Logs missing topics every 30 seconds until all received
   - **Self-published topic initialization**: After 20 seconds, initializes missing self-published topics (battery SOC/energy to 0.0, powerctl_enabled to true) - see `selfPublishedFloatTopics` and `selfPublishedBoolTopics` lists
   - Sends DisplayData to broadcastWorker every second (not on every message)

3. **broadcastWorker** (src/broadcast_worker.go)
   - Implements the actor pattern for fan-out
   - Receives DisplayData from statsWorker
   - Broadcasts to multiple downstream workers using non-blocking sends
   - Isolates fan-out logic, making it easy to add new downstream workers
   - Logs warnings when worker channels are full (but continues processing)

4. **batteryCalibWorker** (src/battery_calib_worker.go)
   - Monitors voltage and charge state to detect calibration events
   - **100% calibration**: When in Float Charging AND voltage ≥ 53.6V AND |inflow_power + outflow_power| ≤ 250W, publishes current inflows/outflows as 100% reference
     - Outflow power is negative (power leaving battery), so we add to get net power
     - Power balance check prevents false calibration during solar spikes when battery is partially discharged
   - **Soft cap**: When NOT in Float Charging, caps SOC based on charge state:
     - Bulk Charging: caps at 99.7% (default)
     - Absorption Charging: caps at 99.8%
     - Prevents calculated SOC from exceeding cap before battery actually reaches Float Charging
     - Reduces calibration outflows by 0.005 kWh to bring SOC back down
     - 2-second cooldown between adjustments to prevent rapid-fire recalibration
   - Publishes calibration reference points (inflow/outflow totals) to MQTT attributes topic
   - MQTT retain flag ensures calibration data persists across restarts
   - Uses DisplayData helper methods (GetFloat, GetString, SumTopics)

5. **batterySOCWorker** (src/battery_soc_worker.go)
   - Calculates battery state of charge from calibration reference points
   - Reads calibration data from DisplayData (published by calibration worker via statestream)
   - **Energy accounting**:
     - Calculates energy delta since last calibration (inflows - outflows)
     - Applies 10% conversion loss rate to outflows
     - Available Wh = capacity + energy_in - energy_out_with_losses
   - Publishes percentage and available_wh to Home Assistant state topic
   - Waits for calibration topics via statsWorker readiness mechanism

6. **lowVoltageWorker** (src/low_voltage_worker.go)
   - Monitors battery voltage and turns off inverters when voltage drops too low
   - Uses 15-minute minimum voltage to avoid reacting to measurement noise
   - **Protection behavior**:
     - Threshold: 50.75V (configurable in main.go)
     - When triggered, sends turn_off commands for all attached inverters via `MQTTSender.CallService()`
     - Re-arms after 16 minutes to allow voltage recovery before checking again
   - Uses Node-RED MQTT proxy (`nodered/proxy/call_service`) to call Home Assistant services

7. **powerExcessCalculator** (src/power_excess_calculator.go)
   - Calculates excess power available for dump loads
   - **Topic constants**: Defines shared topic constants used across multiple workers:
     - `TopicBattery1Energy`, `TopicBattery2Energy`, `TopicBattery3Energy` - Battery energy topics
     - `TopicSolar1Power` - Solar 1 power topic (also used by unifiedInverterEnabler)
   - **Battery inputs** (capped at 900W total):
     - Tesla battery remaining: If 5min avg > 4kWh → Add 1000W
     - Battery 2 available energy: If 5min avg > 2.5kWh → Add 450W
     - Battery 3 available energy: If 5min avg > 3kWh → Add 450W
   - **Solar input** (added after cap):
     - Solar 1 power: If 5min avg > 1kW → Add 1000W
   - Outputs excess watts to dumpLoadEnabler via channel

8. **dumpLoadEnabler** (src/dump_load_enabler.go)
   - Controls miner workmode based on excess power
   - **Thresholds**:
     - \> 1.7kW → "Super" mode
     - \> 1.2kW → "Standard" mode
     - \> 800W → "Eco" mode
     - Otherwise → "Sleep" mode
   - Only sends command when workmode changes
   - Uses `MQTTSender.CallService()` to set miner workmode

9. **unifiedInverterEnabler** (src/unified_inverter_enabler.go)
   - Single worker managing all 9 inverters across both batteries
   - **Per-battery Forecast Excess mode** (replaces Max Inverter mode):
     - Targets 100% battery by solar end time by gradually using excess energy
     - **Formula**: `target_watts = excess_wh / hours_until_solar_end`
       - `excess_wh = (available_wh + expected_solar_wh) - battery_capacity_wh`
       - `expected_solar_wh = solar_multiplier × (forecast_remaining_wh - forecast_after_cutoff_wh)`
       - Solar multiplier: 4.0 (relative to Solcast 1kW reference)
     - **Solar forecast cutoff**: Subtracts solar generation after solar end time from remaining forecast (solar after cutoff can be fully inverted without using the battery)
     - **Solar end time**: Last period where expected generation exceeds battery's inverting capacity (e.g., B2 with 4×255W=1020W needs forecast > 0.255 kW)
     - **Ratchet-down**: Target can only decrease during the day (never increase mid-day)
     - **Daily reset**: Resets when date changes (fresh start each morning)
     - **Night cycle disable**: Returns 0 when current forecast generation is 0
   - **Global modes** (each mode handles its own solar subtraction if needed):
     - Powerwall Low Mode: If Powerwall SOC 15min P1 < 30% → (load_power 1min P99 - current solar)
     - Powerwall Last Mode: Otherwise → 2/3 × (load_power 1min P66 - current solar)
     - Both modes apply **Pressure-Gated Accelerating Ramp** smoothing via `governor.SlowRampState` (see Governor Package below)
   - **Current solar generation**: solar_1 + solar_2 (5min P66), computed once and passed to modes that need it
   - **Per-battery Overflow mode** (SOC-based hysteresis, calculated independently per battery):
     - If NOT in Float Charging: returns 0 inverters
     - Uses separate turn-on and turn-off thresholds to prevent oscillation
     - **Turn OFF thresholds** (when SOC falling): 98.5% → 95.0%, evenly spread
       - Threshold formula: `98.5 - (i-1) * 3.5/(N-1)` for inverter i of N
     - **Turn ON thresholds** (when SOC rising): 95.75% → 99.5%, evenly spread
       - Threshold formula: `95.75 + (i-1) * 3.75/(N-1)` for inverter i of N
     - Uses current SOC value (already smooth enough)
     - No solar subtraction (batteries are full, dumping excess)
   - **Grid outage behavior**: When grid is unavailable (`binary_sensor.home_sweet_home_grid_status_2` == off), per-battery modes (overflow and forecast excess) are disabled (set to 0W). Global modes (Powerwall Last/Low) still operate to help supply the house during outages.
   - **Safety protections** (checked first, disable ALL inverters including global modes):
     - **High frequency protection**: If AC frequency 15min P100 (max) > 53Hz, disable all inverters
     - **Grid off + high Powerwall**: If grid is off AND Powerwall SOC > 90%, disable all inverters
   - **Mode selection** (per-battery first, then global):
     1. Calculate per-battery overflow counts (SOC-based hysteresis, independent per battery)
     2. Calculate per-battery forecast excess counts (excess energy / hours until solar end)
     3. For each battery, take max(overflow, forecast_excess) inverter count
     4. Apply global limit (5000W - solar_1_power 15min P99) to per-battery counts
        - When reducing, reduce from higher count first (B3 wins ties)
     5. Calculate global mode targets with limits (max of all requests, capped by limit)
     6. Compare global inverter count vs limited per-battery total:
        - If global > per-battery: start from limited per-battery counts, round-robin additional inverters (B3→B2→B3→B2...)
        - Otherwise: use limited per-battery counts
     7. Zero targets are not considered "selected" (no winner marked)
   - **Limit**: 5000W - solar_1_power 15min P99 (accounts for solar already flowing)
   - **Round-robin allocation** (when global target exceeds per-battery):
     - Start from limited per-battery counts
     - Add inverters in strict alternation: B3 first, then B2, then B3...
     - SOC limits still apply (via maxInvertersForSOC)
   - **SOC-based limits** (applied to all modes, per-battery, with 2.5% hysteresis):
     - 0 inverters: enters at 12.5%, exits at 15%
     - max 1 inverter: enters at 17.5%, exits at 20%
     - max 2 inverters: enters at 22.5%, exits at 25%
     - SOC >= 25%: all inverters allowed
   - Each inverter: 255W (9 inverters = 2,295W max)
   - **Debug output**: Publishes all mode values to `input_text.powerhouse_control_debug`
     - GFM table showing Forecast Excess (B2), Forecast Excess (B3), Powerwall Last, Powerwall Low, Overflow (B2), Overflow (B3)
     - Sorted by watts descending, winning mode marked with ✓
     - Only publishes when values change via `MQTTSender.CallService()`

10. **mqttSenderWorker** (src/mqtt_sender.go)
    - Dedicated worker for outgoing MQTT messages
    - **Topic constant**: Defines `TopicPowerctlEnabledState` for the powerctl enabled switch state topic
    - Receives MQTTMessage structs via channel (100-message buffer)
    - Receives DisplayData from broadcastWorker to track enabled state
    - Handles message queuing automatically
    - Publishes to MQTT broker with configurable QoS and retain
    - Logs publish failures
    - **Enable/disable filtering**:
      - Subscribes to `TopicPowerctlEnabledState` (`homeassistant/switch/powerctl_enabled/state`)
      - When disabled, drops outgoing messages (except discovery config topics)
      - `--force-enable` flag bypasses this filter for local development
    - Launched automatically when MQTT connection is established

11. **mqttWorker** (src/mqtt_worker.go)
   - Connects to Home Assistant MQTT broker at `homeassistant.lan:1883`
   - Subscribes to multiple sensor topics simultaneously
   - Filters out invalid values ("Undefined", "unavailable") from dropped sensors
   - Forwards received messages to statsWorker via channel
   - Sends MQTT client to mqttSenderWorker when connected
   - Handles reconnection automatically via paho.mqtt client options

12. **debugWorker** (src/debug_worker.go)
   - Interactive introspection of DisplayData values via stdin
   - Enabled via `--debug` flag (included in `make run`)
   - Uses readline for command history and line editing
   - Prompt stays at bottom while output appears above
   - Only prints rows when values change
   - **Commands**:
     - `list` - List all available topics with type indicators
     - `watch <topic>` - Watch current value
     - `watch <topic> -m <1|5|15> -p <1|50|66|99>` - Watch specific time window and percentile
     - `unwatch <topic>` - Remove watch (exact or fuzzy match if single)
     - `unwatch <topic> -m <minutes> -p <percentile>` - Remove specific watch
     - `unwatch --all` - Remove all watches
     - `help` - Show command help
   - **Output format**: Tabular with columns sorted alphabetically by short name
   - **Value formatting**: All values displayed with 2 decimal places
   - **Change highlighting**: Changed values shown in yellow
   - Reprints header when watches are added/removed

13. **sankeyWorker** (src/main.go, inline)
    - Generates and publishes Sankey chart configurations at startup
    - Uses `src/sankey` package for YAML generation
    - **Two outputs** (published via `MQTTSender.CallService`):
      - `notify.sankey_config`: Lovelace sankey chart card configuration
      - `notify.sankey_templates`: Home Assistant template sensor definitions
    - Runs once at startup in its own goroutine (non-blocking)
    - Uses `notify.send_message` service to write file contents

### Data Structures

**MQTT Communication:**
- **SensorMessage**: Incoming MQTT message with topic and value
- **MQTTMessage**: Outgoing MQTT message with topic, payload, QoS, and retain flag
- **MQTTSender** (src/mqtt_sender.go): Wrapper around outgoing channel with helper methods
  - `Send(msg MQTTMessage)` - Sends a raw MQTT message
  - `CallService(domain, service, entityID string, data map[string]string)` - Sends a Home Assistant service call via Node-RED proxy (pass nil for data if not needed)
  - `CreateBatteryEntity(...)` - Creates a Home Assistant entity via MQTT discovery

**Statistics:**
- **Reading**: Timestamped sensor value
- **FloatTopicData**: Holds current value for a numeric sensor topic
  - `Current`: Most recent value
- **StringTopicData**: Holds current value for a string sensor topic
- **BooleanTopicData**: Holds current value for boolean topics (on/off switches, detected by case-insensitive "on"/"off" values)
- **PercentileKey**: Identifies a specific percentile calculation (Topic, Percentile, Window)
- **DisplayData**: Container for topic data broadcast to downstream workers
  - `TopicData`: Map of topic -> FloatTopicData/StringTopicData/BooleanTopicData
  - `Percentiles`: Map of PercentileKey -> float64 (only contains registered percentiles)
  - **Helper methods** (src/main.go):
    - `GetFloat(topic string) *FloatTopicData` - Extracts FloatTopicData with type safety (access `.Current`)
    - `GetPercentile(topic string, percentile int, window time.Duration) float64` - Returns registered percentile value (panics if not registered). Use constants: `P1`, `P50`, `P66`, `P99` for percentiles; `Window5Min`, `Window15Min` for windows.
    - `GetString(topic string) string` - Extracts string value with type safety
    - `GetBoolean(topic string) bool` - Extracts boolean value from BooleanTopicData (for switch states)
    - `GetJSON(topic string, result any)` - Parses JSON string topic into provided pointer. Panics if unmarshaling fails.
    - `SumTopics(topics []string) float64` - Sums multiple float topics (uses `.Current` values)
  - **Topic guarantee**: statsWorker waits for all expected topics before broadcasting any DisplayData. Helper methods that panic on missing/invalid topics (GetPercentile, GetJSON) are safe because topics are guaranteed to be populated.

**Battery Monitoring:**
- **BatteryConfig** (src/battery_config.go): Shared configuration for each battery
  - Name, capacity, manufacturer
  - `InflowEnergyTopics`, `OutflowEnergyTopics`: Cumulative energy topics (kWh) for SOC calculation
  - `InflowPowerTopics`, `OutflowPowerTopics`: Instantaneous power topics (W) for calibration power balance check
  - Charge state topic, voltage topic, calibration thresholds
  - Inverter switch entity IDs for protection control
  - Helper methods: `CalibConfig()`, `SOCConfig()`, `LowVoltageProtectionConfig(threshold)`
- **BuildUnifiedInverterConfig** (src/battery_config.go): Creates UnifiedInverterConfig from two BatteryConfigs
- **BatteryCalibConfig** (src/battery_config.go): Configuration for calibration worker (derived from BatteryConfig)
- **BatterySOCConfig** (src/battery_config.go): Configuration for SOC worker (derived from BatteryConfig)
- **LowVoltageConfig** (src/battery_config.go): Configuration for low voltage protection worker
- **CalibrationTopics** (src/battery_config.go): Statestream topic paths for calibration data

**Unified Inverter Enabler:**
- **UnifiedInverterConfig** (src/unified_inverter_enabler.go): Configuration for unified inverter management
  - Battery2, Battery3 (BatteryInverterGroup): Inverters per battery with entity IDs and state topics
  - SolarForecastTopic, Solar1PowerTopic, LoadPowerTopic: Input topics for mode/target calculation
  - WattsPerInverter (255W), MaxTransferPower (5000W)
  - OverflowSOCTurnOffStart/End (98.5%/95.0%), OverflowSOCTurnOnStart/End (95.75%/99.5%)
- **InverterEnablerState**: Runtime state with per-battery SOC lockout flags

**Sankey Package** (src/sankey/):
- **Config** (config.go): Complete sankey configuration with Sensors and Groups
- **SensorTemplate**: Template sensor definition with Name, Type (Formula/Sum), and template expression
- **Group**: Group of sensors in a section with Name, Section, Sensors, Children, and optional RemainderStrategy
- **Sensor**: Sensor entity with Name and optional Label
- **Section**: Enum for 6 sankey sections (PowerhouseIn, Powerhouse, PowerhouseOut, HouseMainsIn, HouseMains, HouseMainsOut)
- **RemainderStrategy**: Calculated remainder entity with Key, Label, Type, and optional Reconcile rules
- **Reconcile**: Validation rules with ShouldBe and ReconcileTo enums
- **GeneratedConfigs**: Container for both output YAML strings (SankeyConfig, Templates)
- `Generate()`: Main function that produces GeneratedConfigs from DefaultConfig()
- `GenerateSankeyYAML(cfg)`: Generates Lovelace sankey chart card YAML
- `GenerateTemplatesYAML(cfg)`: Generates Home Assistant template sensor definitions

**Governor Package** (src/governor/):
- Provides power governing algorithms for smoothing and rate limiting
- **SlowRampState**: Pressure-Gated Accelerating Ramp smoother
  - `Current`: Current smoothed output value
  - `Pressure`: Signed accumulator tracking sustained change direction
  - `Update(target, config)`: Main entry point - returns smoothed value
- **SlowRampConfig**: Tunable parameters for slow ramp
  - `ThresholdSeconds`: Pressure magnitude required before responding (default: 30)
  - `RateAccel`: Acceleration of ramp rate in units/s² (default: 1.0)
  - `DecayMultiplier`: How much faster pressure drains vs builds (default: 2.0)
- **Algorithm behavior**:
  - Ignores brief fluctuations - only responds after sustained change
  - Slow initial response that accelerates over time (opposite of EMA)
  - Never overshoots - step is capped at remaining difference
  - Hysteresis: pressure drains 2x faster than it builds
  - Pressure capped at 2× threshold (limits max ramp rate to 900 W/s with defaults)
- **Debug sensors**: Pressure values published to HA for tuning:
  - `sensor.powerctl_powerwall_last_pressure`
  - `sensor.powerctl_powerwall_low_pressure`

### Statistics Algorithm

The application uses time-weighted percentiles to account for irregular message arrival times:
1. Each reading is assigned a weight based on how long it was "active"
2. Weight = duration from this reading until the next reading (or until now for the last reading)
3. For percentile calculation:
   - Values are sorted by magnitude
   - Durations are accumulated until reaching the percentile threshold
   - Example: P50 (median) is the value at 50% of total duration
4. **P50** = time-weighted median - stable values have more influence than brief spikes
5. **P66** = 66th percentile - useful for "typical high" values
6. **P1/P99** = 1st/99th percentile - filters out extreme outliers (brief spikes/dips don't affect these)
7. **Last known value preservation**: If no messages arrive in a time window, statistics show the last known value instead of zero
8. At least one reading is always kept (even if older than 15 minutes) to maintain the last known value

### Percentile Registry (Optimization)

To reduce computational overhead, percentiles are only calculated for topics that actually use them. The `requiredPercentiles` map in src/stats.go defines which percentile/window combinations each topic needs:

```go
var requiredPercentiles = map[string][]PercentileSpec{
    TopicSolar1Power: {
        {P50, Window5Min},   // data.GetPercentile(TopicSolar1Power, P50, Window5Min)
        {P66, Window5Min},   // data.GetPercentile(TopicSolar1Power, P66, Window5Min)
        {P99, Window15Min},  // data.GetPercentile(TopicSolar1Power, P99, Window15Min)
    },
    // ... only ~10 topics need percentiles
}
```

**Benefits:**
- Topics not in the registry only track `Current` value (no percentile calculation)
- Percentiles are stored in a separate map, accessed via `GetPercentile(topic, percentile, window)` using constants
- `GetPercentile` panics if requesting an unregistered percentile (catches bugs at development time)
- Calculations are grouped by window to share filtering and sorting work
- Reduces overall percentile calculation work by ~95%

**Live Updates:**
- A 1-second ticker recalculates percentiles for all registered topics and broadcasts DisplayData
- This is the only source of downstream broadcasts (no debouncing on message arrival)
- Keeps percentiles fresh as time passes (since weights depend on duration)
- Only ~10 topics × ~2 window calculations = minimal overhead

**Adding new percentile usage:**
When a worker needs a new percentile/window combination, add it to `requiredPercentiles` in src/stats.go.

### Message Flow

**Incoming (from MQTT):**
```
MQTT Broker → mqttWorker → SensorMessage → statsWorker → DisplayData → broadcastWorker → (fan-out)
                             channel                       channel                         ├─→ batteryCalibWorker (Battery 2)
                                                                                           ├─→ batteryCalibWorker (Battery 3)
                                                                                           ├─→ batterySOCWorker (Battery 2)
                                                                                           ├─→ batterySOCWorker (Battery 3)
                                                                                           ├─→ lowVoltageWorker (Battery 2)
                                                                                           ├─→ lowVoltageWorker (Battery 3)
                                                                                           ├─→ unifiedInverterEnabler
                                                                                           ├─→ powerExcessCalculator → excessChan → dumpLoadEnabler
                                                                                           ├─→ mqttSenderWorker (for enabled state tracking)
                                                                                           └─→ debugWorker (if --debug enabled)
```

**Outgoing (to MQTT/Home Assistant):**
```
batteryCalibWorker → MQTTMessage → mqttOutgoingChan → mqttSenderWorker → MQTT Broker → Home Assistant
(attributes)            channel      (100 msg buffer)                                       (calibration data)

batterySOCWorker → MQTTMessage → mqttOutgoingChan → mqttSenderWorker → MQTT Broker → Home Assistant
(state updates)    channel      (100 msg buffer)                                       (percentage + available_wh)

lowVoltageWorker → MQTTMessage → mqttOutgoingChan → mqttSenderWorker → MQTT Broker → Node-RED → Home Assistant
(service calls)    channel      (100 msg buffer)                                       (nodered/proxy/call_service)

unifiedInverterEnabler → MQTTMessage → mqttOutgoingChan → mqttSenderWorker → MQTT Broker → Node-RED → Home Assistant
(switch control)          channel      (100 msg buffer)                                       (switch.turn_on/turn_off)

dumpLoadEnabler → MQTTMessage → mqttOutgoingChan → mqttSenderWorker → MQTT Broker → Node-RED → Home Assistant
(workmode)         channel      (100 msg buffer)                                       (select.select_option)
```

**Calibration Data Loop:**
```
batteryCalibWorker → MQTT attributes topic → Home Assistant statestream → MQTT statestream topic →
    mqttWorker → statsWorker → DisplayData → broadcastWorker → batterySOCWorker
```

**Flow Details:**
- **statsWorker** waits for all topics (including calibration statestream topics) before sending data
- **broadcastWorker** fans out to all downstream workers using non-blocking sends
- **batteryCalibWorker** detects calibration events and publishes reference values to attributes topic
- **Home Assistant statestream** republishes attributes as sensor topics for consumption
- **batterySOCWorker** reads calibration data from DisplayData and calculates battery percentage
- **lowVoltageWorker** monitors 15-min minimum voltage and triggers inverter shutoff via Node-RED proxy
- **powerExcessCalculator** aggregates battery and power data to calculate excess watts, sends to dumpLoadEnabler
- **dumpLoadEnabler** adjusts miner workmode based on excess power thresholds
- **unifiedInverterEnabler** manages all inverters with mode-based target power and round-robin distribution
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

1. Create a worker function that receives `<-chan DisplayData` (see existing workers in `src/battery_*_worker.go` as examples)
2. In `src/main.go`, create a channel: `newChan := make(chan DisplayData, 10)`
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
    battery2CalibChan,
    battery3CalibChan,
    battery2SOCChan,
    battery3SOCChan,
    controlChan,  // <-- Add here
}
```

Example downstream workers could:
- Control smart switches based on power thresholds
- Send alerts when values exceed limits
- Log data to files or databases
- Expose metrics via HTTP endpoints
- Call Home Assistant services via Node-RED MQTT proxy

### Calling Home Assistant Services

A Node-RED flow is configured to proxy Home Assistant service calls via MQTT.

**Topic:** `nodered/proxy/call_service`

**Payload schema:**
```json
{
  "domain": "switch",
  "service": "turn_on",
  "entity_id": "switch.example"
}
```

This allows powerctl to control any Home Assistant entity by publishing to the MQTT broker.

### Tracking Home Assistant Entity State

**IMPORTANT:** Never track Home Assistant entity state locally in workers (e.g., using maps or variables to remember the "current" state of switches or selects). External actors (users, automations, other systems) can change entity states at any time, making local state invalid.

**Instead:** Subscribe to the entity's state topic via statsWorker and read the actual state from DisplayData:
```go
// Wrong - local state tracking becomes stale
currentState := inverterStates[entityID]  // Don't do this

// Right - read actual state from Home Assistant
currentState := data.GetBoolean(stateTopic)  // For switches (returns true if "on")
currentWorkmode := data.GetString(stateTopic)  // For selects (returns the option string)
```

Entity state topics follow the pattern:
- Switches: `homeassistant/switch/{object_id}/state` (values: "on", "off")
- Selects: `homeassistant/select/{object_id}/state` (values: the current option)
- Sensors: `homeassistant/sensor/{object_id}/state`

Add these topics to the subscription list in main.go so statsWorker tracks them.

### Dependencies

- `github.com/eclipse/paho.mqtt.golang` - MQTT client
- `github.com/joho/godotenv` - Environment variable loading
- `github.com/stretchr/testify` - Test assertions
- `github.com/chzyer/readline` - Readline for debug worker command history

### Configuration

MQTT credentials are loaded from a `.env` file (see `.env.example` for template):
- `MQTT_USERNAME` - MQTT broker username
- `MQTT_PASSWORD` - MQTT broker password
- `MQTT_CLIENT_ID` - Optional client ID (default: "powerctl", use "powerctl-dev" for local development)

MQTT connection settings in main():
- Broker: `homeassistant.lan`
- Port: `1883`

**Topics monitored** (defined in src/main.go):

Battery 2 (9.5 kWh) outflows:
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

Battery monitoring (energy):
- `homeassistant/sensor/solar_5_total_energy/state` (Battery 2 inflow)
- `homeassistant/sensor/solar_5_charge_state/state`
- `homeassistant/sensor/solar_5_battery_voltage/state`
- `homeassistant/sensor/solar_3_total_energy/state` (Battery 3 inflow)
- `homeassistant/sensor/solar_4_total_energy/state` (Battery 3 inflow)
- `homeassistant/sensor/solar_3_charge_state/state`
- `homeassistant/sensor/solar_3_battery_voltage/state`

Battery monitoring (power - for calibration balance check):
- `homeassistant/sensor/solar_5_solar_power/state` (Battery 2 inflow power)
- `homeassistant/sensor/solar_3_solar_power/state` (Battery 3 inflow power)
- `homeassistant/sensor/solar_4_solar_power/state` (Battery 3 inflow power)
- `homeassistant/sensor/powerhouse_inverter_[1-4]_switch_0_power/state` (Battery 2 outflow power)
- `homeassistant/sensor/powerhouse_inverter_[5-9]_switch_0_power/state` (Battery 3 outflow power)

Battery calibration (statestream topics from Home Assistant):
- `homeassistant/sensor/battery_2_state_of_charge/calibration_inflows`
- `homeassistant/sensor/battery_2_state_of_charge/calibration_outflows`
- `homeassistant/sensor/battery_3_state_of_charge/calibration_inflows`
- `homeassistant/sensor/battery_3_state_of_charge/calibration_outflows`

Power excess calculation (defined in src/power_excess_calculator.go):
- `homeassistant/sensor/home_sweet_home_tg118095000r1a_battery_remaining/state` (Tesla battery)
- `homeassistant/sensor/battery_2_available_energy/state`
- `homeassistant/sensor/battery_3_available_energy/state`
- `homeassistant/sensor/solar_1_power/state` (Solar 1 power)

Unified inverter enabler (defined in src/battery_config.go):
- `homeassistant/sensor/solcast_pv_forecast_forecast_today/state` (Solar forecast kWh)
- `homeassistant/sensor/solar_1_power/state` (Current solar power)
- `homeassistant/sensor/home_sweet_home_load_power_2/state` (Load power)
- `homeassistant/sensor/home_sweet_home_charge/state` (Powerwall SOC %)
- `homeassistant/binary_sensor/home_sweet_home_grid_status_2/state` (Grid availability)
- `homeassistant/sensor/lounge_ac_frequency/state` (AC frequency for high frequency protection)
- `homeassistant/switch/powerhouse_inverter_[1-9]_switch_0/state` (Inverter switch states)

Powerctl control (defined in src/mqtt_sender.go):
- `homeassistant/switch/powerctl_enabled/state` (Enabled state for message filtering)

**MQTT Publishing:**

Battery entities are auto-created at startup via Home Assistant MQTT discovery (`MQTTSender.CreateBatteryEntity`):
- Config topics: `homeassistant/sensor/battery_2_[percentage|available_wh]/config`
- Config topics: `homeassistant/sensor/battery_3_[percentage|available_wh]/config`
- State topics: `homeassistant/sensor/battery_2/state` (JSON with percentage + available_wh)
- State topics: `homeassistant/sensor/battery_3/state` (JSON with percentage + available_wh)
- Attributes topics: `homeassistant/sensor/battery_2/attributes` (JSON with calibration_inflows + calibration_outflows)
- Attributes topics: `homeassistant/sensor/battery_3/attributes` (JSON with calibration_inflows + calibration_outflows)

Powerctl switch is auto-created at startup via MQTT discovery (`MQTTSender.CreatePowerctlSwitch`):
- Config topic: `homeassistant/switch/powerctl_enabled/config`
- State topic: `homeassistant/switch/powerctl_enabled/state` (managed by Home Assistant, optimistic mode)

**Home Assistant Statestream:**
The calibration attributes are republished by Home Assistant's statestream integration as separate sensor topics, which are then subscribed to by mqttWorker and used by batterySOCWorker to calculate state of charge.

**Setup**: Copy `.env.example` to `.env` and fill in your credentials.

**Adding topics**: Edit the `topics` slice in src/main.go to add or remove monitored sensors.

**Command Line Flags:**
- `--force-enable`: Bypass the powerctl_enabled switch. Use this for local development when the deployed instance should be disabled via the switch in Home Assistant. The deployed instance (without this flag) will stop sending commands when the switch is turned off, while the local instance (with this flag) will continue to operate normally.
- `--debug`: Enable the debug introspection worker. Provides an interactive prompt for watching sensor values in real-time. Included in `make run` for local development.

- If there are more than 3 arguments to a function definition, put each one on a new line
  - multiple arguments sharing the same type do not count for this purpose, eg.
    - `func(a int, b int, c int)` is 3 arguments
    - `func(a, b, c int)` is 1 argument
  
- Before making a commit, update CLAUDE.md