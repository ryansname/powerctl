package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

var loungeACT0 = time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)

func loungeACSetting(mode string, temp float64) LoungeACSetting {
	return LoungeACSetting{Mode: mode, Temp: temp, HasTemp: true}
}

// tickLoungeAC runs the sequence from a desired change at loungeACT0 out to `until`,
// evaluating once a second as the broadcast worker does, and returns every action in
// order. Driving it on the real cadence is the point: the schedule is only ever sampled
// at tick boundaries.
func tickLoungeAC(state *LoungeACState, in LoungeACInput, until time.Duration) []LoungeACAction {
	var got []LoungeACAction
	for at := time.Duration(0); at <= until; at += time.Second {
		got = append(got, EvaluateLoungeAC(state, in, loungeACT0.Add(at))...)
	}
	return got
}

// seededLoungeACState returns a state that has already taken desired as its baseline,
// as it would be after the first evaluation following startup.
func seededLoungeACState(desired LoungeACSetting) *LoungeACState {
	return &LoungeACState{seeded: true, desired: desired}
}

// covers: LAC-TRIGGER-2
func TestEvaluateLoungeAC_FirstReadingIsBaselineNotCommand(t *testing.T) {
	state := &LoungeACState{}
	in := LoungeACInput{
		Desired: loungeACSetting(acStateCool, 21),
		Actual:  loungeACSetting(acStateHeat, 24),
	}

	// A restart must not re-command the AC, even though desired and actual disagree.
	assert.Empty(t, EvaluateLoungeAC(state, in, loungeACT0))
	assert.Empty(t, EvaluateLoungeAC(state, in, loungeACT0.Add(time.Hour)))
}

// covers: LAC-TRIGGER-1, LAC-SCHEDULE-1
func TestEvaluateLoungeAC_RunsTheScheduleWhenTheUnitNeverAgrees(t *testing.T) {
	state := seededLoungeACState(loungeACSetting(acStateCool, 21))
	in := LoungeACInput{
		Desired: loungeACSetting(acStateCool, 18),
		Actual:  loungeACSetting(acStateHeat, 24),
	}

	// Poll, re-send, poll again, then take the unit over directly — and nothing after.
	got := tickLoungeAC(state, in, loungeACFallbackAt+5*time.Second)
	assert.Equal(t, []LoungeACAction{
		ActionLoungeACRefreshWiFi,
		ActionLoungeACResendIR,
		ActionLoungeACRefreshWiFi,
		ActionLoungeACFallbackWiFi,
	}, got)
}

// covers: LAC-SCHEDULE-1
func TestEvaluateLoungeAC_StepsAreTimedFromTheChange(t *testing.T) {
	desired := loungeACSetting(acStateCool, 18)
	in := LoungeACInput{Desired: desired, Actual: loungeACSetting(acStateHeat, 24)}

	for i, tt := range []struct {
		at   time.Duration
		want LoungeACAction
	}{
		{loungeACFirstRefresh, ActionLoungeACRefreshWiFi},
		{loungeACResendIRAt, ActionLoungeACResendIR},
		{loungeACSecondRefresh, ActionLoungeACRefreshWiFi},
		{loungeACFallbackAt, ActionLoungeACFallbackWiFi},
	} {
		state := seededLoungeACState(loungeACSetting(acStateCool, 21))
		// Only the earlier steps have fired by the second before this one is due, and
		// this one fires on the second it is due — no drift from the change.
		before := tickLoungeAC(state, in, tt.at-time.Second)
		at := EvaluateLoungeAC(state, in, loungeACT0.Add(tt.at))

		assert.Len(t, before, i, "wrong number of steps before %v", tt.at)
		assert.Equal(t, []LoungeACAction{tt.want}, at, "step due at %v", tt.at)
	}
}

