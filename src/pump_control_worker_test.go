package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func makePumpInput(headerPercent float64, pumpOn bool) PumpInput {
	return PumpInput{
		HeaderPercent: headerPercent,
		HeaderValid:   true,
		PumpOn:        pumpOn,
	}
}

// at returns a non-flush test time (June 2026) at the given clock time.
func at(hour, minute, second int) time.Time {
	return time.Date(2026, time.June, 11, hour, minute, second, 0, time.UTC)
}

// covers: PUMP-CHECK-1, PUMP-START-1
func TestEvaluatePump_DailyCheckFiresAt11(t *testing.T) {
	state := &PumpControlState{}

	assert.Empty(t, EvaluatePump(state, makePumpInput(50, false), at(10, 59, 0)),
		"no start before 11:00")
	assert.Equal(t, []PumpAction{ActionStartPumpTimer},
		EvaluatePump(state, makePumpInput(50, false), at(11, 0, 0)))
}

// covers: PUMP-CHECK-1 (at most once per day)
func TestEvaluatePump_DailyCheckConsumedForTheDay(t *testing.T) {
	state := &PumpControlState{}

	assert.NotEmpty(t, EvaluatePump(state, makePumpInput(50, false), at(11, 0, 0)))
	// Still within the 11:00 hour and past the command cooldown, pump still off:
	// the day is consumed.
	assert.Empty(t, EvaluatePump(state, makePumpInput(50, false), at(11, 30, 0)))

	// Next day it fires again.
	nextDay := at(11, 0, 0).AddDate(0, 0, 1)
	assert.Equal(t, []PumpAction{ActionStartPumpTimer},
		EvaluatePump(state, makePumpInput(50, false), nextDay))
}

// covers: PUMP-CHECK-2 (no catch-up: a start after the 11:00 hour skips the day)
func TestEvaluatePump_LateStartSkipsDay(t *testing.T) {
	state := &PumpControlState{}

	assert.Empty(t, EvaluatePump(state, makePumpInput(50, false), at(12, 0, 0)))
	assert.Empty(t, EvaluatePump(state, makePumpInput(50, false), at(14, 0, 0)))
}

// covers: PUMP-START-1 (75% boundary)
func TestEvaluatePump_DailyCheckThresholdBoundary(t *testing.T) {
	state := &PumpControlState{}
	assert.Empty(t, EvaluatePump(state, makePumpInput(75.0, false), at(11, 0, 0)),
		"75.0% is not below the threshold")

	state = &PumpControlState{}
	assert.NotEmpty(t, EvaluatePump(state, makePumpInput(74.9, false), at(11, 0, 0)))
}

// covers: PUMP-START-2, PUMP-FLUSH-1
func TestEvaluatePump_FlushModeUsesDeepDrainThreshold(t *testing.T) {
	flushDay := time.Date(2026, time.July, 3, 11, 0, 0, 0, time.UTC)

	state := &PumpControlState{}
	assert.Empty(t, EvaluatePump(state, makePumpInput(60, false), flushDay),
		"60% is below 75 but flush requires below 15")

	state = &PumpControlState{}
	assert.Equal(t, []PumpAction{ActionStartPumpTimer},
		EvaluatePump(state, makePumpInput(14.9, false), flushDay))
}

// covers: PUMP-FLUSH-1 (fortnight boundary within a flush month)
func TestEvaluatePump_FlushMonthAfterFortnightIsNormal(t *testing.T) {
	state := &PumpControlState{}
	july20 := time.Date(2026, time.July, 20, 11, 0, 0, 0, time.UTC)
	assert.Equal(t, []PumpAction{ActionStartPumpTimer},
		EvaluatePump(state, makePumpInput(60, false), july20))
}

// covers: PUMP-START-3
func TestEvaluatePump_FloorStartsAnyTime(t *testing.T) {
	state := &PumpControlState{}
	assert.Equal(t, []PumpAction{ActionStartPumpTimer},
		EvaluatePump(state, makePumpInput(4.9, false), at(9, 0, 0)),
		"critically low header starts before the daily check window")

	// In flush mode too.
	state = &PumpControlState{}
	flushEvening := time.Date(2026, time.July, 3, 20, 0, 0, 0, time.UTC)
	assert.Equal(t, []PumpAction{ActionStartPumpTimer},
		EvaluatePump(state, makePumpInput(4.9, false), flushEvening))
}

// covers: PUMP-RATE-1 (start cooldown)
func TestEvaluatePump_StartCooldown(t *testing.T) {
	state := &PumpControlState{}

	assert.NotEmpty(t, EvaluatePump(state, makePumpInput(4.9, false), at(9, 0, 0)))
	assert.Empty(t, EvaluatePump(state, makePumpInput(4.9, false), at(9, 0, 30)),
		"no re-send while HA state propagates")
	assert.NotEmpty(t, EvaluatePump(state, makePumpInput(4.9, false), at(9, 1, 30)),
		"re-sends once the cooldown expires if the pump still isn't on")
}

