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

// seededLoungeACState returns a state that has already taken desired as its baseline,
// as it would be after the first evaluation following startup.
func seededLoungeACState(desired LoungeACSetting) *LoungeACState {
	return &LoungeACState{seeded: true, desired: desired, phase: loungeACIdle}
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

// covers: LAC-TRIGGER-1, LAC-RESEND-1
func TestEvaluateLoungeAC_DesiredChangeResendsOnce(t *testing.T) {
	state := seededLoungeACState(loungeACSetting(acStateCool, 21))
	in := LoungeACInput{
		Desired: loungeACSetting(acStateCool, 18),
		Actual:  loungeACSetting(acStateHeat, 24),
	}

	// The change itself doesn't send: the first transfer needs to finish first.
	assert.Empty(t, EvaluateLoungeAC(state, in, loungeACT0))
	assert.Empty(t, EvaluateLoungeAC(state, in, loungeACT0.Add(time.Second)))

	at := loungeACT0.Add(loungeACResendDelay)
	assert.Equal(t, []LoungeACAction{ActionLoungeACResendIR}, EvaluateLoungeAC(state, in, at))

	// Exactly once.
	assert.Empty(t, EvaluateLoungeAC(state, in, at.Add(time.Second)))
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
		assert.Empty(t, EvaluateLoungeAC(state, in, loungeACT0.Add(loungeACConfirmTimeout*2)),
			"actual %v should not trigger anything", actual)
	}
}

// covers: LAC-CONFIRM-1
func TestEvaluateLoungeAC_ConfirmsAsSoonAsUnitAgrees(t *testing.T) {
	desired := loungeACSetting(acStateCool, 18)
	state := seededLoungeACState(loungeACSetting(acStateCool, 21))

	miss := LoungeACInput{Desired: desired, Actual: loungeACSetting(acStateHeat, 24)}
	EvaluateLoungeAC(state, miss, loungeACT0)
	EvaluateLoungeAC(state, miss, loungeACT0.Add(loungeACResendDelay))

	// The unit reports the new state well before the timeout would expire.
	hit := LoungeACInput{Desired: desired, Actual: loungeACSetting(acStateCool, 18)}
	assert.Empty(t, EvaluateLoungeAC(state, hit, loungeACT0.Add(30*time.Second)))
	assert.Equal(t, loungeACIdle, state.phase)

	// And the timeout passing later must not resurrect a fallback.
	assert.Empty(t, EvaluateLoungeAC(state, hit, loungeACT0.Add(loungeACConfirmTimeout*2)))
}

// covers: LAC-CONFIRM-2, LAC-FALLBACK-1, LAC-TERMINAL-1
func TestEvaluateLoungeAC_FallsBackToWiFiThenStops(t *testing.T) {
	desired := loungeACSetting(acStateCool, 18)
	state := seededLoungeACState(loungeACSetting(acStateCool, 21))
	in := LoungeACInput{Desired: desired, Actual: loungeACSetting(acStateHeat, 24)}

	EvaluateLoungeAC(state, in, loungeACT0)
	resendAt := loungeACT0.Add(loungeACResendDelay)
	assert.Equal(t, []LoungeACAction{ActionLoungeACResendIR}, EvaluateLoungeAC(state, in, resendAt))

	// Still wrong just before the deadline: hold.
	assert.Empty(t, EvaluateLoungeAC(state, in, resendAt.Add(loungeACConfirmTimeout-time.Second)))

	deadline := resendAt.Add(loungeACConfirmTimeout)
	assert.Equal(t, []LoungeACAction{ActionLoungeACFallbackWiFi}, EvaluateLoungeAC(state, in, deadline))

	// Terminal: the WiFi path is the last escalation, so nothing more happens.
	assert.Empty(t, EvaluateLoungeAC(state, in, deadline.Add(time.Hour)))
	assert.Equal(t, loungeACIdle, state.phase)
}

