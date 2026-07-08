package main

import "log"

// announceDiscovery (re)publishes every powerctl-owned HA entity via MQTT
// discovery. It runs on each MQTT (re)connect and on an HA birth message so the
// entities are restored after a powerctl, broker, or HA restart. Discovery
// configs are retained (the entity definition); their live state is restored
// separately (retain=false) by the state store.
//
// Errors here only come from JSON marshalling of static configs, so they are
// logged rather than fatal — a transient reconnect must not kill the process.
func announceDiscovery(s *MQTTSender, batteries []BatteryConfig) {
	logErr := func(what string, err error) {
		if err != nil {
			log.Printf("discovery: failed to create %s: %v", what, err)
		}
	}

	for _, b := range batteries {
		if b.CerboSOCTopic != "" {
			logErr(b.Name+" State of Charge",
				s.CreateBatterySOCEntityFromCerbo(b.Name, b.CapacityKWh, b.Manufacturer, b.CerboSOCTopic))
		} else {
			logErr(b.Name+" State of Charge", s.CreateBatteryEntity(
				b.Name, b.CapacityKWh, b.Manufacturer,
				"State of Charge", "battery", "%", "percentage", 1,
			))
		}
		logErr(b.Name+" Available Energy", s.CreateBatteryEntity(
			b.Name, b.CapacityKWh, b.Manufacturer,
			"Available Energy", "energy", "Wh", "available_wh", 0,
		))
	}

	logErr("powerctl switch", s.CreatePowerctlSwitch())
	logErr("powerhouse inverters switch", s.CreatePowerhouseInvertersSwitch())

	// Clean up any HA entities that have been renamed or retired.
	s.DeleteOldEntities()

	logErr("PW2 discharge mode select", s.CreatePW2DischargeModeSelect())
	logErr("expecting power cuts switch", s.CreateExpectingPowerCutsSwitch())
	logErr("inverter 10 AC setpoint entity", s.CreateInverter10ACSetpointEntity())
	logErr("sleep ryan button", s.createButton(
		"powerctl_sleep_ryan",
		"Sleep Ryan",
		"mdi:weather-night",
		TopicSleepRyanPress,
	))
	logErr("inverter 10 AC power entity", s.CreateMultiplusACPowerEntity())
	logErr("inverter 10 DC current entity", s.CreateMultiplusDCCurrentEntity())
	logErr("Solar 3 MPPT mode entity", s.CreateSolarMpptModeEntity("Solar 3", TopicSolarcharger279MppMode))
	logErr("Solar 4 MPPT mode entity", s.CreateSolarMpptModeEntity("Solar 4", TopicSolarcharger278MppMode))
	logErr("Battery 3 DC power entity", s.CreateBattery3DCPowerEntity())
	logErr("Battery 3 DC current entity", s.CreateBattery3CurrentEntity())
	logErr("Battery 3 CCL entity", s.CreateBattery3CCLEntity())
	logErr("Battery 3 CVL entity", s.CreateBattery3CVLEntity())
	logErr("dynamic auto switch", s.CreateDynamicAutoSwitch())
	logErr("car charging switch", s.CreateCarChargingSwitch())
	logErr("car charging cutoff entity", s.CreateCarChargingBattery3CutoffEntity())
	logErr("water tank entities", s.CreateWaterTankEntities())
	logErr("tank flush mode binary sensor", s.CreateTankFlushModeBinarySensor())
}
