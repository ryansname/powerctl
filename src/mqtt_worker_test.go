package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestInverterDropoutValue(t *testing.T) {
	const power3 = "homeassistant/sensor/powerhouse_inverter_3_switch_0_power/state"
	const switch3 = "homeassistant/switch/powerhouse_inverter_3_switch_0/state"
	const energy3 = "homeassistant/sensor/powerhouse_inverter_3_switch_0_energy/state"
	const multiplus = "homeassistant/sensor/powerhouse_inverter_10_ac_power/state"

	assert.Equal(t, "0", inverterDropoutValue(power3, "unavailable"))
	assert.Equal(t, "off", inverterDropoutValue(switch3, "unavailable"))
	assert.Equal(t, "-255.5", inverterDropoutValue(power3, "-255.5"))

	// A cumulative counter must never read 0, and the Multiplus isn't part of the suite.
	assert.True(t, isDroppedValue(energy3, inverterDropoutValue(energy3, "unavailable")))
	assert.True(t, isDroppedValue(multiplus, inverterDropoutValue(multiplus, "unavailable")))
}