// covers: LAC-CONFIRM-1
func TestEvaluateLoungeAC_AgreementAtAnyPointEndsTheSequence(t *testing.T) {
	desired := loungeACSetting(acStateCool, 18)
	// Whenever the unit is seen agreeing, every remaining step is abandoned — each one
	// costs either a poll or a dozen Zigbee round trips, for a command already landed.
	for _, agreesAt := range []time.Duration{
		0,
		loungeACFirstRefresh - time.Second,
		loungeACResendIRAt - time.Second,
		loungeACSecondRefresh - time.Second,
		loungeACFallbackAt - time.Second,
	} {
		state := seededLoungeACState(loungeACSetting(acStateCool, 21))
		miss := LoungeACInput{Desired: desired, Actual: loungeACSetting(acStateHeat, 24)}
		tickLoungeAC(state, miss, agreesAt)

		hit := LoungeACInput{Desired: desired, Actual: desired}
		assert.Empty(t, EvaluateLoungeAC(state, hit, loungeACT0.Add(agreesAt)),
			"agreement at %v must end it", agreesAt)
		assert.False(t, state.running)

		// And no later tick may resurrect the abandoned steps.
		var after []LoungeACAction
		for at := agreesAt; at <= loungeACFallbackAt*2; at += time.Second {
			after = append(after, EvaluateLoungeAC(state, hit, loungeACT0.Add(at))...)
		}
		assert.Empty(t, after, "sequence resumed after agreeing at %v", agreesAt)
	}
}

// covers: LAC-TRIGGER-1
func TestEvaluateLoungeAC_ActualChangeAloneDoesNothing(t *testing.T) {
	desired := loungeACSetting(acStateCool, 21)
	state := seededLoungeACState(desired)

	// Someone picks up the physical remote; powerctl must not fight them.
	for _, actual := range []LoungeACSetting{
		loungeACSetting(acStateHeat, 24),
		loungeACSetting(acStateCool, 30),
		{Mode: hvacModeOff},
	} {
		in := LoungeACInput{Desired: desired, Actual: actual}
		assert.Empty(t, EvaluateLoungeAC(state, in, loungeACT0.Add(loungeACFallbackAt*2)),
			"actual %v should not trigger anything", actual)
	}
}

// covers: LAC-FALLBACK-1, LAC-TERMINAL-1
func TestEvaluateLoungeAC_FallsBackToWiFiThenStops(t *testing.T) {
	desired := loungeACSetting(acStateCool, 18)
	state := seededLoungeACState(loungeACSetting(acStateCool, 21))
	in := LoungeACInput{Desired: desired, Actual: loungeACSetting(acStateHeat, 24)}

	tickLoungeAC(state, in, loungeACFallbackAt)

	// Terminal: the WiFi path is the last escalation, so nothing more happens — not even
	// if the unit is still disagreeing an hour later.
	assert.Empty(t, EvaluateLoungeAC(state, in, loungeACT0.Add(time.Hour)))
	assert.False(t, state.running)
}

// covers: LAC-SUPERSEDE-1
func TestEvaluateLoungeAC_NewCommandRestartsSequence(t *testing.T) {
	state := seededLoungeACState(loungeACSetting(acStateCool, 21))
	actual := loungeACSetting(acStateHeat, 24)

	first := LoungeACInput{Desired: loungeACSetting(acStateCool, 18), Actual: actual}
	tickLoungeAC(state, first, loungeACResendIRAt)

	// A new command mid-flight abandons the old sequence and restarts the clock, so the
	// superseded value's remaining steps never fire.
	second := LoungeACInput{Desired: loungeACSetting(acStateCool, 19), Actual: actual}
	changedAt := loungeACResendIRAt + time.Second
	assert.Empty(t, EvaluateLoungeAC(state, second, loungeACT0.Add(changedAt)))
	assert.Equal(t, 0, state.step)

	// The old schedule's remaining steps were due within a few seconds of here.
	for at := changedAt; at < changedAt+loungeACFirstRefresh; at += time.Second {
		assert.Empty(t, EvaluateLoungeAC(state, second, loungeACT0.Add(at)),
			"superseded schedule fired at %v", at)
	}
}

