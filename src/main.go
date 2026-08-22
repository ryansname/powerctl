package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/joho/godotenv"

	"github.com/ryansname/powerctl/src/sankey"
)

// SensorMessage represents an MQTT message with topic and value
type SensorMessage struct {
	Topic string
	Value string
}

// DisplayData holds all data needed for display
type DisplayData struct {
	TopicData   map[string]any
	Percentiles map[PercentileKey]float64
}

// GetFloat extracts FloatTopicData from DisplayData
// Returns a zero-valued FloatTopicData if topic doesn't exist or isn't a float topic
func (d *DisplayData) GetFloat(topic string) *FloatTopicData {
	if td, ok := d.TopicData[topic].(*FloatTopicData); ok {
		return td
	}
	return &FloatTopicData{}
}

// GetPercentile returns a percentile value for a topic.
// Panics if the topic/percentile/window combination is not in requiredPercentiles.
func (d *DisplayData) GetPercentile(topic string, percentile int, window time.Duration) float64 {
	key := PercentileKey{topic, percentile, window}
	if value, exists := d.Percentiles[key]; exists {
		return value
	}

	// Slow path: diagnose why it's missing
	specs, topicExists := requiredPercentiles[topic]
	if !topicExists {
		panic(fmt.Sprintf("GetPercentile: topic %q is not in requiredPercentiles registry", topic))
	}

	for _, spec := range specs {
		if spec.Percentile == percentile && spec.Window == window {
			panic(fmt.Sprintf("GetPercentile: P%d with %v window is registered for %q but was not calculated", percentile, window, topic))
		}
	}

	panic(fmt.Sprintf("GetPercentile: P%d with %v window is not registered for topic %q (add it to requiredPercentiles)", percentile, window, topic))
}

// GetString extracts a string value from DisplayData.
// Trims surrounding quotes in case the MQTT payload is JSON-encoded.
// Also works for boolean topics, returning the raw value (e.g. "off").
func (d *DisplayData) GetString(topic string) string {
	switch td := d.TopicData[topic].(type) {
	case *StringTopicData:
		return strings.Trim(td.Current, "\"")
	case *BooleanTopicData:
		return strings.Trim(td.Raw, "\"")
	}
	return ""
}

// GetBoolean extracts a boolean value from DisplayData.
func (d *DisplayData) GetBoolean(topic string) bool {
	if td, ok := d.TopicData[topic].(*BooleanTopicData); ok {
		return td.Current
	}
	return false
}

// SumTopics calculates the sum of all specified topics
func (d *DisplayData) SumTopics(topics []string) float64 {
	var total float64
	for _, topic := range topics {
		total += d.GetFloat(topic).Current
	}
	return total
}

// GetJSON parses a JSON string topic into the provided pointer.
// Go doesn't support generic methods, so caller passes pointer to result.
// Panics if JSON unmarshaling fails.
func (d *DisplayData) GetJSON(topic string, result any) {
	jsonStr := d.GetString(topic)
	if err := json.Unmarshal([]byte(jsonStr), result); err != nil {
		panic(fmt.Sprintf("GetJSON: failed to unmarshal topic %q: %v", topic, err))
	}
}

// buildTopicsList creates the MQTT subscription list from battery configs
func buildTopicsList(batteries []BatteryConfig) []string {
	var topics []string
	for _, b := range batteries {
		topics = append(topics, b.InflowEnergyTopics...)
		topics = append(topics, b.OutflowEnergyTopics...)
		topics = append(topics, b.InflowPowerTopics...)
		topics = append(topics, b.OutflowPowerTopics...)
		topics = append(topics, b.ChargeStateTopic)
		topics = append(topics, b.BatteryVoltageTopic)
		topics = append(topics, b.CalibrationTopics.Inflows, b.CalibrationTopics.Outflows)
	}
	return topics
}