// covers: LAC-SUPERSEDE-1
func TestEvaluateLoungeAC_NewCommandRestartsSequence(t *testing.T) {
	state := seededLoungeACState(loungeACSetting(acStateCool, 21))
	actual := loungeACSetting(acStateHeat, 24)

	first := LoungeACInput{Desired: loungeACSetting(acStateCool, 18), Actual: actual}
	EvaluateLoungeAC(state, first, loungeACT0)
	EvaluateLoungeAC(state, first, loungeACT0.Add(loungeACResendDelay))

	// A new command mid-flight abandons the old sequence.
	second := LoungeACInput{Desired: loungeACSetting(acStateCool, 19), Actual: actual}
	changedAt := loungeACT0.Add(20 * time.Second)
	assert.Empty(t, EvaluateLoungeAC(state, second, changedAt))
	assert.Equal(t, loungeACAwaitingResend, state.phase)

	// The old sequence's deadline must not fire a fallback for the superseded value.
	assert.Empty(t, EvaluateLoungeAC(state, second, changedAt.Add(time.Second)))
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

	// Then it settles: one re-send, five seconds after the final change, carrying the
	// value the user landed on rather than any of the ones they passed through.
	final := LoungeACInput{Desired: loungeACSetting(acStateCool, 22), Actual: actual}
	lastChange := loungeACT0.Add(time.Duration(len(steps)-1) * time.Second)
	var after []LoungeACAction
	for i := range 11 {
		after = append(after, EvaluateLoungeAC(state, final, lastChange.Add(time.Duration(i)*time.Second))...)
	}

	assert.Equal(t, []LoungeACAction{ActionLoungeACResendIR}, after)
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
		{Mode: hvacModeUnknown},          // the pre-seeded sentinel
		{Mode: ""},                       // nothing published yet
		{Mode: acStateCool},                   // a mode that needs a setpoint, without one
	} {
		state := seededLoungeACState(loungeACSetting(acStateCool, 21))
		in := LoungeACInput{Desired: desired, Actual: loungeACSetting(acStateHeat, 24)}

		assert.Empty(t, EvaluateLoungeAC(state, in, loungeACT0))
		assert.Empty(t, EvaluateLoungeAC(state, in, loungeACT0.Add(loungeACConfirmTimeout*2)),
			"desired %v should never act", desired)
	}
}

// covers: LAC-VALID-2
func TestEvaluateLoungeAC_UnusableActualSkipsFallback(t *testing.T) {
	desired := loungeACSetting(acStateCool, 18)
	state := seededLoungeACState(loungeACSetting(acStateCool, 21))
	// The WiFi entity has told us nothing, so there is no evidence IR missed.
	in := LoungeACInput{Desired: desired, Actual: LoungeACSetting{Mode: ""}}

	EvaluateLoungeAC(state, in, loungeACT0)
	resendAt := loungeACT0.Add(loungeACResendDelay)
	assert.Equal(t, []LoungeACAction{ActionLoungeACResendIR}, EvaluateLoungeAC(state, in, resendAt))
	assert.Empty(t, EvaluateLoungeAC(state, in, resendAt.Add(loungeACConfirmTimeout)))
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
		TopicLoungeACIRTemp: &FloatTopicData{Current: 0},
	}}

	got := readLoungeACSetting(data, TopicLoungeACIRState, TopicLoungeACIRTemp)
	assert.False(t, got.HasTemp)
	assert.Zero(t, got.Temp, "a rejected setpoint must not be carried, or it looks like a change")
}

// covers: LAC-BOUND-1
func TestEvaluateLoungeAC_OneChangeYieldsAtMostTwoCommands(t *testing.T) {
	desired := loungeACSetting(acStateCool, 18)
	state := seededLoungeACState(loungeACSetting(acStateCool, 21))
	in := LoungeACInput{Desired: desired, Actual: loungeACSetting(acStateHeat, 24)}

	// Tick once a second for well past the deadline, as the broadcast worker does.
	var got []LoungeACAction
	for i := 0; i < int((loungeACConfirmTimeout+time.Minute)/time.Second); i++ {
		got = append(got, EvaluateLoungeAC(state, in, loungeACT0.Add(time.Duration(i)*time.Second))...)
	}

	assert.Equal(t, []LoungeACAction{ActionLoungeACResendIR, ActionLoungeACFallbackWiFi}, got)
}
