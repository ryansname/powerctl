package main

import (
	"context"
	"log"
)

// TopicLoungeACAction is the hvac_action attribute topic for the lounge AC.
const TopicLoungeACAction = "homeassistant/climate/lounge/hvac_action"

// TopicLoungeACState is the state topic for the lounge AC.
const TopicLoungeACState = "homeassistant/climate/lounge/state"

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

// acActionColors maps hvac_action values to tile colors.
var acActionColors = map[string]hsColor{
	"cooling": {200, 80},
	"heating": {0, 85},
}

// acStateColors maps climate state to tile colors for modes
// where the Daikin module doesn't report hvac_action.
var acStateColors = map[string]hsColor{
	"fan_only": {120, 70},
	"dry":      {200, 80},
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

	lastAction := ""

	lookupColor := func(action string) (hsColor, bool) {
		if c, ok := acActionColors[action]; ok {
			return c, true
		}
		if c, ok := acStateColors[action]; ok {
			return c, true
		}
		return hsColor{}, false
	}

	for {
		select {
		case data := <-dataChan:
			state := data.GetString(TopicLoungeACState)
			hvacAction := data.GetString(TopicLoungeACAction)
			action := resolveACTileAction(state, hvacAction)

			if action == "" || action == lastAction {
				continue
			}
			lastAction = action

			if color, ok := lookupColor(action); ok {
				log.Printf("AC tile: action=%s, setting tiles to hs(%.0f, %.0f)\n", action, color.Hue, color.Saturation)
				sender.CallService("light", "turn_on", "light.tiles", map[string]any{
					"hs_color":       []float64{color.Hue, color.Saturation},
					"brightness_pct": 75,
				})
			} else {
				log.Printf("AC tile: action=%s, turning tiles off\n", action)
				sender.CallService("light", "turn_off", "light.tiles", nil)
			}

		case <-ctx.Done():
			log.Println("AC tile worker stopped")
			return
		}
	}
}