// covers: LAC-SUPERSEDE-1
func TestEvaluateLoungeAC_RapidSteppingSendsOnlyTheFinalValue(t *testing.T) {
	state := seededLoungeACState(loungeACSetting(acStateCool, 20))
	actual := loungeACSetting(acStateCool, 20)

	// Hold the up-arrow in HA: a 0.5C step every second.
	steps := []float64{20.5, 21, 21.5, 22}
	var during []LoungeACAction
	for i, temp := range steps {
		in := LoungeACInput{Desired: loungeACSetting(acStateCool, temp), Actual: actual}
		during = append(during, EvaluateLoungeAC(state, in, loungeACT0.Add(time.Duration(i)*time.Second))...)
	}
	assert.Empty(t, during, "nothing goes out mid-drag: each step supersedes the last")

	// Then it settles and the schedule runs once, carrying the value the user landed on
	// rather than any of the ones they passed through.
	final := LoungeACInput{Desired: loungeACSetting(acStateCool, 22), Actual: actual}
	lastChange := loungeACT0.Add(time.Duration(len(steps)-1) * time.Second)
	var after []LoungeACAction
	for at := time.Duration(0); at <= loungeACFallbackAt; at += time.Second {
		after = append(after, EvaluateLoungeAC(state, final, lastChange.Add(at))...)
	}

	assert.Equal(t, []LoungeACAction{
		ActionLoungeACRefreshWiFi,
		ActionLoungeACResendIR,
		ActionLoungeACRefreshWiFi,
		ActionLoungeACFallbackWiFi,
	}, after)
	assert.Equal(t, loungeACSetting(acStateCool, 22), state.desired)
}

// covers: LAC-MATCH-1, LAC-MATCH-2, LAC-MATCH-3
func TestLoungeACMatches(t *testing.T) {
	tests := []struct {
		name    string
		desired LoungeACSetting
		actual  LoungeACSetting
		want    bool
	}{
		{"exact", loungeACSetting(acStateCool, 20), loungeACSetting(acStateCool, 20), true},
		{"mode differs", loungeACSetting(acStateCool, 20), loungeACSetting(acStateHeat, 20), false},
		// The IR entity steps in 0.5C and the WiFi entity in 1.0C, so a half-degree
		// gap is the two entities describing the same setpoint.
		{"half degree below", loungeACSetting(acStateCool, 20.5), loungeACSetting(acStateCool, 20), true},
		{"half degree above", loungeACSetting(acStateCool, 20.5), loungeACSetting(acStateCool, 21), true},
		{"one degree out", loungeACSetting(acStateCool, 20), loungeACSetting(acStateCool, 21), false},
		{"far out", loungeACSetting(acStateCool, 18), loungeACSetting(acStateCool, 24), false},
		// Off has no meaningful setpoint, so the mode settles it.
		{"off matches off", LoungeACSetting{Mode: hvacModeOff}, LoungeACSetting{Mode: hvacModeOff}, true},
		{"off vs cool", LoungeACSetting{Mode: hvacModeOff}, loungeACSetting(acStateCool, 20), false},
		{"missing actual temp", loungeACSetting(acStateCool, 20), LoungeACSetting{Mode: acStateCool}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, loungeACMatches(tt.desired, tt.actual))
		})
	}
}

// decodeCall reads one queued service call off the channel.
func decodeCall(t *testing.T, ch <-chan MQTTMessage) (service string, data map[string]any) {
	t.Helper()
	msg := <-ch
	assert.Equal(t, TopicCallServiceProxy, msg.Topic)

	var payload map[string]any
	assert.NoError(t, json.Unmarshal(msg.Payload, &payload))
	assert.Equal(t, "climate", payload["domain"])

	service, _ = payload["service"].(string)
	data, _ = payload["data"].(map[string]any)
	return service, data
}

