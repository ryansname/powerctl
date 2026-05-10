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

	log.Println("PW2 discharge: sending initial Octopus tariff")
	sendOctopusTariff(sender)

	for {
		select {
		case data := <-dataChan:
			switchEnabled, switchChanged := data.GetBoolean(TopicPW2DischargeState)
			currentMode := data.GetString(TopicPW2OperationMode)
			backupReserve := data.GetFloat(TopicPW2BackupReserve).Current

			// Cooldown after sending commands (wait for mode to update)
			if time.Since(lastCommandSent) < commandCooldown {
				continue
			}

			actuallyDischarging := currentMode == "Time-Based Control"
			switchDisabled := switchChanged && !switchEnabled

			switch {
			case switchDisabled:
				log.Println("PW2 discharge: deactivating")
				stopDischarge(sender)
				requestModeUpdate(sender)
				lastCommandSent = time.Now()
			case switchEnabled && !actuallyDischarging:
				log.Println("PW2 discharge: activating")
				startDischarge(sender, backupReserve)
				requestModeUpdate(sender)
				lastCommandSent = time.Now()
				lastTOURefresh = time.Now()
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

// stopDischarge restores self-consumption mode with no battery export and resets the tariff.
func stopDischarge(sender *MQTTSender) {
	sendOctopusTariff(sender)
	sendTeslaAPI(sender, "OPERATION_MODE", map[string]any{
		"default_real_mode": "self_consumption",
	})
	sendTeslaAPI(sender, "ENERGY_SITE_IMPORT_EXPORT_CONFIG", map[string]any{
		"customer_preferred_export_rule": "never",
	})
	setBackupReserve(sender, 10)
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

// sendOctopusTariff restores the Octopus/Vector pricing schedule to the Powerwall.
func sendOctopusTariff(sender *MQTTSender) {
	sendTeslaAPI(sender, "TIME_OF_USE_SETTINGS", map[string]any{
		"tou_settings": map[string]any{
			"tariff_content_v2": buildOctopusTariff(),
		},
	})
}

// buildOctopusTariff returns the tariff_content_v2 for the Octopus/Vector residential plan.
// Current tariff readable via: tesla_custom.api command=SITE_TARIFF parameters={path_vars: {site_id: "2233628"}}
//
// Band semantics — within each season we honour Tesla's standard ordering
//   ON_PEAK ≥ PARTIAL_PEAK ≥ OFF_PEAK ≥ SUPER_OFF_PEAK
// (writes that violate it are silently rejected). So:
//   ON_PEAK       — peak hours WITH Vector rebate (highest sell rate)
//   PARTIAL_PEAK  — peak hours / shoulders WITHOUT rebate
//   OFF_PEAK      — mid-day / late-evening on non-rebate seasons / weekend daytime
//   SUPER_OFF_PEAK— overnight
//
// Seasons:
//   Summer (Oct–Apr)    — no rebate; both peaks ON_PEAK at no-rebate rate (0.19)
//   ShoulderMay (May)   — evening rebate; ON_PEAK=evening (rebated), PARTIAL_PEAK=morning+21:00–22:00
//   Winter (Jun–Aug)    — full rebate; both peaks ON_PEAK (rebated), PARTIAL_PEAK=21:00–22:00
//   ShoulderSep (Sep)   — evening rebate; mirrors ShoulderMay
//
// Vector rebate: +5.24c on the rebated slot → 0.2424.
func buildOctopusTariff() map[string]any {
	// Tesla's TIME_OF_USE_SETTINGS write API requires the wrapped wire format:
	//   tou_periods band → {"periods": [...]} (not bare array)
	//   energy_charges/demand_charges entry → {"rates": {...}} (not flat map)
	// (The SITE_TARIFF read endpoint returns the unwrapped form, which is misleading.)
	wrapBands := func(bands map[string]any) map[string]any {
		out := make(map[string]any, len(bands))
		for k, v := range bands {
			out[k] = map[string]any{"periods": v}
		}
		return out
	}
	wrapRates := func(rates map[string]any) map[string]any {
		return map[string]any{"rates": rates}
	}

	makeSeason := func(bands map[string]any, fromMonth, toMonth, toDay int) map[string]any {
		return map[string]any{
			"fromMonth":   fromMonth,
			"fromDay":     1,
			"toMonth":     toMonth,
			"toDay":       toDay,
			"tou_periods": wrapBands(bands),
		}
	}

	zeroAll := wrapRates(map[string]any{"ALL": 0})
	emptyRates := wrapRates(map[string]any{})
	dailyCharges := []any{map[string]any{"name": "Charge", "amount": 0}}

	// Summer periods (no rebate season): standard weekday peaks 07-11 + 17-21.
	summerPeriods := map[string]any{
		"ON_PEAK": []any{
			map[string]any{"fromDayOfWeek": 0, "toDayOfWeek": 4, "fromHour": 7, "fromMinute": 0, "toHour": 11, "toMinute": 0},
			map[string]any{"fromDayOfWeek": 0, "toDayOfWeek": 4, "fromHour": 17, "fromMinute": 0, "toHour": 21, "toMinute": 0},
		},
		"OFF_PEAK": []any{
			map[string]any{"fromDayOfWeek": 0, "toDayOfWeek": 4, "fromHour": 11, "fromMinute": 0, "toHour": 17, "toMinute": 0},
			map[string]any{"fromDayOfWeek": 0, "toDayOfWeek": 4, "fromHour": 21, "fromMinute": 0, "toHour": 23, "toMinute": 0},
			map[string]any{"fromDayOfWeek": 5, "toDayOfWeek": 6, "fromHour": 7, "fromMinute": 0, "toHour": 23, "toMinute": 0},
		},
		"SUPER_OFF_PEAK": []any{
			map[string]any{"fromDayOfWeek": 0, "toDayOfWeek": 4, "fromHour": 23, "fromMinute": 0, "toHour": 7, "toMinute": 0},
			map[string]any{"fromDayOfWeek": 5, "toDayOfWeek": 6, "fromHour": 23, "fromMinute": 0, "toHour": 7, "toMinute": 0},
		},
	}

	// Shoulder periods (May, Sep): only the evening peak gets the rebate.
	// ON_PEAK = rebated evening; PARTIAL_PEAK = unrebated morning + 21–22 transition.
	shoulderPeriods := map[string]any{
		"ON_PEAK": []any{
			map[string]any{"fromDayOfWeek": 0, "toDayOfWeek": 4, "fromHour": 17, "fromMinute": 0, "toHour": 21, "toMinute": 0},
		},
		"PARTIAL_PEAK": []any{
			map[string]any{"fromDayOfWeek": 0, "toDayOfWeek": 4, "fromHour": 7, "fromMinute": 0, "toHour": 11, "toMinute": 0},
			map[string]any{"fromDayOfWeek": 0, "toDayOfWeek": 4, "fromHour": 21, "fromMinute": 0, "toHour": 22, "toMinute": 0},
		},
		"OFF_PEAK": []any{
			map[string]any{"fromDayOfWeek": 0, "toDayOfWeek": 4, "fromHour": 11, "fromMinute": 0, "toHour": 17, "toMinute": 0},
			map[string]any{"fromDayOfWeek": 0, "toDayOfWeek": 4, "fromHour": 22, "fromMinute": 0, "toHour": 23, "toMinute": 0},
			map[string]any{"fromDayOfWeek": 5, "toDayOfWeek": 6, "fromHour": 7, "fromMinute": 0, "toHour": 23, "toMinute": 0},
		},
		"SUPER_OFF_PEAK": []any{
			map[string]any{"fromDayOfWeek": 0, "toDayOfWeek": 4, "fromHour": 23, "fromMinute": 0, "toHour": 7, "toMinute": 0},
			map[string]any{"fromDayOfWeek": 5, "toDayOfWeek": 6, "fromHour": 23, "fromMinute": 0, "toHour": 7, "toMinute": 0},
		},
	}

	// Winter periods (Jun-Aug): both morning and evening peaks rebated.
	// ON_PEAK = both rebated peaks; PARTIAL_PEAK = 21–22 transition slot.
	winterPeriods := map[string]any{
		"ON_PEAK": []any{
			map[string]any{"fromDayOfWeek": 0, "toDayOfWeek": 4, "fromHour": 7, "fromMinute": 0, "toHour": 11, "toMinute": 0},
			map[string]any{"fromDayOfWeek": 0, "toDayOfWeek": 4, "fromHour": 17, "fromMinute": 0, "toHour": 21, "toMinute": 0},
		},
		"PARTIAL_PEAK": []any{
			map[string]any{"fromDayOfWeek": 0, "toDayOfWeek": 4, "fromHour": 21, "fromMinute": 0, "toHour": 22, "toMinute": 0},
		},
		"OFF_PEAK": []any{
			map[string]any{"fromDayOfWeek": 0, "toDayOfWeek": 4, "fromHour": 11, "fromMinute": 0, "toHour": 17, "toMinute": 0},
			map[string]any{"fromDayOfWeek": 0, "toDayOfWeek": 4, "fromHour": 22, "fromMinute": 0, "toHour": 23, "toMinute": 0},
			map[string]any{"fromDayOfWeek": 5, "toDayOfWeek": 6, "fromHour": 7, "fromMinute": 0, "toHour": 23, "toMinute": 0},
		},
		"SUPER_OFF_PEAK": []any{
			map[string]any{"fromDayOfWeek": 0, "toDayOfWeek": 4, "fromHour": 23, "fromMinute": 0, "toHour": 7, "toMinute": 0},
			map[string]any{"fromDayOfWeek": 5, "toDayOfWeek": 6, "fromHour": 23, "fromMinute": 0, "toHour": 7, "toMinute": 0},
		},
	}

	// Buy rates: PARTIAL_PEAK billed at ON_PEAK rate (same hours, just a label).
	buyRates := map[string]any{"ON_PEAK": 0.42, "PARTIAL_PEAK": 0.42, "OFF_PEAK": 0.34, "SUPER_OFF_PEAK": 0.22}

	// Sell rates per season — ON_PEAK ≥ PARTIAL_PEAK ≥ OFF_PEAK in every case.
	sellSummerRates := map[string]any{"ON_PEAK": 0.19, "OFF_PEAK": 0.14, "SUPER_OFF_PEAK": 0.14}
	sellShoulderRates := map[string]any{"ON_PEAK": 0.2424, "PARTIAL_PEAK": 0.19, "OFF_PEAK": 0.14, "SUPER_OFF_PEAK": 0.14}
	sellWinterRates := map[string]any{"ON_PEAK": 0.2424, "PARTIAL_PEAK": 0.19, "OFF_PEAK": 0.14, "SUPER_OFF_PEAK": 0.14}

	demandCharges := map[string]any{
		"ALL":         zeroAll,
		"Summer":      emptyRates,
		"ShoulderMay": emptyRates,
		"Winter":      emptyRates,
		"ShoulderSep": emptyRates,
	}

	return map[string]any{
		"version":               1,
		"utility":               "Custom",
		"code":                  "CUSTOM-EXPORT",
		"name":                  "Octopus",
		"currency":              "USD",
		"monthly_minimum_bill":  0,
		"min_applicable_demand": 0,
		"max_applicable_demand": 0,
		"monthly_charges":       0,
		"daily_charges":         dailyCharges,
		"daily_demand_charges":  map[string]any{},
		"demand_charges":        demandCharges,
		"energy_charges": map[string]any{
			"ALL":         zeroAll,
			"Summer":      wrapRates(buyRates),
			"ShoulderMay": wrapRates(buyRates),
			"Winter":      wrapRates(buyRates),
			"ShoulderSep": wrapRates(buyRates),
		},
		"seasons": map[string]any{
			// Summer wraps Oct–Apr; fromMonth > toMonth indicates year-wrap.
			"Summer":      makeSeason(summerPeriods, 10, 4, 30),
			"ShoulderMay": makeSeason(shoulderPeriods, 5, 5, 31),
			"Winter":      makeSeason(winterPeriods, 6, 8, 31),
			"ShoulderSep": makeSeason(shoulderPeriods, 9, 9, 30),
		},
		"sell_tariff": map[string]any{
			"utility":               "Custom",
			"monthly_minimum_bill":  0,
			"min_applicable_demand": 0,
			"max_applicable_demand": 0,
			"monthly_charges":       0,
			"daily_charges":         dailyCharges,
			"daily_demand_charges":  map[string]any{},
			"demand_charges":        demandCharges,
			"energy_charges": map[string]any{
				"ALL":         zeroAll,
				"Summer":      wrapRates(sellSummerRates),
				"ShoulderMay": wrapRates(sellShoulderRates),
				"Winter":      wrapRates(sellWinterRates),
				"ShoulderSep": wrapRates(sellShoulderRates),
			},
			"seasons": map[string]any{
				"Summer":      makeSeason(summerPeriods, 10, 4, 30),
				"ShoulderMay": makeSeason(shoulderPeriods, 5, 5, 31),
				"Winter":      makeSeason(winterPeriods, 6, 8, 31),
				"ShoulderSep": makeSeason(shoulderPeriods, 9, 9, 30),
			},
		},
	}
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

// buildTOUTariff creates a tariff_content_v2 structure with ON_PEAK for ~90 minutes
// from the current time and SUPER_OFF_PEAK for the remaining hours.
// Start rounds down to nearest 30min, end rounds to nearest 30min from now+90min.
// Wrapping (toHour < fromHour) is valid and covers the full 24 hours.
func buildTOUTariff(now time.Time) map[string]any {
	totalMin := now.Hour()*60 + now.Minute()
	startMin := totalMin / 30 * 30
	endMin := (totalMin + 90 + 15) / 30 * 30
	onPeakStartHour := (startMin / 60) % 24
	onPeakStartMin := startMin % 60
	onPeakEndHour := (endMin / 60) % 24
	onPeakEndMin := endMin % 60

	// Wrapped wire format required by Tesla's TIME_OF_USE_SETTINGS write endpoint.
	touPeriods := map[string]any{
		"ON_PEAK": map[string]any{"periods": []any{
			map[string]any{
				"fromDayOfWeek": 0,
				"toDayOfWeek":   6,
				"fromHour":      onPeakStartHour,
				"fromMinute":    onPeakStartMin,
				"toHour":        onPeakEndHour,
				"toMinute":      onPeakEndMin,
			},
		}},
		"SUPER_OFF_PEAK": map[string]any{"periods": []any{
			map[string]any{
				"fromDayOfWeek": 0,
				"toDayOfWeek":   6,
				"fromHour":      onPeakEndHour,
				"fromMinute":    onPeakEndMin,
				"toHour":        onPeakStartHour,
				"toMinute":      onPeakStartMin,
			},
		}},
	}

	season := map[string]any{
		"fromMonth":   1,
		"fromDay":     1,
		"toMonth":     12,
		"toDay":       31,
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
		"ALL":     map[string]any{"rates": map[string]any{"ALL": 0}},
		"AllYear": map[string]any{"rates": map[string]any{"ON_PEAK": 0.31, "SUPER_OFF_PEAK": 0.07}},
	}

	sellRates := map[string]any{
		"ALL":     map[string]any{"rates": map[string]any{"ALL": 0}},
		"AllYear": map[string]any{"rates": map[string]any{"ON_PEAK": 0.30, "SUPER_OFF_PEAK": 0.07}},
	}

	return map[string]any{
		"version":               1,
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
