package main

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// makeDisplayData builds a DisplayData with the given topic values for testing.
func makeDisplayData(bools map[string]bool, floats map[string]float64, strs map[string]string) DisplayData {
	td := map[string]any{}
	for topic, v := range bools {
		raw := payloadOff
		if v {
			raw = payloadOn
		}
		td[topic] = &BooleanTopicData{Current: v, Raw: raw}
	}
	for topic, v := range floats {
		td[topic] = &FloatTopicData{Current: v}
	}
	for topic, v := range strs {
		td[topic] = &StringTopicData{Current: v}
	}
	return DisplayData{TopicData: td}
}

func TestLoadStateSeeds_MissingFileUsesFallbacks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.toml")
	seeds := LoadStateSeeds(path)

	byTopic := map[string]string{}
	for _, s := range seeds {
		byTopic[s.Topic] = s.Value
	}
	require.Len(t, seeds, len(durableTopics))
	assert.Equal(t, payloadOn, byTopic[TopicPowerctlEnabledState])
	assert.Equal(t, payloadOff, byTopic[TopicCarChargingEnabledState])
	assert.Equal(t, PW2DischargeModeAuto, byTopic[TopicPW2DischargeMode])
	assert.Equal(t, "20", byTopic[TopicCarChargingBattery3CutoffState])
}

func TestSnapshotWriteLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.toml")
	data := makeDisplayData(
		map[string]bool{
			TopicPowerctlEnabledState:            true,
			TopicPowerhouseInvertersEnabledState: false,
			TopicExpectingPowerCutsState:         true,
			TopicDynamicAutoState:                false,
			TopicCarChargingEnabledState:         true,
		},
		map[string]float64{TopicCarChargingBattery3CutoffState: 42},
		map[string]string{TopicPW2DischargeMode: PW2DischargeModeForceOff},
	)

	snap := snapshotDurable(data)
	require.Len(t, snap, len(durableTopics))
	require.NoError(t, writeStateFile(path, snap))

	// LoadStateSeeds should reproduce the persisted values (not the fallbacks).
	byTopic := map[string]string{}
	for _, s := range LoadStateSeeds(path) {
		byTopic[s.Topic] = s.Value
	}
	assert.Equal(t, payloadOn, byTopic[TopicPowerctlEnabledState])
	assert.Equal(t, payloadOff, byTopic[TopicPowerhouseInvertersEnabledState])
	assert.Equal(t, payloadOn, byTopic[TopicCarChargingEnabledState])
	assert.Equal(t, payloadOff, byTopic[TopicDynamicAutoState])
	assert.Equal(t, PW2DischargeModeForceOff, byTopic[TopicPW2DischargeMode])
	assert.Equal(t, "42", byTopic[TopicCarChargingBattery3CutoffState])
}

func TestSnapshotSkipsAbsentTopics(t *testing.T) {
	// Only one topic present; the rest must be skipped, not defaulted to zero.
	data := makeDisplayData(map[string]bool{TopicPowerctlEnabledState: true}, nil, nil)
	snap := snapshotDurable(data)
	assert.Equal(t, payloadOn, snap["powerctl_enabled"])
	_, ok := snap["dynamic_auto"]
	assert.False(t, ok, "absent topic should not be snapshotted")
}

func TestPartialFileMergesWithFallbacks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.toml")
	// Persist only a subset of keys.
	require.NoError(t, writeStateFile(path, map[string]string{
		"car_charging":           payloadOn,
		"car_charging_b3_cutoff": "15",
	}))

	byTopic := map[string]string{}
	for _, s := range LoadStateSeeds(path) {
		byTopic[s.Topic] = s.Value
	}
	// File keys win.
	assert.Equal(t, payloadOn, byTopic[TopicCarChargingEnabledState])
	assert.Equal(t, "15", byTopic[TopicCarChargingBattery3CutoffState])
	// Absent keys fall back.
	assert.Equal(t, payloadOn, byTopic[TopicPowerctlEnabledState])
	assert.Equal(t, PW2DischargeModeAuto, byTopic[TopicPW2DischargeMode])
}

func TestStateRestoreAndClearMessages(t *testing.T) {
	values := resolvedValues(map[string]string{"dynamic_auto": payloadOff})

	restore := stateRestoreMessages(values)
	require.Len(t, restore, len(durableTopics))
	for _, m := range restore {
		assert.False(t, m.Retain, "state restore must be retain=false")
	}
	byTopic := map[string]string{}
	for _, m := range restore {
		byTopic[m.Topic] = string(m.Payload)
	}
	assert.Equal(t, payloadOff, byTopic[TopicDynamicAutoState])

	clear := clearRetainedStateMessages()
	require.Len(t, clear, len(durableTopics))
	for _, m := range clear {
		assert.True(t, m.Retain, "retained clear must be retain=true")
		assert.Empty(t, m.Payload, "retained clear must have empty payload")
	}
}
