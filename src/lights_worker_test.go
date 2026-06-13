package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// lightAt returns a test time in June 2026 at the given clock time (UTC).
func lightAt(hour, minute, second int) time.Time {
	return time.Date(2026, time.June, 11, hour, minute, second, 0, time.UTC)
}

// seed advances the controller past its first-snapshot bookkeeping with the
// given steady state so subsequent calls see real transitions.
func seed(state *LightControlState, in LightInput, now time.Time) {
	EvaluateLights(state, in, now)
}

// covers: LIGHT-OUT-1, LIGHT-OUT-3
func TestEvaluateLights_OutsideAutoOffAfterDelayWhenDark(t *testing.T) {
	state := &LightControlState{}
	dark := LightInput{SunBelow: true}
	seed(state, dark, lightAt(20, 0, 0))

	// Outside turns on while dark: armed, not yet off (the off→on edge does
	// mirror the garage, so assert specifically on the outside-off action).
	on := LightInput{OutsideOn: true, SunBelow: true}
	assert.NotContains(t, EvaluateLights(state, on, lightAt(20, 0, 1)), ActionOutsideOff)
	assert.NotContains(t, EvaluateLights(state, on, lightAt(20, 9, 0)), ActionOutsideOff,
		"still within 10 min")

	assert.Contains(t, EvaluateLights(state, on, lightAt(20, 10, 1)), ActionOutsideOff)
}

// covers: LIGHT-OUT-2
func TestEvaluateLights_OutsideOffImmediatelyDuringDay(t *testing.T) {
	state := &LightControlState{}
	day := LightInput{SunBelow: false}
	seed(state, day, lightAt(12, 0, 0))

	on := LightInput{OutsideOn: true, SunBelow: false}
	assert.Contains(t, EvaluateLights(state, on, lightAt(12, 0, 1)), ActionOutsideOff)
}

// covers: LIGHT-OUT-3 (already on at startup is left alone)
func TestEvaluateLights_OutsideOnAtStartupNotAutoOff(t *testing.T) {
	state := &LightControlState{}
	on := LightInput{OutsideOn: true, SunBelow: true, GarageOn: true}
	// First snapshot with outside already on (garage already matching): no
	// auto-off armed, no garage sync needed.
	assert.Empty(t, EvaluateLights(state, on, lightAt(20, 0, 0)))
	assert.NotContains(t, EvaluateLights(state, on, lightAt(23, 0, 0)), ActionOutsideOff,
		"hours later still left alone")
}

// covers: LIGHT-KIT-1, LIGHT-KIT-2
func TestEvaluateLights_KitchenFollowsMumAtNight(t *testing.T) {
	state := &LightControlState{}
	base := LightInput{SunBelow: true}
	seed(state, base, lightAt(20, 0, 0))

	mumOn := LightInput{MumOn: true, SunBelow: true}
	assert.Equal(t, []LightAction{ActionKitchenOn},
		EvaluateLights(state, mumOn, lightAt(20, 0, 1)))

	// Mum off → kitchen off (powerctl owns it).
	mumOff := LightInput{MumOn: false, KitchenOn: true, SunBelow: true}
	assert.Equal(t, []LightAction{ActionKitchenOff},
		EvaluateLights(state, mumOff, lightAt(20, 5, 0)))
}

// covers: LIGHT-KIT-2 (don't auto-off a kitchen powerctl didn't turn on)
func TestEvaluateLights_KitchenNotOffWhenNotManaged(t *testing.T) {
	state := &LightControlState{}
	// Kitchen already on (someone else); mum on at night → no kitchen-on action.
	base := LightInput{SunBelow: true, KitchenOn: true}
	seed(state, base, lightAt(20, 0, 0))

	mumOn := LightInput{MumOn: true, KitchenOn: true, SunBelow: true}
	assert.Empty(t, EvaluateLights(state, mumOn, lightAt(20, 0, 1)))

	mumOff := LightInput{MumOn: false, KitchenOn: true, SunBelow: true}
	assert.Empty(t, EvaluateLights(state, mumOff, lightAt(20, 1, 0)),
		"kitchen wasn't powerctl-managed, leave it")
}

