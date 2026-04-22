package main

import (
	"context"
	"log"

	"github.com/ryansname/powerctl/src/governor"
)

// BaselineInverterConfig holds configuration for the baseline inverter controller.
type BaselineInverterConfig struct {
	Input    BaselineInputConfig
	Battery2 BatteryInverterGroup

	WattsPerInverter float64
	MaxTransferPower float64
	MaxBaselineWatts float64

	OverflowSOCTurnOffStart  float64
	OverflowSOCTurnOffEnd    float64
	OverflowSOCTurnOnStart   float64
	OverflowSOCTurnOnEnd     float64
	LowVoltageTripThreshold  float64
	LowVoltageResetThreshold float64
}

// BaselineInverterState holds runtime state for the baseline inverter controller.
type BaselineInverterState struct {
	overflow2      BatteryOverflowState
	forecastExcess governor.ForecastExcessState

	gridOffSolarMax    governor.RollingMinMax
	battery2VoltageMin governor.RollingMinMax
	houseLoadHourly    governor.RollingMinMax // 168-hour (7-day) window, hourly buckets
	targetMinusSolar   governor.RollingMinMax // 60-minute window, 1-minute buckets

	socLimit2      *governor.SteppedHysteresis
	powerCutAllow2 *governor.SteppedHysteresis
	lowVoltage2    *governor.SteppedHysteresis

}

// BaselineDebugInfo contains mode states for the baseline controller debug output.
type BaselineDebugInfo struct {
	Modes         []ModeState
	SafetyReason  string
	ACFreqCurrent float64
	ACFreqP100    float64
	PowerwallSOC  float64

	Battery2LowVoltage bool
	Battery2VoltageMin float64

	BaselineTarget float64
	BaselineUsed   float64
}

// calculateBaseline returns the baseline power request from the 7-day house load floor.
// Updates the rolling windows in state as a side effect.
func calculateBaseline(
	houseLoad float64,
	solar1 float64,
	solar2 float64,
	maxWatts float64,
	state *BaselineInverterState,
) PowerRequest {
	state.houseLoadHourly.Update(houseLoad)
	baselineTarget := state.houseLoadHourly.BucketMinPercentile(2)

	targetMinusSolar := baselineTarget - solar1 - solar2
	state.targetMinusSolar.Update(targetMinusSolar)
	usedBaseline := max(0.0, state.targetMinusSolar.Min())

	return PowerRequest{
		Name:  "Baseline",
		Watts: min(usedBaseline, maxWatts),
	}
}

// selectBaselineMode computes the desired inverter count and debug info from a BaselineInput.
func selectBaselineMode(
	input BaselineInput,
	config BaselineInverterConfig,
	state *BaselineInverterState,
) (int, BaselineDebugInfo) {
	if input.ACFreqP100_5Min > 52.75 {
		return 0, BaselineDebugInfo{
			SafetyReason:  "High frequency",
			ACFreqCurrent: input.ACFrequency,
			ACFreqP100:    input.ACFreqP100_5Min,
			PowerwallSOC:  input.PowerwallSOC,
		}
	}

	if !input.GridAvailable && input.PowerwallSOC > 90.0 {
		return 0, BaselineDebugInfo{
			SafetyReason:  "Grid off + high Powerwall",
			ACFreqCurrent: input.ACFrequency,
			ACFreqP100:    input.ACFreqP100_5Min,
			PowerwallSOC:  input.PowerwallSOC,
		}
	}

	overflow2 := checkBatteryOverflow(
		input.Battery2ChargeState,
		input.Battery2SOC,
		config.WattsPerInverter,
		&state.overflow2,
	)
	forecastExcess2 := forecastExcessRequest(
		input.ForecastRemainingWh,
		input.DetailedForecast,
		input.Battery2EnergyWh,
		config.WattsPerInverter,
		config.Battery2,
		&state.forecastExcess,
	)

	// Grid off: disable per-battery modes when solar is consistently high (≥3kW over 1h)
	if !input.GridAvailable {
		state.gridOffSolarMax.Update(input.Solar1Power + input.Solar2Power)
		if state.gridOffSolarMax.Max() >= 3000 {
			overflow2.Watts = 0
			forecastExcess2.Watts = 0
		}
	}

	perBattery := maxPowerRequest(overflow2, forecastExcess2)
	baseline := calculateBaseline(input.HouseLoad, input.Solar1Power, input.Solar2Power, config.MaxBaselineWatts, state)
	baselineTarget := state.houseLoadHourly.BucketMinPercentile(2)

	selected := maxPowerRequest(perBattery, baseline)
	selectedCount := calculateInverterCount(selected.Watts, config.WattsPerInverter)

	// SOC-based limit
	maxB2 := maxInvertersForSOC(input.Battery2SOC, state.socLimit2)
	selectedCount = min(selectedCount, maxB2)

	// Powerhouse transfer limit — skipped when Battery 3 SOC < 85% so the Multiplus can absorb
	if input.Battery3SOC >= 85.0 {
		limit := powerhouseTransferLimit(input.Solar1P90_15Min, config.MaxTransferPower)
		limitCount := int(limit.Watts / config.WattsPerInverter)
		if limitCount < 0 {
			limitCount = 0
		}
		selectedCount = min(selectedCount, limitCount)
	}

	overflowContrib := selectedCount > 0 && selected.Name == overflow2.Name
	forecastContrib := selectedCount > 0 && selected.Name == forecastExcess2.Name
	baselineContrib := selectedCount > 0 && selected.Name == baseline.Name

	debug := BaselineDebugInfo{
		PowerwallSOC:  input.PowerwallSOC,
		ACFreqCurrent: input.ACFrequency,
		ACFreqP100:    input.ACFreqP100_5Min,
		Modes: []ModeState{
			{Name: overflow2.Name, Watts: overflow2.Watts, Contributing: overflowContrib},
			{Name: forecastExcess2.Name, Watts: forecastExcess2.Watts, Contributing: forecastContrib},
			{Name: baseline.Name, Watts: baseline.Watts, Contributing: baselineContrib},
		},
		BaselineTarget: baselineTarget,
		BaselineUsed:   baseline.Watts,
	}

	return selectedCount, debug
}

