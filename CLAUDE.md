# CLAUDE.md

This file provides guidance to Claude Code when working with this repository.

## Project Overview

`powerctl` is a Go-based MQTT client that monitors Home Assistant sensors and tracks battery state of charge. Connects to Home Assistant MQTT broker, subscribes to sensor topics, calculates real-time statistics, and monitors battery levels with automatic HA integration.

**Batteries:**
- Battery 2: 9.5 kWh (SunnyTech) - Solar 5 inflow, Inverters 1-4 outflow
- Battery 3: 15 kWh (Micromall) - Solar 3 & 4 inflow, Inverters 5-9 outflow

## Development Commands

```bash
nix-shell           # Enter dev environment
make build          # Build binary
make run            # Build and run with --force-enable --debug
make check          # Run linter, tests, verify vendorHash (ALWAYS run before commit)
make clean          # Remove binary
go test ./...       # Run tests
```

## Architecture

Goroutine-based with message passing via channels. All source code in `src/`.

### Core Components

1. **SafeGo** (src/main.go) - Launches goroutines with panic recovery; cancels app context on panic

2. **statsWorker** (src/stats.go) - Receives SensorMessage, maintains per-topic state, calculates percentiles only for topics in `requiredPercentiles` registry. 1-second ticker broadcasts DisplayData. Waits for all expected topics before sending. After 20s, initializes missing self-published topics.

3. **broadcastWorker** (src/broadcast_worker.go) - Actor pattern fan-out to downstream workers using non-blocking sends

4. **batteryCalibWorker** (src/battery_calib_worker.go) - Detects calibration events (Float Charging + voltage ≥ 53.6V + |net power| ≤ 250W), publishes reference points. Soft-caps SOC based on charge state when not in Float.

5. **batterySOCWorker** (src/battery_soc_worker.go) - Calculates SOC from calibration references with 10% conversion loss on outflows

6. **lowVoltageWorker** (src/low_voltage_worker.go) - Monitors 15-min minimum voltage, turns off inverters at 50.75V threshold

7. **powerExcessCalculator** (src/power_excess_calculator.go) - Calculates excess power for dump loads based on battery levels and solar

8. **dumpLoadEnabler** (src/dump_load_enabler.go) - Controls miner workmode (Super/Standard/Eco/Sleep) based on excess power

9. **unifiedInverterEnabler** (src/unified_inverter_enabler.go) - Manages all 9 inverters with multiple modes:
   - **Forecast Excess**: Targets 100% battery by solar end using `excess_wh / hours_until_solar_end`
   - **Powerwall Low**: SOC-based hysteresis (ON: 41%→25%, OFF: 28%→44%)
   - **Powerwall Last**: 2/3 × (load - solar) with pressure-gated ramp smoothing
   - **Overflow**: Per-battery SOC hysteresis when Float Charging (OFF: 98.5%→95%, ON: 95.75%→99.5%)
   - **Safety**: High frequency (>52.75Hz) or grid off + Powerwall >90% disables all
   - **Selection**: max(overflow, forecast_excess) per battery, then global modes, round-robin allocation
   - **Limit**: 5000W - solar_1_power 15min P99
   - **SOC limits**: Per-battery hysteresis with steps = inverter count (ON: 15%→25%, OFF: 12.5%→22.5%)

10. **mqttSenderWorker** (src/mqtt_sender.go) - Outgoing MQTT with 100-msg buffer, filters based on `powerctl_enabled` switch

11. **mqttInterceptorWorker** (src/mqtt_interceptor.go) - Filters inverter messages via `powerhouse_inverters_enabled` switch

12. **mqttWorker** (src/mqtt_worker.go) - Connects to MQTT broker, subscribes to topics, forwards to statsWorker

13. **debugWorker** (src/debug_worker.go) - Interactive introspection via `--debug` flag. Commands: list, watch, unwatch, help

14. **sankeyWorker** (src/main.go) - Generates Sankey chart configs at startup via `src/sankey` package

### Data Structures