// covers: LAC-SEND-1
func TestCommandLoungeACWiFi_SetsModeThenFlooredSetpoint(t *testing.T) {
	ch := make(chan MQTTMessage, 4)
	sender := NewMQTTSender(ch)

	commandLoungeACWiFi(sender, loungeACSetting(acStateCool, 20.5))

	// Mode goes as its own call: set_temperature honouring an hvac_mode kwarg is the
	// integration's choice, and a fallback that moved only the setpoint would be worse
	// than no fallback at all.
	service, data := decodeCall(t, ch)
	assert.Equal(t, "set_hvac_mode", service)
	assert.Equal(t, acStateCool, data["hvac_mode"])

	service, data = decodeCall(t, ch)
	assert.Equal(t, "set_temperature", service)
	// Cooler is preferred, so 20.5 floors to 20 rather than rounding to 21.
	assert.Equal(t, 20.0, data["temperature"])
}

// covers: LAC-REFRESH-1
func TestRefreshLoungeACWiFi_PollsWithoutCommanding(t *testing.T) {
	ch := make(chan MQTTMessage, 4)
	sender := NewMQTTSender(ch)

	refreshLoungeACWiFi(sender)

	msg := <-ch
	var payload map[string]any
	assert.NoError(t, json.Unmarshal(msg.Payload, &payload))
	// Not a climate service: this drives the integration's own update, so it must not
	// carry anything that could change what the unit is doing.
	assert.Equal(t, "homeassistant", payload["domain"])
	assert.Equal(t, "update_entity", payload["service"])
	assert.Equal(t, loungeACWiFiEntityID, payload["entity_id"])
	assert.Nil(t, payload["data"])
	assert.Empty(t, ch)
}

// covers: LAC-RESEND-1
func TestResendLoungeACIR_SendsWholeStateInOneCall(t *testing.T) {
	ch := make(chan MQTTMessage, 4)
	sender := NewMQTTSender(ch)

	resendLoungeACIR(sender, loungeACSetting(acStateCool, 20.5))

	// One frame, not two: every IR frame is a dozen Zigbee round trips. And the IR
	// entity handles 0.5C steps, so a re-send must not degrade the setpoint.
	service, data := decodeCall(t, ch)
	assert.Equal(t, "set_temperature", service)
	assert.Equal(t, 20.5, data["temperature"])
	assert.Equal(t, acStateCool, data["hvac_mode"])
	assert.Empty(t, ch)
}

// covers: LAC-MATCH-3
func TestCommandLoungeACWiFi_OffSendsModeOnly(t *testing.T) {
	ch := make(chan MQTTMessage, 4)
	sender := NewMQTTSender(ch)

	commandLoungeACWiFi(sender, LoungeACSetting{Mode: hvacModeOff})

	service, data := decodeCall(t, ch)
	assert.Equal(t, "set_hvac_mode", service)
	assert.Equal(t, hvacModeOff, data["hvac_mode"])
	assert.Empty(t, ch, "a setpoint is meaningless with the unit off")
}

// covers: LAC-TRIGGER-3
func TestEvaluateLoungeAC_SetpointChangeWhileOffIsNotACommand(t *testing.T) {
	off := DisplayData{TopicData: map[string]any{
		TopicLoungeACIRState: &StringTopicData{Current: hvacModeOff},
		TopicLoungeACIRTemp:  &FloatTopicData{Current: 20},
		TopicLoungeACState:   &StringTopicData{Current: hvacModeOff},
		TopicLoungeACTemp:    &FloatTopicData{Current: 20},
	}}
	state := &LoungeACState{}
	assert.Empty(t, EvaluateLoungeAC(state, ExtractLoungeACInput(off), loungeACT0))

	// Nudging the dial while the unit is off must not fire an IR frame to turn off an
	// already-off unit.
	nudged := DisplayData{TopicData: map[string]any{
		TopicLoungeACIRState: &StringTopicData{Current: hvacModeOff},
		TopicLoungeACIRTemp:  &FloatTopicData{Current: 24},
		TopicLoungeACState:   &StringTopicData{Current: hvacModeOff},
		TopicLoungeACTemp:    &FloatTopicData{Current: 20},
	}}
	for i := range 120 {
		at := loungeACT0.Add(time.Duration(i) * time.Second)
		assert.Empty(t, EvaluateLoungeAC(state, ExtractLoungeACInput(nudged), at))
	}
}

