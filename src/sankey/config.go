package sankey

const (
	groupBattery2        = "battery_2"
	groupBattery3        = "battery_3"
	groupPowerhouseNet   = "powerhouse_net"
	groupGridExport      = "grid_export"
	groupHouseMains      = "house_mains"
	groupPowerwallCharge = "powerwall_charge"
)

// DefaultConfig returns the embedded sankey configuration
func DefaultConfig() Config {
	return Config{
		Sensors: []SensorTemplate{
			{Name: "powerhouse_inverter_1_switch_0_power_inverted", Type: TemplateFormula, Formula: "states('sensor.powerhouse_inverter_1_switch_0_power') | multiply(-1)"},
			{Name: "powerhouse_inverter_2_switch_0_power_inverted", Type: TemplateFormula, Formula: "states('sensor.powerhouse_inverter_2_switch_0_power') | multiply(-1)"},
			{Name: "powerhouse_inverter_3_switch_0_power_inverted", Type: TemplateFormula, Formula: "states('sensor.powerhouse_inverter_3_switch_0_power') | multiply(-1)"},
			{Name: "powerhouse_inverter_4_switch_0_power_inverted", Type: TemplateFormula, Formula: "states('sensor.powerhouse_inverter_4_switch_0_power') | multiply(-1)"},
			{Name: "powerhouse_inverter_5_switch_0_power_inverted", Type: TemplateFormula, Formula: "states('sensor.powerhouse_inverter_5_switch_0_power') | multiply(-1)"},
			{Name: "powerhouse_inverter_6_switch_0_power_inverted", Type: TemplateFormula, Formula: "states('sensor.powerhouse_inverter_6_switch_0_power') | multiply(-1)"},
			{Name: "powerhouse_inverter_7_switch_0_power_inverted", Type: TemplateFormula, Formula: "states('sensor.powerhouse_inverter_7_switch_0_power') | multiply(-1)"},
			{Name: "powerhouse_inverter_8_switch_0_power_inverted", Type: TemplateFormula, Formula: "states('sensor.powerhouse_inverter_8_switch_0_power') | multiply(-1)"},
			{Name: "powerhouse_inverter_9_switch_0_power_inverted", Type: TemplateFormula, Formula: "states('sensor.powerhouse_inverter_9_switch_0_power') | multiply(-1)"},
			{Name: "home_sweet_home_site_power_inverted", Type: TemplateFormula, Formula: "states('sensor.home_sweet_home_site_power') | multiply(-1000)"},
			{Name: "home_sweet_home_battery_power_2_inverted", Type: TemplateFormula, Formula: "states('sensor.home_sweet_home_battery_power_2') | multiply(-1000)"},
			{Name: "powerhouse_inverter_10_ac_power_inverted", Type: TemplateFormula, Formula: "states('sensor.powerhouse_inverter_10_ac_power') | float(0) | multiply(-1)"},
			{Name: "solar_2_power", Type: TemplateFormula, Formula: "states('sensor.home_sweet_home_solar_power_2') | multiply(1000) - states('sensor.powerhouse_net_power') | float"},
			{Name: "all_lights", Type: TemplateSum, Entities: []string{"sensor.dining_room_power", "sensor.downlight_power", "sensor.outside_power", "sensor.triple_power"}},
		},
		Groups: []Group{
			{
				Name:     "battery_2_charging",
				Section:  SectionPowerhouseIn,
				Sensors:  []Sensor{{Name: "sensor.solar_5_solar_power", Label: "Solar 5"}},
				Children: []string{groupBattery2},
			},
			{
				Name:    "battery_3_charging",
				Section: SectionPowerhouseIn,
				Sensors: []Sensor{
					{Name: "sensor.solar_3_solar_power", Label: "Solar 3"},
					{Name: "sensor.solar_4_solar_power", Label: "Solar 4"},
					{Name: "sensor.powerhouse_inverter_10_ac_power", Label: "Powerhouse 10"},
				},
				Children: []string{groupBattery3},
			},
			{
				Name:     groupBattery2,
				Section:  SectionPowerhouse,
				Children: []string{"microinverter_bank_2"},
				Other: &RemainderStrategy{
					Key:        groupBattery2,
					Label:      "Battery 2",
					Type:       RemainderChildState,
					ParentsSum: &Reconcile{ShouldBe: ShouldBeEqualOrLess, ReconcileTo: ReconcileToMax},
				},
			},
			{
				Name:     groupBattery3,
				Section:  SectionPowerhouse,
				Children: []string{"powerhouse_10"},
				Other: &RemainderStrategy{
					Key:        groupBattery3,
					Label:      "Battery 3",
					Type:       RemainderChildState,
					ParentsSum: &Reconcile{ShouldBe: ShouldBeEqualOrLess, ReconcileTo: ReconcileToMax},
				},
			},
			{
				Name:    "microinverter_bank_2",
				Section: SectionPowerhouseOut,
				Sensors: []Sensor{
					{Name: "sensor.powerhouse_inverter_1_switch_0_power_inverted", Label: "Powerhouse 1"},
					{Name: "sensor.powerhouse_inverter_2_switch_0_power_inverted", Label: "Powerhouse 2"},
					{Name: "sensor.powerhouse_inverter_3_switch_0_power_inverted", Label: "Powerhouse 3"},
					{Name: "sensor.powerhouse_inverter_4_switch_0_power_inverted", Label: "Powerhouse 4"},
					{Name: "sensor.powerhouse_inverter_5_switch_0_power_inverted", Label: "Powerhouse 5"},
					{Name: "sensor.powerhouse_inverter_6_switch_0_power_inverted", Label: "Powerhouse 6"},
					{Name: "sensor.powerhouse_inverter_7_switch_0_power_inverted", Label: "Powerhouse 7"},
					{Name: "sensor.powerhouse_inverter_8_switch_0_power_inverted", Label: "Powerhouse 8"},
					{Name: "sensor.powerhouse_inverter_9_switch_0_power_inverted", Label: "Powerhouse 9"},
				},
				Children: []string{groupPowerhouseNet},
			},
			{
				Name:     "solar_1",
				Section:  SectionPowerhouseOut,
				Sensors:  []Sensor{{Name: "sensor.solar_1_power", Label: "Solar 1"}},
				Children: []string{groupPowerhouseNet},
			},
			{
				Name:     groupPowerhouseNet,
				Section:  SectionHouseMainsIn,
				Sensors:  []Sensor{{Name: "sensor.powerhouse_net_power", Label: "Powerhouse"}},
				Children: []string{groupHouseMains, groupGridExport, groupPowerwallCharge},
			},
			{
				Name:     "powerhouse_10",
				Section:  SectionPowerhouseOut,
				Sensors:  []Sensor{{Name: "sensor.powerhouse_inverter_10_ac_power_inverted", Label: "Powerhouse 10"}},
				Children: []string{groupPowerhouseNet},
			},
			{
				Name:     "powerwall_discharge",
				Section:  SectionHouseMainsIn,
				Sensors:  []Sensor{{Name: "sensor.home_sweet_home_battery_power_2", Label: "Powerwall"}},
				Children: []string{groupHouseMains},
			},
			{
				Name:     "solar_2",
				Section:  SectionHouseMainsIn,
				Sensors:  []Sensor{{Name: "sensor.primo_5_0_ac_power", Label: "Solar 2"}},
				Children: []string{groupHouseMains, groupGridExport, groupPowerwallCharge},
			},
			{
				Name:     "grid_import",
				Section:  SectionHouseMainsIn,
				Sensors:  []Sensor{{Name: "sensor.home_sweet_home_site_power", Label: "Buying In"}},
				Children: []string{groupHouseMains, groupGridExport},
			},
			{
				Name:    groupGridExport,
				Section: SectionHouseMains,
				Sensors: []Sensor{{Name: "sensor.home_sweet_home_site_power_inverted", Label: "Selling Back"}},
			},
			{
				Name:    groupPowerwallCharge,
				Section: SectionHouseMains,
				Sensors: []Sensor{{Name: "sensor.home_sweet_home_battery_power_2_inverted", Label: "Powerwall"}},
			},
			{
				Name:     groupHouseMains,
				Section:  SectionHouseMains,
				Sensors:  []Sensor{},
				Other:    &RemainderStrategy{Key: groupHouseMains, Label: "House Usage", Type: RemainderParentState},
				Children: []string{"house_draw_components"},
			},
			{
				Name:    "house_draw_components",
				Section: SectionHouseMainsOut,
				Sensors: []Sensor{
					{Name: "sensor.all_lights", Label: "Lights"},
					{Name: "sensor.dryer_power"},
					{Name: "sensor.hot_water_cylinder_power"},
					{Name: "sensor.lounge_ac_measure_channel_1_power", Label: "Lounge A/C"},
					{Name: "sensor.lounge_ac_measure_channel_2_power"},
					{Name: "sensor.plb942_charger_power"},
					{Name: "sensor.shelly_4_shed_switch_0_power"},
					{Name: "sensor.shelly_4_shed_switch_1_power"},
					{Name: "sensor.shelly_4_shed_switch_2_power"},
					{Name: "sensor.shelly_4_shed_switch_3_power"},
					{Name: "sensor.washing_machine_power"},
					{Name: "sensor.mums_estimated_power_consumption", Label: "Mums A/C"},
					{Name: "sensor.ryans_estimated_power_consumption", Label: "Ryans A/C"},
					{Name: "sensor.blakes_estimated_power_consumption", Label: "Blakes A/C"},
					{Name: "sensor.miner1_cur_load", Label: "Miner 1"},
				},
				Other: &RemainderStrategy{Key: "unaccounted_power_draw", Label: "Other", Type: RemainderParentState},
			},
		},
	}
}