**DisplayData** (broadcast to all workers):
- `TopicData`: Map of topic → FloatTopicData/StringTopicData/BooleanTopicData
- `Percentiles`: Map of PercentileKey → float64 (only registered percentiles)
- Helpers: `GetFloat(topic)`, `GetPercentile(topic, percentile, window)`, `GetString(topic)`, `GetBoolean(topic)`, `GetJSON(topic, result)`, `SumTopics(topics)`
- **Topic guarantee**: statsWorker waits for all expected topics; helpers that panic are safe

**MQTTSender** (src/mqtt_sender.go):
- `Send(msg)` - Raw MQTT message
- `CallService(domain, service, entityID, data)` - HA service via Node-RED proxy
- `CreateBatteryEntity(...)` - HA entity via MQTT discovery

**BatteryConfig** (src/battery_config.go): Shared config with inflow/outflow topics, calibration settings. Helpers: `CalibConfig()`, `SOCConfig()`, `LowVoltageProtectionConfig(threshold)`

**Governor Package** (src/governor/):
- **SlowRampState**: Pressure-gated accelerating ramp smoothing. Ignores brief fluctuations, responds after sustained change.
- **SteppedHysteresis**: Converts continuous values to discrete steps with separate enter/exit thresholds. Constructor: `NewSteppedHysteresis(steps, ascending, increaseStart, increaseEnd, decreaseStart, decreaseEnd)`. Call `Update(value)` to get current step.
  - Ascending mode (value↑ → step↑): Overflow, SOC Limits
  - Descending mode (value↓ → step↑): Powerwall Low
  - Thresholds linearly interpolated from start→end for steps 1 through N

### Statistics Algorithm

Time-weighted percentiles: weight = duration until next reading. P50 = median, P66 = typical high, P1/P99 = filter outliers. Last known value preserved if no messages.

**Percentile Registry** (src/stats.go): Add to `requiredPercentiles` map when worker needs new percentile/window combination. `GetPercentile` panics if unregistered.

### Message Flow

```
MQTT → mqttWorker → statsWorker → broadcastWorker → downstream workers
                                                  → batteryCalibWorker (×2)
                                                  → batterySOCWorker (×2)
                                                  → lowVoltageWorker (×2)
                                                  → unifiedInverterEnabler
                                                  → powerExcessCalculator → dumpLoadEnabler
                                                  → debugWorker (if --debug)

Outgoing: workers → MQTTMessage → mqttOutgoingChan → mqttSenderWorker → MQTT
Inverters: unifiedInverterEnabler → inverterOutgoingChan → mqttInterceptorWorker → mqttOutgoingChan
```

**Calibration loop**: calibWorker → MQTT attributes → HA statestream → MQTT → statsWorker → SOCWorker

### Concurrency

- `SafeGo` wraps goroutines with panic recovery
- Buffered channels: 10 for data, 100 for outgoing MQTT
- Context for lifecycle management; any panic shuts down app

### Adding Downstream Workers

1. Create worker receiving `<-chan DisplayData`
2. Create channel: `newChan := make(chan DisplayData, 10)`
3. Launch: `SafeGo(ctx, cancel, "name", func(ctx) { worker(ctx, newChan) })`
4. Add to `downstreamChans` slice

### HA Service Calls

Topic: `nodered/proxy/call_service`
```json
{"domain": "switch", "service": "turn_on", "entity_id": "switch.example"}
```

### Entity State Tracking

**Never track HA state locally** - external actors can change it. Subscribe to state topic and read from DisplayData:
```go
data.GetBoolean(stateTopic)  // switches
data.GetString(stateTopic)   // selects
```

### Configuration

MQTT credentials in `.env` (see `.env.example`): `MQTT_USERNAME`, `MQTT_PASSWORD`, `MQTT_CLIENT_ID`

**Flags:**
- `--force-enable`: Bypass enabled switches (local dev)
- `--debug`: Interactive debug worker

## Code Style

- If >3 arguments to function, put each on new line (shared-type args count as 1)
- Before committing, update CLAUDE.md if necessary, and consolidate it to keep it as small as possible
