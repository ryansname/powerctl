package main

import (
	"context"
	"log"
	"time"
)

// TopicLoungeACAction is the hvac_action attribute topic for the lounge AC.
const TopicLoungeACAction = "homeassistant/climate/lounge/hvac_action"

// TopicLoungeACState is the state topic for the lounge AC.
const TopicLoungeACState = "homeassistant/climate/lounge/state"

// TopicTemperatureInside is the indoor temperature sensor.
const TopicTemperatureInside = "homeassistant/sensor/temperature_inside_temperature/state"

// TopicSunState is the sun entity state (above_horizon / below_horizon).
const TopicSunState = "homeassistant/sun/sun/state"

type hsColor struct {
	Hue        float64
	Saturation float64
}

// States where the Daikin module reports hvac_action.
// For these, we use hvac_action to distinguish active vs idle.
var acHvacActionStates = map[string]bool{
	"cool":      true,
	"heat":      true,
	"heat_cool": true,
}

// acActionColors maps hvac_action values to tile colors (active, 75% brightness).
var acActionColors = map[string]hsColor{
	"cooling": {200, 80},
	"heating": {0, 85},
}

// acStateIdleColors maps climate state to tile colors when hvac_action is "idle".
// Uses same color as active but at 25% brightness to indicate standby.
var acStateIdleColors = map[string]hsColor{
	"cool":      {200, 80},
	"heat":      {0, 85},
	"heat_cool": {0, 85},
}

// acStateColors maps climate state to tile colors for modes
// where the Daikin module doesn't report hvac_action.
var acStateColors = map[string]hsColor{
	"fan_only": {120, 70},
	"dry":      {200, 80},
}

// temperatureToHue maps temperature (18-23C) to hue (240 blue → 0 red).
func temperatureToHue(temp float64) float64 {
	clamped := max(18.0, min(temp, 23.0))
	return (23 - clamped) / 5 * 240
}

// isTileActiveTime returns true between 7am and 10pm.
func isTileActiveTime() bool {
	h := time.Now().Hour()
	return h >= 7 && h < 22
}

// resolveACTileAction determines the effective action for tile color.
// Checks state first: for states that report hvac_action (cool/heat),
// uses that to distinguish active vs idle. For other states (fan_only, dry),
// uses the state directly since hvac_action is absent/stale.
func resolveACTileAction(state, hvacAction string) string {
	if acHvacActionStates[state] {
		return hvacAction
	}
	return state
}

// acTileWorker watches the lounge AC's state and hvac_action, then sets the
// tile light color to match what the unit is actively doing.
func acTileWorker(
	ctx context.Context,
	dataChan <-chan DisplayData,
	sender *MQTTSender,
) {
	log.Println("AC tile worker started")

	lastState := ""
	lastAction := ""
	lastTemp := 0.0
	lastActiveTime := true
	lastSunBelow := false

	for {
		select {
		case data := <-dataChan:
			state := data.GetString(TopicLoungeACState)
			hvacAction := data.GetString(TopicLoungeACAction)
			action := resolveACTileAction(state, hvacAction)
			temp := data.GetFloat(TopicTemperatureInside).Current
			activeTime := isTileActiveTime()
			sunBelow := data.GetString(TopicSunState) == "below_horizon"

			if action == "" {
				continue
			}

			// Detect changes to avoid redundant sends.
			tempChanged := temp != lastTemp
			timeChanged := activeTime != lastActiveTime
			sunChanged := sunBelow != lastSunBelow
			acChanged := action != lastAction || state != lastState
			if !acChanged && !tempChanged && !timeChanged && !sunChanged {
				continue
			}
			lastAction = action
			lastState = state
			lastTemp = temp
			lastActiveTime = activeTime
			lastSunBelow = sunBelow

			// Halve brightness after sunset.
			dimBrightness := func(pct int) int {
				if sunBelow {
					return pct / 2
				}
				return pct
			}

			if !activeTime {
				if acChanged || timeChanged {
					log.Printf("AC tile: outside active hours, turning tiles off\n")
					sender.CallService("light", "turn_off", "light.tiles", nil)
				}
				continue
			}

			stateChanged := acChanged || timeChanged || sunChanged

			if color, ok := acActionColors[action]; ok {
				if stateChanged {
					brightness := dimBrightness(75)
					log.Printf("AC tile: state=%s action=%s, setting tiles to hs(%.0f, %.0f) %d%%\n", state, action, color.Hue, color.Saturation, brightness)
					sender.CallService("light", "turn_on", "light.tiles", map[string]any{
						"hs_color":       []float64{color.Hue, color.Saturation},
						"brightness_pct": brightness,
					})
				}
			} else if action == "idle" {
				if color, ok := acStateIdleColors[state]; ok {
					if stateChanged {
						brightness := dimBrightness(25)
						log.Printf("AC tile: state=%s action=idle, setting tiles to hs(%.0f, %.0f) %d%%\n", state, color.Hue, color.Saturation, brightness)
						sender.CallService("light", "turn_on", "light.tiles", map[string]any{
							"hs_color":       []float64{color.Hue, color.Saturation},
							"brightness_pct": brightness,
						})
					}
				}
			} else if color, ok := acStateColors[action]; ok {
				if stateChanged {
					brightness := dimBrightness(75)
					log.Printf("AC tile: state=%s, setting tiles to hs(%.0f, %.0f) %d%%\n", state, color.Hue, color.Saturation, brightness)
					sender.CallService("light", "turn_on", "light.tiles", map[string]any{
						"hs_color":       []float64{color.Hue, color.Saturation},
						"brightness_pct": brightness,
					})
				}
			} else {
				hue := temperatureToHue(temp)
				brightness := dimBrightness(50)
				log.Printf("AC tile: AC off, temp=%.1f°C, setting tiles to hs(%.0f, 80) %d%%\n", temp, hue, brightness)
				sender.CallService("light", "turn_on", "light.tiles", map[string]any{
					"hs_color":       []float64{hue, 80},
					"brightness_pct": brightness,
				})
			}

		case <-ctx.Done():
			log.Println("AC tile worker stopped")
			return
		}
	}
}