// SafeGo launches a goroutine with panic recovery and retry logic.
// On panic, retries with exponential backoff (max 10 retries).
// Retry count resets if worker ran for 2+ minutes before failing.
// After exhausting retries, cancels context to trigger shutdown.
func SafeGo(
	ctx context.Context,
	cancel context.CancelFunc,
	name string,
	fn func(ctx context.Context),
) {
	const maxRetries = 10
	const maxDelay = 10 * time.Minute
	const resetAfter = 2 * time.Minute

	go func() {
		retries := 0
		delay := time.Second

		for {
			startTime := time.Now()
			var panicValue any

			func() {
				defer func() {
					panicValue = recover()
				}()
				fn(ctx)
			}()

			// If function returned normally (no panic), exit the goroutine
			// This covers both context cancellation and unexpected completion
			if panicValue == nil {
				return
			}

			// If ran for resetAfter duration before panicking, reset retry state
			if time.Since(startTime) >= resetAfter {
				retries = 0
				delay = time.Second
			}

			retries++
			log.Printf("Panic in %s (attempt %d/%d): %v\n", name, retries, maxRetries, panicValue)

			// Check if we've exhausted retries
			if retries >= maxRetries {
				log.Printf("%s failed after %d retries, shutting down\n", name, maxRetries)
				cancel()
				return
			}

			// Wait before retry with exponential backoff
			log.Printf("%s will retry in %v\n", name, delay)
			select {
			case <-time.After(delay):
				// Double delay for next time, cap at max
				delay = min(delay*2, maxDelay)
			case <-ctx.Done():
				return
			}
		}
	}()
}

