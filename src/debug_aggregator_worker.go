package main

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
)

const (
	markerActive  = "✓"
	markerWarning = "⚠"
	modeManual    = "Manual"
	modeAuto      = "Auto"
)

// formatCombinedDebug renders baseline and dynamic debug info as a single side-by-side GFM table.
func formatCombinedDebug(baseline BaselineDebugInfo, dynamic DynamicDebugInfo) string {
	var leftRows [][3]string  // [name, value, marker]
	var rightRows [][2]string // [name, value]

	if baseline.SafetyReason != "" {
		leftRows = append(leftRows,
			[3]string{"Safety", baseline.SafetyReason, markerWarning},
			[3]string{"Freq", fmt.Sprintf("%.2f Hz", baseline.ACFreqCurrent), ""},
			[3]string{"Freq (5m)", fmt.Sprintf("%.2f Hz", baseline.ACFreqP100), ""},
			[3]string{"Powerwall", fmt.Sprintf("%.1f%%", baseline.PowerwallSOC), ""},
		)
	} else {
		modes := make([]ModeState, len(baseline.Modes))
		copy(modes, baseline.Modes)
		sort.Slice(modes, func(i, j int) bool { return modes[i].Watts > modes[j].Watts })
		for _, m := range modes {
			marker := ""
			if m.Contributing && m.Watts > 0 {
				marker = markerActive
			}
			leftRows = append(leftRows, [3]string{m.Name, fmt.Sprintf("%.0f", m.Watts), marker})
		}
		if baseline.Battery2LowVoltage {
			leftRows = append(leftRows, [3]string{"Low Voltage (B2)", fmt.Sprintf("%.2fV", baseline.Battery2VoltageMin), markerWarning})
		}
	}

	control := modeManual
	if dynamic.Auto {
		control = modeAuto
	}
	rightRows = [][2]string{
		{"Control", control},
		{"Priority", dynamic.Priority},
		{"Setpoint", fmt.Sprintf("%.0fW", dynamic.Setpoint)},
		{"Headroom", fmt.Sprintf("%.0fW", dynamic.Headroom)},
	}

	n := max(len(leftRows), len(rightRows))
	var sb strings.Builder
	sb.WriteString("| B2 Mode | Watts |   |   | B3 Mode | Value |\n")
	sb.WriteString("|---------|------:|---|---|---------|------:|\n")
	for i := range n {
		var l [3]string
		var r [2]string
		if i < len(leftRows) {
			l = leftRows[i]
		}
		if i < len(rightRows) {
			r = rightRows[i]
		}
		fmt.Fprintf(&sb, "| %s | %s | %s |   | %s | %s |\n", l[0], l[1], l[2], r[0], r[1])
	}
	return sb.String()
}

// debugAggregatorWorker collects debug info from both controllers and publishes
// a combined GFM table to input_text.powerhouse_control_debug on change.
func debugAggregatorWorker(
	ctx context.Context,
	baselineChan <-chan BaselineDebugInfo,
	dynamicChan <-chan DynamicDebugInfo,
	sender *MQTTSender,
) {
	log.Println("Debug aggregator started")

	var latestBaseline BaselineDebugInfo
	var latestDynamic DynamicDebugInfo
	var lastOutput string

	publish := func() {
		output := formatCombinedDebug(latestBaseline, latestDynamic)
		if output == lastOutput {
			return
		}
		sender.CallService("input_text", "set_value", "input_text.powerhouse_control_debug", map[string]any{"value": output})
		lastOutput = output
	}

	for {
		select {
		case info := <-baselineChan:
			latestBaseline = info
			publish()
		case info := <-dynamicChan:
			latestDynamic = info
			publish()
		case <-ctx.Done():
			log.Println("Debug aggregator stopped")
			return
		}
	}
}
