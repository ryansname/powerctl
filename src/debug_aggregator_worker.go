package main

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
)

const modeManual = "Manual"

// formatCombinedDebug renders baseline and dynamic debug info as a single side-by-side GFM table.
func formatCombinedDebug(baseline BaselineDebugInfo, dynamic DynamicDebugInfo) string {
	var leftRows [][2]string
	var rightRows [][2]string

	if baseline.SafetyReason != "" {
		leftRows = append(leftRows,
			[2]string{"Safety", baseline.SafetyReason},
			[2]string{"Freq", fmt.Sprintf("%.2f Hz", baseline.ACFreqCurrent)},
			[2]string{"Freq (5m)", fmt.Sprintf("%.2f Hz", baseline.ACFreqP100)},
			[2]string{"Powerwall", fmt.Sprintf("%.1f%%", baseline.PowerwallSOC)},
		)
	} else {
		modes := make([]ModeState, len(baseline.Modes))
		copy(modes, baseline.Modes)
		sort.Slice(modes, func(i, j int) bool { return modes[i].Watts > modes[j].Watts })
		for _, m := range modes {
			if m.Watts != 0 {
				leftRows = append(leftRows, [2]string{m.Name, fmt.Sprintf("%.0f", m.Watts)})
			}
		}
		if baseline.Battery2LowVoltage {
			leftRows = append(leftRows, [2]string{"Low Voltage", fmt.Sprintf("%d @ %.2fV", baseline.Battery2VoltageMaxInv, baseline.Battery2VoltageMin)})
		}
	}

	mode := modeManual
	if dynamic.Auto {
		mode = dynamic.Priority
	}
	rightRows = [][2]string{
		{"Mode", mode},
		{"Setpoint", fmt.Sprintf("%.0fW", dynamic.Setpoint)},
		{"Headroom", fmt.Sprintf("%.0fW", dynamic.Headroom)},
	}
	if dynamic.CarCharging != "" {
		rightRows = append(rightRows, [2]string{"Car", dynamic.CarCharging})
	}

	n := max(len(leftRows), len(rightRows))
	var sb strings.Builder
	sb.WriteString("| B2 | Watts |   | B3 | Value |\n")
	sb.WriteString("|---------|------:|---|---------|------:|\n")
	for i := range n {
		var l [2]string
		var r [2]string
		if i < len(leftRows) {
			l = leftRows[i]
		}
		if i < len(rightRows) {
			r = rightRows[i]
		}
		fmt.Fprintf(&sb, "| %s | %s |   | %s | %s |\n", l[0], l[1], r[0], r[1])
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