func main() {
	// Parse command line flags
	forceEnable := flag.Bool("force-enable", false, "Bypass powerctl_enabled switch")
	debugMode := flag.Bool("debug", false, "Enable debug introspection worker")
	multiplusOnly := flag.Bool("multiplus-only", false, "Drop all outgoing MQTT messages whose topic is not under powerhouse_3/")
	dataDir := flag.String("data-dir", "", "Directory for persistent state (state.toml). Defaults to $POWERCTL_DATA_DIR, else the current directory.")
	heartbeatDir := flag.String("heartbeat-dir", "", "Directory for the liveness heartbeat file. Defaults to $POWERCTL_HEARTBEAT_DIR, else the data dir. Should be ephemeral scratch, not durable storage.")
	healthcheck := flag.Bool("healthcheck", false, "Exit 0 if powerctl has published to MQTT recently, non-zero otherwise, then exit. Used as the container liveness probe.")
	flag.Parse()

	// --healthcheck is a second, short-lived process run by the k8s exec probe
	// inside the running container: it only reads the heartbeat file the main
	// process writes, so it must return before any of the startup below.
	if *healthcheck {
		os.Exit(runHealthcheck(resolveHeartbeatDir(*heartbeatDir, *dataDir)))
	}

	log.Println("Starting powerctl...")

	if *forceEnable {
		log.Println("WARNING: --force-enable active, ignoring powerctl_enabled switch")
	}

	if *multiplusOnly {
		log.Println("WARNING: --multiplus-only active, only powerhouse_3/ outgoing messages will be sent")
	}

	// Load .env file
	if err := godotenv.Load(); err != nil {
		log.Printf("Warning: Error loading .env file: %v\n", err)
	}

	// Resolve the persistent-state directory (flag > $POWERCTL_DATA_DIR > cwd).
	resolvedDataDir := resolveDir(*dataDir, "POWERCTL_DATA_DIR", ".")
	if err := os.MkdirAll(resolvedDataDir, 0o750); err != nil { //nolint:gosec // path from operator-set --data-dir/env, not untrusted input
		log.Fatalf("Cannot create data dir %s: %v", resolvedDataDir, err) //nolint:gosec // operator-set path
	}
	stateFilePath := filepath.Join(resolvedDataDir, "state.toml")
	log.Printf("Persistent state file: %s\n", stateFilePath) //nolint:gosec // operator-set path

	// Liveness heartbeat, written from the publish path (see heartbeat.go) and
	// read back by --healthcheck. Defaults into the data dir so a bare run works,
	// but deployments point it at ephemeral scratch.
	resolvedHeartbeatDir := resolveHeartbeatDir(*heartbeatDir, *dataDir)
	if err := os.MkdirAll(resolvedHeartbeatDir, 0o750); err != nil { //nolint:gosec // path from operator-set --heartbeat-dir/env, not untrusted input
		log.Fatalf("Cannot create heartbeat dir %s: %v", resolvedHeartbeatDir, err) //nolint:gosec // operator-set path
	}
	heartbeat := NewHeartbeat(resolvedHeartbeatDir)
	log.Printf("Liveness heartbeat file: %s\n", filepath.Join(resolvedHeartbeatDir, heartbeatFileName)) //nolint:gosec // operator-set path

	// Get MQTT credentials from environment
	mqttUsername := os.Getenv("MQTT_USERNAME")
	mqttPassword := os.Getenv("MQTT_PASSWORD")

	if mqttUsername == "" || mqttPassword == "" {
		log.Fatal("MQTT_USERNAME and MQTT_PASSWORD must be set in .env file")
	}

	// Get MQTT client ID from environment, default to "powerctl"
	mqttClientID := os.Getenv("MQTT_CLIENT_ID")
	if mqttClientID == "" {
		mqttClientID = deviceIDPowerctl
	}

	// Get MQTT host from environment, default to "homeassistant.lan"
	mqttHost := os.Getenv("MQTT_HOST")
	if mqttHost == "" {
		mqttHost = "homeassistant.lan"
	}

	// Get MQTT port from environment, default to 1883
	mqttPort := 1883
	if portStr := os.Getenv("MQTT_PORT"); portStr != "" {
		p, err := strconv.Atoi(portStr)
		if err != nil {
			log.Fatalf("MQTT_PORT must be a valid integer: %v", err)
		}
		mqttPort = p
	}

	// Create context for lifecycle management
	ctx, cancel := context.WithCancel(context.Background())

	// Define battery configurations
	battery2 := BatteryConfig{
		Name:         "Battery 2",
		CapacityKWh:  9.5,
		Manufacturer: "SunnyTech Solar",
		InflowEnergyTopics: []string{
			"homeassistant/sensor/solar_5_total_energy/state",
		},
		OutflowEnergyTopics: []string{
			"homeassistant/sensor/powerhouse_inverter_1_switch_0_energy/state",
			"homeassistant/sensor/powerhouse_inverter_2_switch_0_energy/state",
			"homeassistant/sensor/powerhouse_inverter_3_switch_0_energy/state",
			"homeassistant/sensor/powerhouse_inverter_4_switch_0_energy/state",
			"homeassistant/sensor/powerhouse_inverter_5_switch_0_energy/state",
			"homeassistant/sensor/powerhouse_inverter_6_switch_0_energy/state",
			"homeassistant/sensor/powerhouse_inverter_7_switch_0_energy/state",
			"homeassistant/sensor/powerhouse_inverter_8_switch_0_energy/state",
			"homeassistant/sensor/powerhouse_inverter_9_switch_0_energy/state",
		},
		InflowPowerTopics: []string{
			"homeassistant/sensor/solar_5_solar_power/state",
		},
		OutflowPowerTopics: []string{
			"homeassistant/sensor/powerhouse_inverter_1_switch_0_power/state",
			"homeassistant/sensor/powerhouse_inverter_2_switch_0_power/state",
			"homeassistant/sensor/powerhouse_inverter_3_switch_0_power/state",
			"homeassistant/sensor/powerhouse_inverter_4_switch_0_power/state",
			"homeassistant/sensor/powerhouse_inverter_5_switch_0_power/state",
			"homeassistant/sensor/powerhouse_inverter_6_switch_0_power/state",
			"homeassistant/sensor/powerhouse_inverter_7_switch_0_power/state",
			"homeassistant/sensor/powerhouse_inverter_8_switch_0_power/state",
			"homeassistant/sensor/powerhouse_inverter_9_switch_0_power/state",
		},
		ChargeStateTopic:    "homeassistant/sensor/solar_5_charge_state/state",
		BatteryVoltageTopic: "homeassistant/sensor/solar_5_battery_voltage/state",
		CalibrationTopics: CalibrationTopics{
			Inflows:  "homeassistant/sensor/battery_2_state_of_charge/calibration_inflows",
			Outflows: "homeassistant/sensor/battery_2_state_of_charge/calibration_outflows",
		},
		HighVoltageThreshold: 53.6,
		FloatChargeState:     "Float Charging",
		ConversionLossRate:   0.10,
		InverterSwitchIDs: []string{
			"switch.powerhouse_inverter_1_switch_0",
			"switch.powerhouse_inverter_2_switch_0",
			"switch.powerhouse_inverter_3_switch_0",
			"switch.powerhouse_inverter_4_switch_0",
			"switch.powerhouse_inverter_5_switch_0",
			"switch.powerhouse_inverter_6_switch_0",
			"switch.powerhouse_inverter_7_switch_0",
			"switch.powerhouse_inverter_8_switch_0",
			"switch.powerhouse_inverter_9_switch_0",
		},
	}

	battery3 := BatteryConfig{
		Name:         deviceNameBattery3,
		CapacityKWh:  3 * 14.5,
		Manufacturer: "Micromall",
		InflowEnergyTopics: []string{
			"homeassistant/sensor/solar_3_total_energy/state",
			"homeassistant/sensor/solar_4_total_energy/state",
		},
		OutflowEnergyTopics: []string{},
		InflowPowerTopics: []string{
			"homeassistant/sensor/solar_3_solar_power/state",
			"homeassistant/sensor/solar_4_solar_power/state",
		},
		OutflowPowerTopics:  []string{},
		ChargeStateTopic:    "homeassistant/sensor/solar_3_charge_state/state",
		BatteryVoltageTopic: "homeassistant/sensor/solar_3_battery_voltage/state",
		CalibrationTopics: CalibrationTopics{
			Inflows:  "homeassistant/sensor/battery_3_state_of_charge/calibration_inflows",
			Outflows: "homeassistant/sensor/battery_3_state_of_charge/calibration_outflows",
		},
		HighVoltageThreshold: 53.6,
		FloatChargeState:     "Float Charging",
		ConversionLossRate:   0.05,
		InverterSwitchIDs:    []string{},
		CerboSOCTopic:        TopicCerboBatterySOC,
	}

	batteries := []BatteryConfig{battery2, battery3}

	// Build HA statestream topic list from battery configs and power excess calculator
	haTopics := buildTopicsList(batteries)
	haTopics = append(haTopics, PowerExcessTopics()...)

	// Build inverter controller configs and add their topics
	baselineConfig := BuildBaselineInverterConfig(battery2)
	dynamicConfig := BuildDynamicInverterConfig(battery2, battery3)
	haTopics = append(haTopics, baselineConfig.Input.Topics()...)
	haTopics = append(haTopics, dynamicConfig.Input.Topics()...)

	// Add miner workmode topic for dump load enabler
	haTopics = append(haTopics, TopicMinerWorkmode)

	// Add powerctl enabled state topic
	haTopics = append(haTopics, TopicPowerctlEnabledState)

	// Add powerhouse inverters enabled state topic
	haTopics = append(haTopics, TopicPowerhouseInvertersEnabledState)

	// Add PW2 discharge, operation mode, and expecting power cuts state topics
	haTopics = append(haTopics, TopicPW2DischargeMode)
	haTopics = append(haTopics, TopicPW2OperationMode)
	haTopics = append(haTopics, TopicPW2BackupReserve)
	haTopics = append(haTopics, TopicExpectingPowerCutsState)
	haTopics = append(haTopics, TopicHotWaterCylinderState)

	// Add lounge AC, temperature, and sun topics for tile color worker
	haTopics = append(haTopics, TopicLoungeACAction)
	haTopics = append(haTopics, TopicLoungeACState)
	haTopics = append(haTopics, TopicTemperatureInside)
	haTopics = append(haTopics, TopicSunState)

	// Add inverter 10 (Multiplus) setpoint command topic
	haTopics = append(haTopics, TopicInverter10SetpointCmd)

	// Add powerhouse cooling topics
	haTopics = append(haTopics, TopicPowerhouseBlowerTemp)
	haTopics = append(haTopics, TopicPowerhouseBlowerSwitch0State)

	// Add water tank and pump control topics
	haTopics = append(haTopics, TankTopics()...)
	haTopics = append(haTopics, PumpTopics()...)
	haTopics = append(haTopics, LightsTopics()...)
	haTopics = append(haTopics, LoungeACTopics()...)

	// Sort and dedupe HA topics list
	slices.Sort(haTopics)
	haTopics = slices.Compact(haTopics)

	// No separate Victron route needed: HA reads Cerbo N/ topics directly from the broker.

	// Create channels for communication between workers
	msgChan := make(chan SensorMessage, 10)
	statsChan := make(chan DisplayData, 10)
	mqttOutgoingChan := make(chan MQTTMessage, 100)     // Larger buffer for queuing
	inverterOutgoingChan := make(chan MQTTMessage, 100) // For inverter control messages
	mqttClientChan := make(chan mqtt.Client, 1)         // Buffered to prevent blocking onConnect
	senderDataChan := make(chan DisplayData, 10)        // For mqttSenderWorker to receive enabled state

	// Launch the heartbeat writer: flushes the broadcast mark to disk on a timer
	// so the exec liveness probe has something to read.
	SafeGo(ctx, cancel, "heartbeat-writer", func(ctx context.Context) {
		heartbeat.Run(ctx)
	})

	// Launch MQTT sender worker (receives client updates via channel)
	SafeGo(ctx, cancel, "mqtt-sender-worker", func(ctx context.Context) {
		mqttSenderWorker(ctx, mqttOutgoingChan, mqttClientChan, senderDataChan, *forceEnable, *multiplusOnly)
	})
	log.Println("MQTT sender worker started")

	// Create MQTT sender for workers
	mqttSender := NewMQTTSender(mqttOutgoingChan)

	// announce (re)publishes every HA discovery config and restores the durable
	// control state from the TOML state file. It runs on each MQTT (re)connect
	// (after the subscribe loop, so the retain=false state restore loops back to
	// our own subscriptions) and on an HA birth message — covering powerctl,
	// broker, and HA restarts. The one-time clear purges any stale retained state
	// (e.g. car_charging's historical retained OFF) so nothing fights the file.
	var clearRetainedOnce sync.Once
	announce := func() {
		clearRetainedOnce.Do(func() {
			for _, m := range clearRetainedStateMessages() {
				mqttSender.Send(m)
			}
		})
		announceDiscovery(mqttSender, batteries)
		for _, m := range stateRestoreMessages(resolvedValues(LoadStateValues(stateFilePath))) {
			mqttSender.Send(m)
		}
	}

	// Launch sankey config worker (generates and publishes sankey configurations)
	SafeGo(ctx, cancel, "sankey-worker", func(ctx context.Context) {
		log.Println("Generating sankey configurations...")
		configs := sankey.Generate()
		mqttSender.CallService("notify", "send_message", "notify.sankey_config", map[string]any{
			"message": configs.SankeyConfig,
		})
		mqttSender.CallService("notify", "send_message", "notify.sankey_templates", map[string]any{
			"message": configs.Templates,
		})
		mqttSender.CallService("homeassistant", "reload_all", "", nil)
		log.Println("Sankey configurations published")
	})

	// Launch stats worker (produces statistics)
	SafeGo(ctx, cancel, "stats-worker", func(ctx context.Context) {
		statsWorker(ctx, msgChan, statsChan, haTopics)
	})
	log.Println("Stats worker started")

	// Seed the durable control state from the TOML state file (fallbacks apply on
	// first run), then the ephemeral pre-seeds (see preSeededTopics in stats.go),
	// so statsWorker doesn't block on first startup. The on-connect restore
	// republishes these anyway; this just populates our view immediately.
	// Sent after the stats worker launch so the seeds can't outgrow the channel buffer.
	for _, msg := range LoadStateSeeds(stateFilePath) {
		msgChan <- msg
	}
	for _, msg := range preSeededTopics {
		msgChan <- msg
	}

	// Launch battery workers and collect downstream channels.
	var downstreamChans []chan<- DisplayData
	for _, b := range batteries {
		calibChan := make(chan DisplayData, 10)
		socChan := make(chan DisplayData, 10)
		downstreamChans = append(downstreamChans, calibChan, socChan)

		// Launch calibration worker
		calibConfig := b.CalibConfig()
		SafeGo(ctx, cancel, b.Name+"-calib", func(ctx context.Context) {
			batteryCalibWorker(ctx, calibChan, calibConfig, mqttSender)
		})
		log.Printf("%s calibration worker started\n", b.Name)

		// Launch SOC or available-energy worker depending on SOC source
		if b.CerboSOCTopic != "" {
			availConfig := b.AvailableEnergyFromSOCConfig()
			SafeGo(ctx, cancel, b.Name+"-available-energy", func(ctx context.Context) {
				batteryAvailableEnergyFromSOCWorker(ctx, socChan, availConfig, mqttSender)
			})
			log.Printf("%s available energy worker started\n", b.Name)
		} else {
			socConfig := b.SOCConfig()
			SafeGo(ctx, cancel, b.Name+"-soc", func(ctx context.Context) {
				batterySOCWorker(ctx, socChan, socConfig, mqttSender)
			})
			log.Printf("%s SOC worker started\n", b.Name)
		}
	}

	// Launch power excess calculator and dump load enabler
	powerExcessChan := make(chan DisplayData, 10)
	excessValueChan := make(chan float64, 10)
	dumpLoadDataChan := make(chan DisplayData, 10)
	downstreamChans = append(downstreamChans, powerExcessChan, dumpLoadDataChan)

	SafeGo(ctx, cancel, "power-excess-calculator", func(ctx context.Context) {
		powerExcessCalculator(ctx, powerExcessChan, excessValueChan)
	})

	SafeGo(ctx, cancel, "dump-load-enabler", func(ctx context.Context) {
		dumpLoadEnabler(ctx, excessValueChan, dumpLoadDataChan, mqttSender)
	})

	// Create inverterSender that sends to inverterOutgoingChan (filtered by interceptor)
	inverterSender := NewMQTTSender(inverterOutgoingChan)

	// Launch interceptor to filter inverter messages based on powerctl_inverter_enabled switch
	interceptorDataChan := make(chan DisplayData, 10)
	downstreamChans = append(downstreamChans, interceptorDataChan)

	SafeGo(ctx, cancel, "inverter-interceptor", func(ctx context.Context) {
		mqttInterceptorWorker(
			ctx,
			"Powerhouse inverters",
			TopicPowerhouseInvertersEnabledState,
			inverterOutgoingChan,
			mqttOutgoingChan,
			interceptorDataChan,
			*forceEnable,
		)
	})

	// Launch baseline inverter controller (Battery 2 inverters)
	baselineDisplayChan := make(chan DisplayData, 10)
	baselineInputChan := make(chan BaselineInput, 10)
	baselineDebugChan := make(chan BaselineDebugInfo, 10)
	downstreamChans = append(downstreamChans, baselineDisplayChan)

	SafeGo(ctx, cancel, "baseline-input-bridge", func(ctx context.Context) {
		for {
			select {
			case data := <-baselineDisplayChan:
				select {
				case baselineInputChan <- ExtractBaselineInput(data, baselineConfig.Input):
				default:
				}
			case <-ctx.Done():
				return
			}
		}
	})

	SafeGo(ctx, cancel, "baseline-inverter-control", func(ctx context.Context) {
		baselineInverterControl(ctx, baselineInputChan, baselineConfig, inverterSender, baselineDebugChan)
	})

	// Launch dynamic inverter controller (Multiplus II, Battery 3)
	dynamicDisplayChan := make(chan DisplayData, 10)
	dynamicInputChan := make(chan DynamicInput, 10)
	dynamicDebugChan := make(chan DynamicDebugInfo, 10)
	downstreamChans = append(downstreamChans, dynamicDisplayChan)

	SafeGo(ctx, cancel, "dynamic-input-bridge", func(ctx context.Context) {
		for {
			select {
			case data := <-dynamicDisplayChan:
				select {
				case dynamicInputChan <- ExtractDynamicInput(data, dynamicConfig.Input):
				default:
				}
			case <-ctx.Done():
				return
			}
		}
	})

	SafeGo(ctx, cancel, "dynamic-inverter-control", func(ctx context.Context) {
		dynamicInverterControl(ctx, dynamicInputChan, mqttSender, dynamicDebugChan)
	})

	// Launch debug aggregator (combines baseline + dynamic debug info for HA display)
	SafeGo(ctx, cancel, "debug-aggregator", func(ctx context.Context) {
		debugAggregatorWorker(ctx, baselineDebugChan, dynamicDebugChan, mqttSender)
	})

	// Vote channel carries discharge requests from automation sources into the arbiter.
	dischargeVoteChan := make(chan DischargeRequest, 10)

	// Launch Powerwall 2 discharge arbiter
	pw2DischargeChan := make(chan DisplayData, 10)
	downstreamChans = append(downstreamChans, pw2DischargeChan)

	SafeGo(ctx, cancel, "discharge-arbiter", func(ctx context.Context) {
		dischargeArbiter(ctx, pw2DischargeChan, dischargeVoteChan, mqttSender)
	})

	// Launch expecting power cuts worker
	expectingPowerCutsChan := make(chan DisplayData, 10)
	downstreamChans = append(downstreamChans, expectingPowerCutsChan)

	SafeGo(ctx, cancel, "expecting-power-cuts", func(ctx context.Context) {
		expectingPowerCutsWorker(ctx, expectingPowerCutsChan, dischargeVoteChan, mqttSender)
	})

	// Launch AC tile color worker
	acTileChan := make(chan DisplayData, 10)
	downstreamChans = append(downstreamChans, acTileChan)

	SafeGo(ctx, cancel, "ac-tile-worker", func(ctx context.Context) {
		acTileWorker(ctx, acTileChan, mqttSender)
	})

	// Launch powerhouse cooling worker
	coolingChan := make(chan DisplayData, 10)
	downstreamChans = append(downstreamChans, coolingChan)

	SafeGo(ctx, cancel, "powerhouse-cooling-worker", func(ctx context.Context) {
		powerhouseCoolingWorker(ctx, coolingChan, mqttSender)
	})

	// Launch tank levels worker (computes water tank fill percentages)
	tankLevelsChan := make(chan DisplayData, 10)
	downstreamChans = append(downstreamChans, tankLevelsChan)

	SafeGo(ctx, cancel, "tank-levels-worker", func(ctx context.Context) {
		tankLevelsWorker(ctx, tankLevelsChan, mqttSender)
	})

	// Launch pump control worker (daily start check, low-level floor, full stop)
	pumpControlChan := make(chan DisplayData, 10)
	downstreamChans = append(downstreamChans, pumpControlChan)

	SafeGo(ctx, cancel, "pump-control-worker", func(ctx context.Context) {
		pumpControlWorker(ctx, pumpControlChan, mqttSender)
	})

	// Launch health worker: marks the liveness heartbeat on each broadcast, which is
	// the one event that happens only while powerctl is actually running.
	healthChan := make(chan DisplayData, 10)
	downstreamChans = append(downstreamChans, healthChan)

	SafeGo(ctx, cancel, "health-worker", func(ctx context.Context) {
		healthWorker(ctx, healthChan, heartbeat)
	})

	// Launch lounge AC worker (IR re-send, WiFi-confirmed, WiFi fallback)
	loungeACChan := make(chan DisplayData, 10)
	downstreamChans = append(downstreamChans, loungeACChan)

	SafeGo(ctx, cancel, "lounge-ac-worker", func(ctx context.Context) {
		loungeACWorker(ctx, loungeACChan, mqttSender)
	})

	// Launch lights worker (outside auto-off, kitchen↔mum, garage mirror, sleep dim).
	// sleepRyanChan delivers button presses on a dedicated route so momentary
	// presses aren't collapsed by statsWorker's per-topic state.
	lightsChan := make(chan DisplayData, 10)
	sleepRyanChan := make(chan SensorMessage, 10)
	downstreamChans = append(downstreamChans, lightsChan)

	SafeGo(ctx, cancel, "lights-worker", func(ctx context.Context) {
		lightsWorker(ctx, lightsChan, sleepRyanChan, mqttSender)
	})

	// Launch Cerbo keepalive worker (outbound only)
	SafeGo(ctx, cancel, "cerbo-keepalive", func(ctx context.Context) {
		cerboKeepaliveWorker(ctx, mqttSender)
	})

	// Launch state store worker (persists durable control state to state.toml
	// and republishes it to keep HA in sync).
	stateStoreChan := make(chan DisplayData, 10)
	downstreamChans = append(downstreamChans, stateStoreChan)
	SafeGo(ctx, cancel, "state-store-worker", func(ctx context.Context) {
		stateStoreWorker(ctx, stateStoreChan, stateFilePath, mqttSender)
	})

	// Watch the HA birth message: when HA (re)connects it publishes "online",
	// which re-announces discovery + restores state so an HA-only restart doesn't
	// leave the powerctl entities reset. Runs on its own MQTT route so it can't
	// gate statsWorker (the birth message is not retained and may never arrive).
	haStatusChan := make(chan SensorMessage, 10)
	SafeGo(ctx, cancel, "ha-birth-worker", func(ctx context.Context) {
		for {
			select {
			case msg := <-haStatusChan:
				if strings.EqualFold(msg.Value, "online") {
					log.Println("HA birth message received: re-announcing entities and restoring state")
					announce()
				}
			case <-ctx.Done():
				return
			}
		}
	})

	// Add senderDataChan to downstream channels for mqttSenderWorker to receive enabled state
	downstreamChans = append(downstreamChans, senderDataChan)

	// Launch debug worker if enabled
	if *debugMode {
		debugChan := make(chan DisplayData, 10)
		downstreamChans = append(downstreamChans, debugChan)
		SafeGo(ctx, cancel, "debug-worker", func(ctx context.Context) {
			debugWorker(ctx, cancel, debugChan)
		})
	}

	// Launch broadcast worker (fans out to all downstream workers)
	SafeGo(ctx, cancel, "broadcast-worker", func(ctx context.Context) {
		broadcastWorker(ctx, statsChan, downstreamChans)
	})
	log.Println("Broadcast worker started")

	// Launch MQTT worker. onConnect runs after the subscribe loop on every
	// (re)connect: re-announce discovery and restore durable state (retain=false),
	// which then loops back through our own subscriptions.
	SafeGo(ctx, cancel, "mqtt-worker", func(ctx context.Context) {
		mqttWorker(ctx, mqttHost, mqttPort, []TopicRoute{
			{Topics: haTopics, Channel: msgChan},
			{Topics: []string{TopicSleepRyanPress}, Channel: sleepRyanChan},
			{Topics: []string{TopicHAStatus}, Channel: haStatusChan},
		}, mqttUsername, mqttPassword, mqttClientID, mqttClientChan, announce)
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
	cancel()
}
