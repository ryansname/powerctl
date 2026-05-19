package main

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
)

const modeManual = "Manual"

// formatCombinedDebug renders baseline and dynamic debug info as a single two-column GFM table
// with B2 rows at the top, a blank separator row, then B3 rows below.
func formatCombinedDebug(baseline BaselineDebugInfo, dynamic DynamicDebugInfo) string {
	var rows [][2]string

	if baseline.SafetyReason != "" {
		rows = append(rows, [2]string{modeSafety, baseline.SafetyReason})
	} else {
		modes := make([]ModeState, len(baseline.Modes))
		copy(modes, baseline.Modes)
		sort.Slice(modes, func(i, j int) bool { return modes[i].Watts > modes[j].Watts })
		if len(modes) > 0 && modes[0].Watts != 0 {
			rows = append(rows, [2]string{modes[0].Name, fmt.Sprintf("%.0f", modes[0].Watts)})
		}
		if baseline.Battery2LowVoltage {
			rows = append(rows, [2]string{"Low Voltage", fmt.Sprintf("%d @ %.2fV", baseline.Battery2VoltageMaxInv, baseline.Battery2VoltageMin)})
		}
	}

	rows = append(rows, [2]string{"", ""})
	mode := modeManual
	if dynamic.Auto {
		mode = dynamic.Priority
	}
	rows = append(rows, [2]string{"**B3**", mode})
	rows = append(rows,
		[2]string{"Setpoint", fmt.Sprintf("%.0fW", dynamic.Setpoint)},
		[2]string{"Headroom", fmt.Sprintf("%.0fW", dynamic.Headroom)},
	)
	if dynamic.CarCharging != "" {
		rows = append(rows, [2]string{"Car", dynamic.CarCharging})
	}
	if dynamic.CCLOverflowW > 0 {
		rows = append(rows, [2]string{"CCL+", fmt.Sprintf("%.0fW", dynamic.CCLOverflowW)})
	}
	if dynamic.CVLOverflowW > 0 {
		rows = append(rows, [2]string{"CVL+", fmt.Sprintf("%.0fW", dynamic.CVLOverflowW)})
	}
	if dynamic.B3ChargeMaxW < dynamicMaxChargeW {
		rows = append(rows, [2]string{"Charge Limit", fmt.Sprintf("%.0fW", dynamic.B3ChargeMaxW)})
	}

	var sb strings.Builder
	sb.WriteString("| B2 | Value |\n")
	sb.WriteString("|---|---:|\n")
	for _, r := range rows {
		fmt.Fprintf(&sb, "| %s | %s |\n", r[0], r[1])
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
		sender.CallService("input_text", "set_value", "input_text.powerhouse_control_debug", map[string]any{haServiceValueKey: output})
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