// covers: LIGHT-KIT-3 (outside the night window, do nothing)
func TestEvaluateLights_KitchenIgnoredOutsideNightWindow(t *testing.T) {
	state := &LightControlState{}
	day := LightInput{SunBelow: false}
	seed(state, day, lightAt(14, 0, 0))

	mumOn := LightInput{MumOn: true, SunBelow: false}
	assert.Empty(t, EvaluateLights(state, mumOn, lightAt(14, 0, 1)),
		"2pm in daylight is not the night window")
}

// covers: LIGHT-KIT-3 (19:30 boundary)
func TestIsKitchenNightWindow_Boundary(t *testing.T) {
	assert.False(t, isKitchenNightWindow(lightAt(19, 29, 0), false))
	assert.True(t, isKitchenNightWindow(lightAt(19, 30, 0), false))
	assert.True(t, isKitchenNightWindow(lightAt(6, 0, 0), true), "dark before noon")
	assert.False(t, isKitchenNightWindow(lightAt(13, 0, 0), true), "dark but afternoon")
}

// covers: LIGHT-GAR-1, LIGHT-GAR-3
func TestEvaluateLights_GarageMirrorsOutside(t *testing.T) {
	state := &LightControlState{}
	off := LightInput{}
	seed(state, off, lightAt(12, 0, 0))

	on := LightInput{OutsideOn: true, SunBelow: false}
	acts := EvaluateLights(state, on, lightAt(12, 0, 1))
	assert.Contains(t, acts, ActionGarageOn)

	// Steady on: no repeat garage command.
	assert.NotContains(t, EvaluateLights(state, on, lightAt(12, 0, 2)), ActionGarageOn)
}

// covers: LIGHT-GAR-2 (startup sync)
func TestEvaluateLights_GarageSyncsOnStartup(t *testing.T) {
	state := &LightControlState{}
	// Outside on, garage off at startup → sync garage on.
	in := LightInput{OutsideOn: true, GarageOn: false, SunBelow: true}
	assert.Contains(t, EvaluateLights(state, in, lightAt(20, 0, 0)), ActionGarageOn)
}

// ryanLit returns Ryan's lights on at the given brightness percent.
func ryanLit(pct float64) LightInput {
	return LightInput{RyansOn: true, RyansBrightnessPct: pct}
}

// covers: LIGHT-DIM-1, LIGHT-DIM-2 (ramp waits for the light, then follows down)
func TestEvaluateLights_SleepDimRampCatchesUp(t *testing.T) {
	state := &LightControlState{}
	at80 := ryanLit(80)
	seed(state, at80, lightAt(20, 0, 0))

	press := LightInput{RyansOn: true, RyansBrightnessPct: 80, SleepRyanPressed: true}
	// At press the ramp target is ~100% > 80%, so the light is left alone.
	assert.Empty(t, EvaluateLights(state, press, lightAt(20, 0, 1)))
	// 3 min in: target = 100*(1-3/30) = 90% > 80%, still left alone.
	assert.Empty(t, EvaluateLights(state, at80, lightAt(20, 3, 0)))
	// 9 min in: target = 100*(1-9/30) = 70% < 80%, ramp has caught up → command 70%.
	assert.Equal(t, []LightAction{ActionSetRyansBrightness},
		EvaluateLights(state, at80, lightAt(20, 9, 0)))
	assert.Equal(t, 70, state.dimReqPct)
}

// covers: LIGHT-DIM-1 (a full-brightness light follows the ramp from the start)
func TestEvaluateLights_SleepDimFromFull(t *testing.T) {
	state := &LightControlState{}
	full := ryanLit(100)
	seed(state, full, lightAt(20, 0, 0))

	press := LightInput{RyansOn: true, RyansBrightnessPct: 100, SleepRyanPressed: true}
	EvaluateLights(state, press, lightAt(20, 0, 1))
	// 6 min in: target = 80%.
	assert.Equal(t, []LightAction{ActionSetRyansBrightness},
		EvaluateLights(state, full, lightAt(20, 6, 0)))
	assert.Equal(t, 80, state.dimReqPct)
}

