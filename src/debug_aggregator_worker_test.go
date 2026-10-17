package main

import (
	"strings"
	"testing"
)

func TestFormatCombinedDebug_Normal(t *testing.T) {
	baseline := BaselineDebugInfo{
		Modes: []ModeState{
			{Name: "Overflow", Watts: 2295, Contributing: true},
			{Name: "Forecast Excess", Watts: 1200, Contributing: false},
			{Name: modeBaseline, Watts: 350, Contributing: false},
		},
	}
	dynamic := DynamicDebugInfo{
		Auto:     true,
		Priority: "Default Supply",
		Setpoint: -800,
		Headroom: 1500,
	}

	out := formatCombinedDebug(baseline, dynamic)

	if !strings.Contains(out, "Overflow") {
		t.Error("expected Overflow in output")
	}
	if !strings.Contains(out, "Default Supply") {
		t.Error("expected Default Supply as mode when auto")
	}
	if !strings.Contains(out, "**B3**") {
		t.Error("expected B3 section label in table body")
	}
	if strings.Count(out, "| B2") != 1 {
		t.Error("expected single B2 header")
	}
}

func TestFormatCombinedDebug_B2Safety(t *testing.T) {
	baseline := BaselineDebugInfo{
		SafetyReason:  "High frequency",
		ACFreqCurrent: 53.1,
		ACFreqP100:    53.2,
		PowerwallSOC:  45.0,
	}
	dynamic := DynamicDebugInfo{
		Auto:     false,
		Priority: modeSafety,
		Setpoint: 0,
		Headroom: -500,
	}

	out := formatCombinedDebug(baseline, dynamic)

	if !strings.Contains(out, "High frequency") {
		t.Error("expected safety reason in output")
	}
	if !strings.Contains(out, "Manual") {
		t.Error("expected Manual mode")
	}
}

func TestFormatCombinedDebug_LowVoltage(t *testing.T) {
	baseline := BaselineDebugInfo{
		Modes:              []ModeState{{Name: modeBaseline, Watts: 300, Contributing: true}},
		Battery2LowVoltage: true,
		Battery2VoltageMin: 50.60,
	}
	dynamic := DynamicDebugInfo{Auto: true, Priority: "Charge from Surplus", Setpoint: 500, Headroom: 2000}

	out := formatCombinedDebug(baseline, dynamic)

	if !strings.Contains(out, "Low Voltage") {
		t.Error("expected Low Voltage row in output")
	}
	if !strings.Contains(out, "50.60V") {
		t.Error("expected voltage reading in output")
	}
}

// The common case: bus healthy, nothing throttling. Neither
// voltage row should take up space — the voltage is displayed better elsewhere.
func TestFormatCombinedDebug_VoltageRowsHiddenWhenNotCurtailing(t *testing.T) {
	dynamic := DynamicDebugInfo{
		Auto:        true,
		Priority:    "Default Supply",
		BusVoltage:  230.0, // ordinary voltage, nowhere near the limit
		VoltageCapW: 1150,  // tracking just above output, as it always does
		ExportCapW:  dynamicMaxDischargeW,
	}

	out := formatCombinedDebug(BaselineDebugInfo{}, dynamic)

	if strings.Contains(out, "Bus V") || strings.Contains(out, "230.0V") {
		t.Errorf("expected bus voltage hidden while not curtailing, got:\n%s", out)
	}
	if strings.Contains(out, "Volt Limit") {
		t.Errorf("expected voltage ceiling hidden at max, got:\n%s", out)
	}
	if strings.Contains(out, "Export Limit") {
		t.Errorf("export limit should be hidden when unrestricted, got:\n%s", out)
	}
}

// Once the bus is high enough to hold or throttle, both rows appear so the ceiling can be watched
// settling or hunting against the voltage driving it.
func TestFormatCombinedDebug_VoltageRowsShownWhenCurtailing(t *testing.T) {
	dynamic := DynamicDebugInfo{
		Auto:        true,
		Priority:    "Default Supply",
		BusVoltage:  251.4,
		VoltageCapW: 1800,
		ExportCapW:  dynamicMaxDischargeW,
	}

	out := formatCombinedDebug(BaselineDebugInfo{}, dynamic)

	if !strings.Contains(out, "Bus V") || !strings.Contains(out, "251.4V") {
		t.Errorf("expected bus voltage while curtailing, got:\n%s", out)
	}
	if !strings.Contains(out, "Volt Limit") || !strings.Contains(out, "1800W") {
		t.Errorf("expected voltage ceiling while curtailing, got:\n%s", out)
	}
}

// If the sensor dies while curtailed the ceiling holds, so the limit still shows
// — but an absent reading must not render as a plausible-looking 0.0V.
func TestFormatCombinedDebug_CurtailingWithNoReadingHidesVoltage(t *testing.T) {
	dynamic := DynamicDebugInfo{Auto: true, Priority: "Default Supply", VoltageCapW: 1200}

	out := formatCombinedDebug(BaselineDebugInfo{}, dynamic)

	if strings.Contains(out, "Bus V") || strings.Contains(out, "0.0V") {
		t.Errorf("expected no bus voltage row without a reading, got:\n%s", out)
	}
	if !strings.Contains(out, "Volt Limit") {
		t.Errorf("expected voltage ceiling still shown, got:\n%s", out)
	}
}

// The export limit shows once it actually restricts discharge.
func TestFormatCombinedDebug_ExportLimitShownWhenBinding(t *testing.T) {
	dynamic := DynamicDebugInfo{Auto: true, Priority: "Default Supply", ExportCapW: 500}

	out := formatCombinedDebug(BaselineDebugInfo{}, dynamic)

	if !strings.Contains(out, "Export Limit") || !strings.Contains(out, "500W") {
		t.Errorf("expected export limit in output, got:\n%s", out)
	}
}