// covers: PUMP-STOP-1
func TestEvaluatePump_StopWhenFull(t *testing.T) {
	state := &PumpControlState{}

	assert.Empty(t, EvaluatePump(state, makePumpInput(89.9, true), at(13, 0, 0)),
		"below 90% the pump keeps running")
	assert.Equal(t, []PumpAction{ActionTurnOffPump},
		EvaluatePump(state, makePumpInput(90.0, true), at(13, 5, 0)))
}

// covers: PUMP-STOP-1 (no-op when already off), PUMP-RATE-1 (stop cooldown)
func TestEvaluatePump_StopCooldownAndNoOp(t *testing.T) {
	state := &PumpControlState{}

	assert.NotEmpty(t, EvaluatePump(state, makePumpInput(95, true), at(13, 0, 0)))
	assert.Empty(t, EvaluatePump(state, makePumpInput(95, true), at(13, 0, 30)),
		"no re-send within the cooldown")
	assert.NotEmpty(t, EvaluatePump(state, makePumpInput(95, true), at(13, 2, 0)),
		"re-sends after the cooldown if the pump is still on")

	state = &PumpControlState{}
	assert.Empty(t, EvaluatePump(state, makePumpInput(95, false), at(13, 0, 0)),
		"pump already off: nothing to do")
}

// covers: PUMP-GATE-1
func TestEvaluatePump_NoStartWhilePumpOn(t *testing.T) {
	state := &PumpControlState{}

	assert.Empty(t, EvaluatePump(state, makePumpInput(50, true), at(11, 0, 0)),
		"daily check fires but the start is gated on the pump being off")
	assert.Empty(t, EvaluatePump(state, makePumpInput(4.9, true), at(20, 0, 0)),
		"floor start is gated too")
}

// covers: PUMP-INVALID-1, PUMP-CHECK-1 (check deferred while data is invalid)
func TestEvaluatePump_InvalidDataDoesNothing(t *testing.T) {
	state := &PumpControlState{}
	invalid := PumpInput{HeaderPercent: -1000, HeaderValid: false, PumpOn: true}

	assert.Empty(t, EvaluatePump(state, invalid, at(11, 0, 0)),
		"invalid data triggers neither starts nor stops")

	// Data returns within the 11:00 hour: the check was not consumed and fires now.
	assert.Equal(t, []PumpAction{ActionStartPumpTimer},
		EvaluatePump(state, makePumpInput(50, false), at(11, 30, 0)))
}

// covers: PUMP-CHECK-2 (data invalid for the whole 11:00 hour skips the day)
func TestEvaluatePump_InvalidThrough11SkipsDay(t *testing.T) {
	state := &PumpControlState{}
	invalid := PumpInput{HeaderPercent: -1000, HeaderValid: false, PumpOn: false}

	assert.Empty(t, EvaluatePump(state, invalid, at(11, 0, 0)))
	assert.Empty(t, EvaluatePump(state, invalid, at(11, 59, 59)))
	assert.Empty(t, EvaluatePump(state, makePumpInput(50, false), at(13, 0, 0)),
		"data back after the 11:00 hour: no catch-up")
}

// covers: PUMP-FLUSH-1
func TestIsFlushMode(t *testing.T) {
	cases := []struct {
		date  time.Time
		flush bool
	}{
		{time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC), true},
		{time.Date(2026, time.January, 14, 12, 0, 0, 0, time.UTC), true},
		{time.Date(2026, time.January, 15, 12, 0, 0, 0, time.UTC), false},
		{time.Date(2026, time.February, 5, 12, 0, 0, 0, time.UTC), false},
		{time.Date(2026, time.April, 10, 12, 0, 0, 0, time.UTC), true},
		{time.Date(2026, time.June, 11, 12, 0, 0, 0, time.UTC), false},
		{time.Date(2026, time.July, 14, 12, 0, 0, 0, time.UTC), true},
		{time.Date(2026, time.October, 1, 12, 0, 0, 0, time.UTC), true},
		{time.Date(2026, time.December, 31, 12, 0, 0, 0, time.UTC), false},
	}
	for _, c := range cases {
		assert.Equal(t, c.flush, IsFlushMode(c.date), "%s", c.date.Format("2006-01-02"))
	}
}

// covers: PUMP-GATE-1 (input wiring), TANK-VALID-1 (sentinel handling)
func TestExtractPumpInput(t *testing.T) {
	data := DisplayData{
		TopicData: map[string]any{
			TopicHeaderTankLevelsState: &StringTopicData{Current: `{"percent_full": 68.9}`},
			TopicPumpSwitchState:       &BooleanTopicData{Current: true, Raw: "on"},
		},
	}
	in := ExtractPumpInput(data)
	assert.True(t, in.HeaderValid)
	assert.InDelta(t, 68.9, in.HeaderPercent, 0.001)
	assert.True(t, in.PumpOn)

	// Pre-seeded sentinel marks "no data yet".
	data.TopicData[TopicHeaderTankLevelsState] = &StringTopicData{Current: `{"percent_full": -1000}`}
	in = ExtractPumpInput(data)
	assert.False(t, in.HeaderValid)
}