// covers: LIGHT-DIM-3 (turns off at the 30-min end)
func TestEvaluateLights_SleepDimEndsOff(t *testing.T) {
	state := &LightControlState{}
	seed(state, ryanLit(100), lightAt(20, 0, 0))

	press := LightInput{RyansOn: true, RyansBrightnessPct: 100, SleepRyanPressed: true}
	EvaluateLights(state, press, lightAt(20, 0, 1))

	// Just before 30 min, still ramping (light hasn't been manually dimmed).
	assert.Equal(t, []LightAction{ActionSetRyansBrightness},
		EvaluateLights(state, ryanLit(100), lightAt(20, 29, 0)))
	// At/after 30 min, turn off.
	assert.Equal(t, []LightAction{ActionRyansOff},
		EvaluateLights(state, ryanLit(100), lightAt(20, 30, 1)))
}

// covers: LIGHT-DIM-4 (stops when the light is switched off)
func TestEvaluateLights_SleepDimStopsWhenOff(t *testing.T) {
	state := &LightControlState{}
	seed(state, ryanLit(100), lightAt(20, 0, 0))

	press := LightInput{RyansOn: true, RyansBrightnessPct: 100, SleepRyanPressed: true}
	EvaluateLights(state, press, lightAt(20, 0, 1))

	off := LightInput{RyansOn: false}
	assert.Empty(t, EvaluateLights(state, off, lightAt(20, 5, 0)))
	assert.Empty(t, EvaluateLights(state, ryanLit(100), lightAt(20, 10, 0)),
		"dim is over; light coming back on does not resume it")
}

// covers: LIGHT-DIM-6 (a manual brightness increase re-anchors the ramp)
func TestEvaluateLights_SleepDimReanchorsOnIncrease(t *testing.T) {
	state := &LightControlState{}
	seed(state, ryanLit(100), lightAt(20, 0, 0))

	press := LightInput{RyansOn: true, RyansBrightnessPct: 100, SleepRyanPressed: true}
	EvaluateLights(state, press, lightAt(20, 0, 1))

	// 18 min in the light has tracked down to ~40% (no manual change).
	EvaluateLights(state, ryanLit(40), lightAt(20, 18, 0))

	// User bumps brightness back to 100% at 19 min: re-anchor, don't yank down.
	acts := EvaluateLights(state, ryanLit(100), lightAt(20, 19, 0))
	assert.NotContains(t, acts, ActionSetRyansBrightness)
	// Full brightness → ramp origin moves to ~now (a fresh ~30 min ahead).
	assert.WithinDuration(t, lightAt(20, 19, 0), state.dimStart, time.Second)

	// 6 min after the re-anchor, the target is 80% again and it resumes dimming.
	assert.Equal(t, []LightAction{ActionSetRyansBrightness},
		EvaluateLights(state, ryanLit(100), lightAt(20, 25, 0)))
	assert.Equal(t, 80, state.dimReqPct)
}

// covers: LIGHT-DIM-5 (restart resets the 30-min ramp origin)
func TestEvaluateLights_SleepDimRestart(t *testing.T) {
	state := &LightControlState{}
	full := ryanLit(100)
	seed(state, full, lightAt(20, 0, 0))

	press := LightInput{RyansOn: true, RyansBrightnessPct: 100, SleepRyanPressed: true}
	EvaluateLights(state, press, lightAt(20, 0, 1))
	// Ramp down to ~50% at 15 min in.
	EvaluateLights(state, full, lightAt(20, 15, 0))
	assert.Equal(t, 50, state.dimReqPct)

	// A second press restarts: 6 min after the new origin → target 80% again.
	EvaluateLights(state, press, lightAt(21, 0, 0))
	assert.Equal(t, []LightAction{ActionSetRyansBrightness},
		EvaluateLights(state, full, lightAt(21, 6, 0)))
	assert.Equal(t, 80, state.dimReqPct)
}
