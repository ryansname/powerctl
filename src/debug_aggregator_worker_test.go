package main

import (
	"strings"
	"testing"
)

// Suppress unused-function linter errors until Step 6 wires up the aggregator.
var _ = debugAggregatorWorker

func TestFormatCombinedDebug_Normal(t *testing.T) {
	baseline := BaselineDebugInfo{
		Modes: []ModeState{
			{Name: "Overflow", Watts: 2295, Contributing: true},
			{Name: "Forecast Excess", Watts: 1200, Contributing: false},
			{Name: "Baseline", Watts: 350, Contributing: false},
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
		t.Error("expected Default Supply in output")
	}
	if !strings.Contains(out, markerActive) {
		t.Error("expected active marker for contributing mode")
	}
	if !strings.Contains(out, modeAuto) {
		t.Error("expected Auto control mode")
	}
	// Both sections in one table (no second header row)
	if strings.Count(out, "B2 Mode") != 1 {
		t.Error("expected single table header")
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
		Priority: "Safety",
		Setpoint: 0,
		Headroom: -500,
	}

	out := formatCombinedDebug(baseline, dynamic)

	if !strings.Contains(out, "High frequency") {
		t.Error("expected safety reason in output")
	}
	if !strings.Contains(out, markerWarning) {
		t.Error("expected warning marker for safety")
	}
	if !strings.Contains(out, modeManual) {
		t.Error("expected Manual control mode")
	}
}

func TestFormatCombinedDebug_LowVoltage(t *testing.T) {
	baseline := BaselineDebugInfo{
		Modes:              []ModeState{{Name: "Baseline", Watts: 300, Contributing: true}},
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