// covers: LAC-VALID-1
func TestEvaluateLoungeAC_InvalidDesiredNeverActs(t *testing.T) {
	for _, desired := range []LoungeACSetting{
		{Mode: hvacModeUnknown}, // the pre-seeded sentinel
		{Mode: ""},              // nothing published yet
		{Mode: acStateCool},     // a mode that needs a setpoint, without one
	} {
		state := seededLoungeACState(loungeACSetting(acStateCool, 21))
		in := LoungeACInput{Desired: desired, Actual: loungeACSetting(acStateHeat, 24)}

		assert.Empty(t, tickLoungeAC(state, in, loungeACFallbackAt*2),
			"desired %v should never act", desired)
	}
}

// covers: LAC-VALID-2
func TestEvaluateLoungeAC_UnusableActualSkipsFallback(t *testing.T) {
	desired := loungeACSetting(acStateCool, 18)
	state := seededLoungeACState(loungeACSetting(acStateCool, 21))
	// The WiFi entity has told us nothing, so there is no evidence IR missed.
	in := LoungeACInput{Desired: desired, Actual: LoungeACSetting{Mode: ""}}

	// The IR half still runs — it costs the unit nothing to be told twice — but taking
	// the unit over on no evidence does not.
	assert.Equal(t, []LoungeACAction{
		ActionLoungeACRefreshWiFi,
		ActionLoungeACResendIR,
		ActionLoungeACRefreshWiFi,
	}, tickLoungeAC(state, in, loungeACFallbackAt+time.Second))
}

// covers: LAC-VALID-3
func TestExtractLoungeACInput_SentinelsReadAsInvalid(t *testing.T) {
	data := DisplayData{TopicData: map[string]any{}}
	for _, seed := range preSeededTopics {
		switch seed.Topic {
		case TopicLoungeACIRState:
			data.TopicData[seed.Topic] = &StringTopicData{Current: seed.Value}
		case TopicLoungeACIRTemp, TopicLoungeACTemp:
			var f FloatTopicData
			assert.NoError(t, json.Unmarshal([]byte(seed.Value), &f.Current))
			data.TopicData[seed.Topic] = &f
		}
	}

	in := ExtractLoungeACInput(data)
	assert.False(t, in.Desired.Valid(), "seeded desired must not look like a command")
	assert.False(t, in.Actual.Valid(), "seeded actual must not look like a confirmation")
}

// covers: LAC-VALID-1
func TestReadLoungeACSetting_RejectsImplausibleSetpoint(t *testing.T) {
	// `None` while the unit is off parses as a zero float, not as an error.
	data := DisplayData{TopicData: map[string]any{
		TopicLoungeACIRState: &StringTopicData{Current: "cool"},
		TopicLoungeACIRTemp:  &FloatTopicData{Current: 0},
	}}

	got := readLoungeACSetting(data, TopicLoungeACIRState, TopicLoungeACIRTemp)
	assert.False(t, got.HasTemp)
	assert.Zero(t, got.Temp, "a rejected setpoint must not be carried, or it looks like a change")
}

// covers: LAC-BOUND-1
func TestEvaluateLoungeAC_OneChangeYieldsAtMostOneOfEachAction(t *testing.T) {
	desired := loungeACSetting(acStateCool, 18)
	state := seededLoungeACState(loungeACSetting(acStateCool, 21))
	in := LoungeACInput{Desired: desired, Actual: loungeACSetting(acStateHeat, 24)}

	// Tick once a second for minutes past the last step, as the broadcast worker does.
	// The unit never agrees, so nothing here stops the schedule early: what bounds it is
	// the schedule being finite.
	got := tickLoungeAC(state, in, 5*time.Minute)

	assert.Equal(t, []LoungeACAction{
		ActionLoungeACRefreshWiFi,
		ActionLoungeACResendIR,
		ActionLoungeACRefreshWiFi,
		ActionLoungeACFallbackWiFi,
	}, got)
}