// baselineInverterControl manages Battery 2 inverters using baseline + overflow/forecast strategy.
func baselineInverterControl(
	ctx context.Context,
	inputChan <-chan BaselineInput,
	config BaselineInverterConfig,
	sender *MQTTSender,
	debugChan chan<- BaselineDebugInfo,
) {
	log.Println("Baseline inverter control started")

	b2Count := len(config.Battery2.Inverters)

	state := &BaselineInverterState{
		overflow2: BatteryOverflowState{
			Hysteresis: governor.NewSteppedHysteresis(
				b2Count, true,
				config.OverflowSOCTurnOnStart, config.OverflowSOCTurnOnEnd,
				config.OverflowSOCTurnOffStart, config.OverflowSOCTurnOffEnd,
			),
		},
		gridOffSolarMax:    governor.NewRollingMinMax(60),
		battery2VoltageMin: governor.NewRollingMinMax(15),
		houseLoadHourly:    governor.NewRollingMinMaxHours(168),
		targetMinusSolar:   governor.NewRollingMinMax(60),
		socLimit2:          governor.NewSteppedHysteresis(b2Count, true, 15, 25, 12.5, 22.5),
		powerCutAllow2:     governor.NewSteppedHysteresis(1, true, 53, 53, 47, 47),
		lowVoltage2: governor.NewSteppedHysteresis(
			1, false,
			config.LowVoltageTripThreshold, config.LowVoltageTripThreshold,
			config.LowVoltageResetThreshold, config.LowVoltageResetThreshold,
		),
	}
	state.socLimit2.Current = b2Count

	for {
		select {
		case input := <-inputChan:
			desiredCount, debugInfo := selectBaselineMode(input, config, state)

			// Low voltage latch using 15-minute rolling minimum
			state.battery2VoltageMin.Update(input.Battery2Voltage)
			b2VoltMin := state.battery2VoltageMin.Min()
			prevLatched := state.lowVoltage2.Current > 0
			b2LowVoltage := state.lowVoltage2.Update(b2VoltMin) > 0
			if b2LowVoltage != prevLatched {
				if b2LowVoltage {
					log.Printf("Battery 2: LOW VOLTAGE TRIP (15m min %.2fV < %.2fV) - forcing inverters off\n",
						b2VoltMin, config.LowVoltageTripThreshold)
				} else {
					log.Printf("Battery 2: low voltage cleared (15m min %.2fV >= %.2fV)\n",
						b2VoltMin, config.LowVoltageResetThreshold)
				}
			}
			if b2LowVoltage {
				desiredCount = 0
			}
			debugInfo.Battery2LowVoltage = b2LowVoltage
			debugInfo.Battery2VoltageMin = b2VoltMin

			// Expecting power cuts: conserve around 50% SOC, grid-on only
			if input.ExpectingPowerCuts && input.GridAvailable {
				blocked := state.powerCutAllow2.Update(input.Battery2SOC) == 0
				if blocked {
					desiredCount = 0
					if debugInfo.SafetyReason == "" {
						debugInfo.SafetyReason = "Expecting power cuts (battery < 50%)"
					}
				}
			}

			if debugChan != nil {
				select {
				case debugChan <- debugInfo:
				default:
				}
			}

			changed := applyInverterChanges(input.InverterStates, config.Battery2.Inverters, sender, desiredCount)
			if changed {
				log.Printf("Baseline inverter control: B2=%d (%.0fW)\n",
					desiredCount, float64(desiredCount)*config.WattsPerInverter)
			}

		case <-ctx.Done():
			log.Println("Baseline inverter control stopped")
			return
		}
	}
}
