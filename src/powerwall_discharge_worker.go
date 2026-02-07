package main

import (
	"context"
	"fmt"
	"log"
	"time"
)

// TopicPW2DischargeState is the state topic for the powerctl_pw2_discharge switch.
const TopicPW2DischargeState = "homeassistant/switch/powerctl_pw2_discharge/state"

const pw2SiteID = "2233628"

// powerwallDischargeWorker monitors the PW2 discharge switch and controls
// Tesla Powerwall 2 discharge via TOU tariff manipulation.
func powerwallDischargeWorker(
	ctx context.Context,
	dataChan <-chan DisplayData,
	sender *MQTTSender,
) {
	log.Println("Powerwall discharge worker started")

	discharging := false
	var lastTOURefresh time.Time

	for {
		select {
		case data := <-dataChan:
			enabled := data.GetBoolean(TopicPW2DischargeState)

			switch {
			case enabled && !discharging:
				log.Println("PW2 discharge: activating")
				startDischarge(sender)
				discharging = true
				lastTOURefresh = time.Now()
			case !enabled && discharging:
				log.Println("PW2 discharge: deactivating")
				stopDischarge(sender)
				discharging = false
			case discharging && time.Since(lastTOURefresh) >= time.Hour:
				log.Println("PW2 discharge: refreshing TOU tariff")
				sendTOUTariff(sender)
				lastTOURefresh = time.Now()
			}

		case <-ctx.Done():
			if discharging {
				log.Println("PW2 discharge: restoring normal mode on shutdown")
				stopDischarge(sender)
			}
			log.Println("Powerwall discharge worker stopped")
			return
		}
	}
}

// startDischarge pushes a TOU tariff and sets autonomous mode with battery export.
func startDischarge(sender *MQTTSender) {
	sendTOUTariff(sender)
	sendTeslaAPI(sender, "OPERATION_MODE", map[string]any{
		"default_real_mode": "autonomous",
	})
	sendTeslaAPI(sender, "ENERGY_SITE_IMPORT_EXPORT_CONFIG", map[string]any{
		"customer_preferred_export_rule": "battery_ok",
	})
}

// stopDischarge restores self-consumption mode with no battery export.
func stopDischarge(sender *MQTTSender) {
	sendTeslaAPI(sender, "OPERATION_MODE", map[string]any{
		"default_real_mode": "self_consumption",
	})
	sendTeslaAPI(sender, "ENERGY_SITE_IMPORT_EXPORT_CONFIG", map[string]any{
		"customer_preferred_export_rule": "never",
	})
}

// sendTeslaAPI sends a tesla_custom.api service call via the Node-RED proxy.
// Body fields are merged into parameters alongside path_vars, since the
// tesla_custom service pops path_vars and passes the rest as kwargs.
func sendTeslaAPI(sender *MQTTSender, command string, body map[string]any) {
	params := map[string]any{
		"path_vars": map[string]any{
			"site_id": pw2SiteID,
		},
	}
	for k, v := range body {
		params[k] = v
	}
	sender.CallService("tesla_custom", "api", "", map[string]any{
		"command":    command,
		"parameters": params,
	})
}

// sendTOUTariff generates and sends a TOU tariff with ON_PEAK now and SUPER_OFF_PEAK later.
func sendTOUTariff(sender *MQTTSender) {
	tariff := buildTOUTariff(time.Now())
	sendTeslaAPI(sender, "TIME_OF_USE_SETTINGS", map[string]any{
		"tou_settings": map[string]any{
			"tariff_content_v2": tariff,
		},
	})
}

// buildTOUTariff creates a tariff_content_v2 structure with ON_PEAK for 3 hours
// from the current hour and SUPER_OFF_PEAK for the remaining hours.
// Wrapping (toHour < fromHour) is valid and covers the full 24 hours.
func buildTOUTariff(now time.Time) map[string]any {
	onPeakStart := now.Hour()
	onPeakEnd := (now.Hour() + 3) % 24

	touPeriods := map[string]any{
		"ON_PEAK": map[string]any{
			"periods": []any{
				map[string]any{
					"fromDayOfWeek": 0,
					"toDayOfWeek":   6,
					"fromHour":      onPeakStart,
					"fromMinute":    0,
					"toHour":        onPeakEnd,
					"toMinute":      0,
				},
			},
		},
		"SUPER_OFF_PEAK": map[string]any{
			"periods": []any{
				map[string]any{
					"fromDayOfWeek": 0,
					"toDayOfWeek":   6,
					"fromHour":      onPeakEnd,
					"fromMinute":    0,
					"toHour":        onPeakStart,
					"toMinute":      0,
				},
			},
		},
	}

	season := map[string]any{
		"fromMonth":  1,
		"fromDay":    1,
		"toMonth":    12,
		"toDay":      31,
		"tou_periods": touPeriods,
	}

	dailyCharges := []any{
		map[string]any{"name": "Charge", "amount": 0},
	}

	demandCharges := map[string]any{
		"ALL":     map[string]any{"rates": map[string]any{"ALL": 0}},
		"AllYear": map[string]any{"rates": map[string]any{}},
	}

	buyRates := map[string]any{
		"ALL": map[string]any{"rates": map[string]any{"ALL": 0}},
		"AllYear": map[string]any{"rates": map[string]any{
			"ON_PEAK":        0.31,
			"SUPER_OFF_PEAK": 0.07,
		}},
	}

	sellRates := map[string]any{
		"ALL": map[string]any{"rates": map[string]any{"ALL": 0}},
		"AllYear": map[string]any{"rates": map[string]any{
			"ON_PEAK":        0.30,
			"SUPER_OFF_PEAK": 0.07,
		}},
	}

	return map[string]any{
		"version":                1,
		"utility":               "Custom",
		"code":                  "CUSTOM-EXPORT",
		"name":                  fmt.Sprintf("Powerctl Discharge (%s)", now.Format("15:04")),
		"currency":              "USD",
		"monthly_minimum_bill":  0,
		"min_applicable_demand": 0,
		"max_applicable_demand": 0,
		"monthly_charges":       0,
		"daily_charges":         dailyCharges,
		"daily_demand_charges":  map[string]any{},
		"demand_charges":        demandCharges,
		"energy_charges":        buyRates,
		"seasons": map[string]any{
			"AllYear": season,
		},
		"sell_tariff": map[string]any{
			"utility":               "Custom",
			"monthly_minimum_bill":  0,
			"min_applicable_demand": 0,
			"max_applicable_demand": 0,
			"monthly_charges":       0,
			"daily_charges":         dailyCharges,
			"demand_charges":        demandCharges,
			"energy_charges":        sellRates,
			"seasons": map[string]any{
				"AllYear": season,
			},
		},
	}
}
