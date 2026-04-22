# CLAUDE.md

This file provides guidance to Claude Code when working with this repository.
You should use the home assistant integration to investigate and confirm names, options and behaviours; it is preferrable to automate and manage things within Powerctl.
When controlling devices in powerctl you can take two approaches:
1. Rely on internal state - this is good when the humans want to be able to override what is happening, but can be confusing #2 should be preferred unless there is good reason.
2. Rely on HA state via MQTT - this way there is not a desync between the HA view and powerctl view

## Project Overview

`powerctl` is a Go-based MQTT client that monitors Home Assistant sensors and tracks battery state of charge. Connects to Home Assistant MQTT broker, subscribes to sensor topics, calculates real-time statistics, and monitors battery levels with automatic HA integration.

**Batteries:**
- Battery 2: 9.5 kWh (SunnyTech) - Solar 5 inflow, Inverters 1-9 outflow
- Battery 3: 3×14.5 kWh (Micromall) - Solar 3 & 4 inflow, Multiplus 2 48/5000/70 outflow

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

6. **powerExcessCalculator** (src/power_excess_calculator.go) - Calculates excess power for dump loads based on battery levels and solar

7. **dumpLoadEnabler** (src/dump_load_enabler.go) - Controls miner workmode (Super/Standard/Eco/Sleep) based on excess power

8. **baselineInverterControl** (src/baseline_inverter_control.go) - Manages Battery 2 inverters (1-9) with multiple modes:
   - **Overflow**: Float Charging + SOC hysteresis (ON: 95.75%→99.5%, OFF: 98.5%→95%)
   - **Forecast Excess**: Targets 100% battery by solar end using `excess_wh / hours_until_solar_end`
   - **Baseline**: 7-day P2 of hourly house-load minimums minus solar (capped at 500W)
   - **Safety**: High frequency (>52.75Hz) or grid off + Powerwall >90% disables all
   - **SOC limits**: Battery 2 hysteresis (ON: 15%→25%, OFF: 12.5%→22.5%)
   - **Limit**: 5000W - solar_1_power 15min P90 (skipped when Battery 3 SOC < 85%)
   - Selection: `max(overflow, forecast_excess, baseline)` then apply safety/SOC limits

9. **dynamicInverterControl** (src/dynamic_inverter_control.go) - Actively controls Multiplus II (Battery 3) setpoint every 5s. Range: -3000W to +3500W.
   - **Auto mode** (`powerctl_dynamic_auto` switch on): calculates setpoint, writes to HA entity for visibility
   - **Manual mode** (switch off): reads user-set HA number entity, passes through to Cerbo
   - **Priority 2 – Default Supply**: discharge to fill gap between house load max and total generation
   - **Priority 3 – Charge from Surplus**: charge from powerhouse-side excess
   - **4.5kW hard transfer limit**: `solar_1 + inverter_1_9 + multiplus_discharge ≤ 4500W`; forces charge when exceeded
   - **Safety**: high frequency or grid off + Powerwall >90% suppresses discharge

10. **debugAggregatorWorker** (src/debug_aggregator_worker.go) - Receives `BaselineDebugInfo` and `DynamicDebugInfo`, renders a combined side-by-side GFM markdown table, publishes to `input_text.powerhouse_control_debug` on change only.

11. **mqttSenderWorker** (src/mqtt_sender.go) - Outgoing MQTT with 100-msg buffer, filters based on `powerctl_enabled` switch

12. **mqttInterceptorWorker** (src/mqtt_interceptor.go) - Filters inverter messages via `powerctl_inverter_enabled` switch

13. **mqttWorker** (src/mqtt_worker.go) - Connects to MQTT broker, subscribes to topics, forwards to statsWorker

14. **debugWorker** (src/debug_worker.go) - Interactive introspection via `--debug` flag. Commands: list, watch, unwatch, help

15. **sankeyWorker** (src/main.go) - Generates Sankey chart configs at startup via `src/sankey` package

16. **cerboKeepaliveWorker** (src/powerhouse3.go) - Sends Victron GX keepalive every 50s so Cerbo keeps publishing N/ topics

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

**BatteryConfig** (src/battery_config.go): Shared config with inflow/outflow topics, calibration settings. Helpers: `CalibConfig()`, `SOCConfig()`, `BuildBaselineInverterConfig(battery2, battery3)`, `BuildDynamicInverterConfig(battery2, battery3)`

**Inverter types** (src/inverter_common.go): `PowerRequest`, `PowerLimit`, `InverterInfo`, `BatteryInverterGroup`, `BatteryOverflowState`, `ModeState`. Shared helpers: `checkBatteryOverflow`, `forecastExcessRequest`, `applyInverterChanges`, etc.

**Governor Package** (src/governor/):
- **SteppedHysteresis**: Converts continuous values to discrete steps with separate enter/exit thresholds. Constructor: `NewSteppedHysteresis(steps, ascending, increaseStart, increaseEnd, decreaseStart, decreaseEnd)`. Call `Update(value)` to get current step.
  - Ascending mode (value↑ → step↑): Overflow, SOC Limits
  - Thresholds linearly interpolated from start→end for steps 1 through N
- **RollingMinMax**: `NewRollingMinMax(minutes)`, `NewRollingMinMaxSeconds(seconds)`, `NewRollingMinMaxHours(hours)`. `BucketMinPercentile(p)` returns p-th percentile of per-bucket minimums (used by 7-day baseline).

### Statistics Algorithm

Time-weighted percentiles: weight = duration until next reading. P50 = median, P90 = high, P100 = max. Last known value preserved if no messages.

**Percentile Registry** (src/stats.go): Add to `requiredPercentiles` map when worker needs new percentile/window combination. `GetPercentile` panics if unregistered.

### Message Flow

```
MQTT → mqttWorker → statsWorker → broadcastWorker → downstream workers
                                                  → batteryCalibWorker (×2)
                                                  → batterySOCWorker (×2)
                                                  → baseline-input-bridge → baselineInverterControl → baselineDebugChan ─┐
                                                  → dynamic-input-bridge  → dynamicInverterControl  → dynamicDebugChan  ─┤→ debugAggregatorWorker → HA
                                                  → powerExcessCalculator → dumpLoadEnabler
                                                  → debugWorker (if --debug)

Outgoing: workers → MQTTMessage → mqttOutgoingChan → mqttSenderWorker → MQTT
Inverters: baselineInverterControl → inverterOutgoingChan → mqttInterceptorWorker → mqttOutgoingChan
Dynamic: dynamicInverterControl → mqttOutgoingChan (direct, bypasses interceptor)
```

**Calibration loop**: calibWorker → MQTT attributes → HA statestream → MQTT → statsWorker → SOCWorker

### Input Extraction Pattern

The baseline and dynamic controllers take typed input channels (`<-chan BaselineInput`, `<-chan DynamicInput`) for testability. In main.go, a bridge goroutine reads DisplayData, calls `ExtractBaselineInput(data, config.Input)` or `ExtractDynamicInput(data, config.Input)`, and forwards to the controller's input channel.

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
