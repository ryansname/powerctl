package main

import (
	"context"
	"fmt"
	"log"
	"time"
)

// TopicPW2DischargeState is the state topic for the powerctl_pw2_discharge switch.
const TopicPW2DischargeState = "homeassistant/switch/powerctl_pw2_discharge/state"

// TopicPW2OperationMode is the state topic for the Powerwall 2 operation mode select entity.
const TopicPW2OperationMode = "homeassistant/select/home_sweet_home_operation_mode/state"

// TopicPW2BackupReserve is the state topic for the Powerwall 2 backup reserve number entity.
const TopicPW2BackupReserve = "homeassistant/number/home_sweet_home_backup_reserve/state"

const pw2SiteID = "2233628"
const pw2OperationModeEntity = "select.home_sweet_home_operation_mode"
const pw2BackupReserveEntity = "number.home_sweet_home_backup_reserve"

// powerwallDischargeWorker monitors the PW2 discharge switch and controls
// Tesla Powerwall 2 discharge via TOU tariff manipulation.
func powerwallDischargeWorker(
	ctx context.Context,
	dataChan <-chan DisplayData,
	sender *MQTTSender,
) {
	log.Println("Powerwall discharge worker started")

	var lastCommandSent time.Time
	var lastTOURefresh time.Time
	commandCooldown := 60 * time.Second

	for {
		select {
		case data := <-dataChan:
			switchEnabled := data.GetBoolean(TopicPW2DischargeState)
			currentMode := data.GetString(TopicPW2OperationMode)
			backupReserve := data.GetFloat(TopicPW2BackupReserve).Current

			// Cooldown after sending commands (wait for mode to update)
			if time.Since(lastCommandSent) < commandCooldown {
				continue
			}

			actuallyDischarging := currentMode == "Time-Based Control"

			switch {
			case switchEnabled && !actuallyDischarging:
				log.Println("PW2 discharge: activating")
				startDischarge(sender, backupReserve)
				requestModeUpdate(sender)
				lastCommandSent = time.Now()
				lastTOURefresh = time.Now()
			case !switchEnabled && actuallyDischarging:
				log.Println("PW2 discharge: deactivating")
				stopDischarge(sender)
				requestModeUpdate(sender)
				lastCommandSent = time.Now()
			case switchEnabled && actuallyDischarging && time.Since(lastTOURefresh) >= time.Hour:
				log.Println("PW2 discharge: refreshing discharge state")
				startDischarge(sender, backupReserve)
				lastTOURefresh = time.Now()
			}

		case <-ctx.Done():
			log.Println("Powerwall discharge worker stopped")
			return
		}
	}
}

// requestModeUpdate triggers a HA entity update to speed up feedback after commands.
func requestModeUpdate(sender *MQTTSender) {
	sender.CallService("homeassistant", "update_entity", pw2OperationModeEntity, nil)
}

// startDischarge pushes a TOU tariff and sets autonomous mode with battery export.
func startDischarge(sender *MQTTSender, currentReserve float64) {
	sendTOUTariff(sender)
	sendTeslaAPI(sender, "OPERATION_MODE", map[string]any{
		"default_real_mode": "autonomous",
	})
	sendTeslaAPI(sender, "ENERGY_SITE_IMPORT_EXPORT_CONFIG", map[string]any{
		"customer_preferred_export_rule": "battery_ok",
	})
	nudgeReserve := 22.0
	if currentReserve != 21 {
		nudgeReserve = 21.0
	}
	setBackupReserve(sender, nudgeReserve)
}

// stopDischarge restores self-consumption mode with no battery export.
func stopDischarge(sender *MQTTSender) {
	sendTeslaAPI(sender, "OPERATION_MODE", map[string]any{
		"default_real_mode": "self_consumption",
	})
	sendTeslaAPI(sender, "ENERGY_SITE_IMPORT_EXPORT_CONFIG", map[string]any{
		"customer_preferred_export_rule": "never",
	})
	setBackupReserve(sender, 20)
}

// setBackupReserve sets the Powerwall backup reserve percentage via HA.
func setBackupReserve(sender *MQTTSender, percent float64) {
	sender.CallService("number", "set_value", pw2BackupReserveEntity, map[string]any{
		"value": percent,
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
	startMin := now.Hour()*60 + now.Minute()/30*30
	endMin := (startMin + 60) / 30 * 30
	onPeakStartHour := (startMin / 60) % 24
	onPeakStartMin := startMin % 60
	onPeakEndHour := (endMin / 60) % 24
	onPeakEndMin := endMin % 60

	touPeriods := map[string]any{
		"ON_PEAK": map[string]any{
			"periods": []any{
				map[string]any{
					"fromDayOfWeek": 0,
					"toDayOfWeek":   6,
					"fromHour":      onPeakStartHour,
					"fromMinute":    onPeakStartMin,
					"toHour":        onPeakEndHour,
					"toMinute":      onPeakEndMin,
				},
			},
		},
		"SUPER_OFF_PEAK": map[string]any{
			"periods": []any{
				map[string]any{
					"fromDayOfWeek": 0,
					"toDayOfWeek":   6,
					"fromHour":      onPeakEndHour,
					"fromMinute":    onPeakEndMin,
					"toHour":        onPeakStartHour,
					"toMinute":      onPeakStartMin,
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

