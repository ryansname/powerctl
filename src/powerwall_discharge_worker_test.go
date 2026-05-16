package main

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

const (
	testSourcePeak   = "peak-power"
	testReasonSOC92  = "SOC 92%"
)

// covers: DISCHARGE-USER-3
func TestDecideDischarge_UserForceOnWinsOverAllVotes(t *testing.T) {
	votes := map[string]DischargeRequest{
		powerCutVoteSource: {Source: powerCutVoteSource, Want: VoteOff, Reason: "low SOC"},
		testSourcePeak:     {Source: testSourcePeak, Want: VoteOn, Reason: "peak window"},
	}
	intent, reason := decideDischarge(PW2DischargeModeForceOn, votes)
	assert.Equal(t, IntentOn, intent)
	assert.Equal(t, "user-force-on", reason)
}

// covers: DISCHARGE-USER-4
func TestDecideDischarge_UserForceOffWinsOverAllVotes(t *testing.T) {
	votes := map[string]DischargeRequest{
		powerCutVoteSource: {Source: powerCutVoteSource, Want: VoteOn, Reason: testReasonSOC92},
	}
	intent, reason := decideDischarge(PW2DischargeModeForceOff, votes)
	assert.Equal(t, IntentOff, intent)
	assert.Equal(t, "user-force-off", reason)
}

// covers: DISCHARGE-USER-5, DISCHARGE-PASSIVE-1 (decision side)
func TestDecideDischarge_AutoNoVotesIsPassive(t *testing.T) {
	intent, reason := decideDischarge(PW2DischargeModeAuto, map[string]DischargeRequest{})
	assert.Equal(t, IntentPassive, intent,
		"Auto with no votes must be Passive so Tesla-app manual changes are respected")
	assert.Equal(t, "passive", reason)
}

// covers: DISCHARGE-AUTO-2
func TestDecideDischarge_AutoSingleOnVote(t *testing.T) {
	votes := map[string]DischargeRequest{
		powerCutVoteSource: {Source: powerCutVoteSource, Want: VoteOn, Reason: testReasonSOC92},
	}
	intent, reason := decideDischarge(PW2DischargeModeAuto, votes)
	assert.Equal(t, IntentOn, intent)
	assert.Contains(t, reason, powerCutVoteSource)
	assert.Contains(t, reason, testReasonSOC92)
}

// covers: DISCHARGE-AUTO-1
func TestDecideDischarge_AutoVetoBeatsOn(t *testing.T) {
	votes := map[string]DischargeRequest{
		powerCutVoteSource: {Source: powerCutVoteSource, Want: VoteOn, Reason: testReasonSOC92},
		"low-batt":         {Source: "low-batt", Want: VoteOff, Reason: "B3 below 30%"},
		testSourcePeak:     {Source: testSourcePeak, Want: VoteOn, Reason: "evening peak"},
	}
	intent, reason := decideDischarge(PW2DischargeModeAuto, votes)
	assert.Equal(t, IntentOff, intent)
	assert.Contains(t, reason, "low-batt")
	assert.Contains(t, reason, "veto")
}

func TestDecideDischarge_AutoNoOpinionIgnored(t *testing.T) {
	votes := map[string]DischargeRequest{
		powerCutVoteSource: {Source: powerCutVoteSource, Want: VoteNoOpinion, Reason: "armed, SOC too low"},
		testSourcePeak:     {Source: testSourcePeak, Want: VoteOn, Reason: "peak window"},
	}
	intent, reason := decideDischarge(PW2DischargeModeAuto, votes)
	assert.Equal(t, IntentOn, intent)
	assert.True(t, strings.HasPrefix(reason, "peak-power:"))
}

// covers: DISCHARGE-USER-2 (empty/unknown user mode falls back to default Auto)
func TestDecideDischarge_EmptyModeTreatedAsAuto(t *testing.T) {
	votes := map[string]DischargeRequest{
		powerCutVoteSource: {Source: powerCutVoteSource, Want: VoteOn, Reason: testReasonSOC92},
	}
	intent, _ := decideDischarge("", votes)
	assert.Equal(t, IntentOn, intent)
}

func TestDecideDischarge_EmptyModeNoVotesIsPassive(t *testing.T) {
	intent, _ := decideDischarge("", map[string]DischargeRequest{})
	assert.Equal(t, IntentPassive, intent)
}

// covers: DISCHARGE-RECON-2 (no spam when state already matches)
func TestReconcileDischarge_NoChangeWhenDesiredMatchesActual(t *testing.T) {
	now := time.Now()
	send := reconcileDischarge(true, true, true, now.Add(-time.Hour), now)
	assert.False(t, send)
}

// covers: DISCHARGE-RECON-3
func TestReconcileDischarge_FiresImmediatelyOnIntentChange(t *testing.T) {
	now := time.Now()
	send := reconcileDischarge(false, true, true, now.Add(-5*time.Second), now)
	assert.True(t, send, "intent change must fire immediately, not wait for propagation window")
}

// covers: DISCHARGE-RECON-2
func TestReconcileDischarge_SuppressedDuringPropagationWindow(t *testing.T) {
	now := time.Now()
	send := reconcileDischarge(true, false, true, now.Add(-10*time.Second), now)
	assert.False(t, send, "same intent within propagation window must be suppressed")
}

// covers: DISCHARGE-RECON-4
func TestReconcileDischarge_RetriesAfterPropagationWindow(t *testing.T) {
	now := time.Now()
	send := reconcileDischarge(true, false, true, now.Add(-35*time.Second), now)
	assert.True(t, send, "stale mismatch beyond propagation window must retry")
}

// covers: DISCHARGE-RECON-1
func TestReconcileDischarge_ColdStartFiresWhenMismatched(t *testing.T) {
	send := reconcileDischarge(true, false, false, time.Time{}, time.Now())
	assert.True(t, send)
}

// octopusSellRate looks up the sell rate from buildOctopusTariff for a weekday at the given month and hour.
func octopusSellRate(t *testing.T, month, hour int) float64 {
	t.Helper()
	tariff := buildOctopusTariff()
	sell, ok := tariff["sell_tariff"].(map[string]any)
	if !ok {
		t.Fatal("missing sell_tariff")
	}
	seasons, ok := sell["seasons"].(map[string]any)
	if !ok {
		t.Fatal("missing seasons")
	}
	energyCharges, ok := sell["energy_charges"].(map[string]any)
	if !ok {
		t.Fatal("missing energy_charges")
	}

	var seasonName string
	for name, s := range seasons {
		sm, ok := s.(map[string]any)
		if !ok {
			continue
		}
		fromMonth, ok1 := sm["fromMonth"].(int)
		toMonth, ok2 := sm["toMonth"].(int)
		if !ok1 || !ok2 {
			continue
		}
		var inSeason bool
		if fromMonth <= toMonth {
			inSeason = month >= fromMonth && month <= toMonth
		} else {
			// Wrapping season (e.g., Oct–Apr: fromMonth=10 > toMonth=4)
			inSeason = month >= fromMonth || month <= toMonth
		}
		if inSeason {
			seasonName = name
			break
		}
	}
	if seasonName == "" {
		t.Fatalf("no season for month %d", month)
	}

	season, ok := seasons[seasonName].(map[string]any)
	if !ok {
		t.Fatalf("invalid season %s", seasonName)
	}
	touPeriods, ok := season["tou_periods"].(map[string]any)
	if !ok {
		t.Fatalf("missing tou_periods in season %s", seasonName)
	}
	band := bandSuperOffPeak
outer:
	for _, bandName := range []string{bandOnPeak, bandPartialPeak, bandOffPeak} {
		bm, ok := touPeriods[bandName].(map[string]any)
		if !ok {
			continue
		}
		periods, ok := bm["periods"].([]any)
		if !ok {
			continue
		}
		for _, p := range periods {
			pm, ok := p.(map[string]any)
			if !ok {
				continue
			}
			fromHour, ok1 := pm["fromHour"].(int)
			toHour, ok2 := pm["toHour"].(int)
			fromDow, hasDow1 := pm["fromDayOfWeek"].(int)
			toDow, hasDow2 := pm["toDayOfWeek"].(int)
			// Test on Monday (1); skip weekend-only periods.
			if hasDow1 && hasDow2 && (1 < fromDow || 1 > toDow) {
				continue
			}
			if ok1 && ok2 && hour >= fromHour && hour < toHour {
				band = bandName
				break outer
			}
		}
	}

	sc, ok := energyCharges[seasonName].(map[string]any)
	if !ok {
		t.Fatalf("missing energy_charges for season %s", seasonName)
	}
	rates, ok := sc["rates"].(map[string]any)
	if !ok {
		t.Fatalf("missing rates for season %s", seasonName)
	}
	rateVal, ok := rates[band]
	if !ok {
		t.Fatalf("no rate for band %s in season %s (month %d hour %d)", band, seasonName, month, hour)
	}
	rate, ok := rateVal.(float64)
	if !ok {
		t.Fatalf("non-float rate for band %s in season %s", band, seasonName)
	}
	return rate
}

func TestOctopusSellTariff(t *testing.T) {
	// =========================================================================
	// THE NUMBERS IN THE TABLE BELOW ARE THE CONTRACT.
	//
	// They are the actual Octopus / Vector sell-back rates for this site and
	// must NOT be edited to make a failing test pass. If a rate here looks
	// wrong, the bug is in buildOctopusTariff (or in our band/period mapping),
	// not in this table. Fix the producer, not the oracle.
	//
	// The 21:00 ("lateEvening") column for the rebate seasons (May–Sep) is the
	// one place where the encoding is a deliberate approximation: Tesla only
	// supports four bands (ON_PEAK / PARTIAL_PEAK / OFF_PEAK / SUPER_OFF_PEAK)
	// and the ON_PEAK ≥ PARTIAL_PEAK ≥ OFF_PEAK invariant means we can't carve
	// out a separate "post-evening, pre-night" tier. We've chosen to bucket
	// 21:00–22:00 into PARTIAL_PEAK (0.19) during rebate seasons — slightly
	// generous on the sell side but the closest legal mapping. That choice is
	// also fixed; do not change it without explicit direction from the owner.
	//
	// If you genuinely need to change a number here, get explicit, express
	// confirmation from Ryan first and update buildOctopusTariff to match.
	// =========================================================================
	//
	// Sample times: morning=08:00, midday=12:00, evening=18:00, lateEvening=21:00.
	// Tested on a weekday (Monday); weekend behaviour is OFF_PEAK by design.
	tests := []struct {
		month           int
		wantMorning     float64
		wantMidday      float64
		wantEvening     float64
		wantLateEvening float64
	}{
		{1, 0.19, 0.14, 0.19, 0.14}, // Jan: Summer — no rebate
		{2, 0.19, 0.14, 0.19, 0.14},
		{3, 0.19, 0.14, 0.19, 0.14},
		{4, 0.19, 0.14, 0.19, 0.14},
		{5, 0.19, 0.14, 0.2424, 0.19},   // May: ShoulderMay — evening rebate; 21:00 PARTIAL_PEAK
		{6, 0.2424, 0.14, 0.2424, 0.19}, // Jun: Winter — full rebate; 21:00 PARTIAL_PEAK
		{7, 0.2424, 0.14, 0.2424, 0.19},
		{8, 0.2424, 0.14, 0.2424, 0.19},
		{9, 0.19, 0.14, 0.2424, 0.19}, // Sep: ShoulderSep — evening rebate; 21:00 PARTIAL_PEAK
		{10, 0.19, 0.14, 0.19, 0.14},  // Oct: Summer — no rebate
		{11, 0.19, 0.14, 0.19, 0.14},
		{12, 0.19, 0.14, 0.19, 0.14},
	}

	for _, tt := range tests {
		t.Run(time.Month(tt.month).String(), func(t *testing.T) {
			if got := octopusSellRate(t, tt.month, 8); got != tt.wantMorning {
				t.Errorf("morning: got %v, want %v", got, tt.wantMorning)
			}
			if got := octopusSellRate(t, tt.month, 12); got != tt.wantMidday {
				t.Errorf("midday: got %v, want %v", got, tt.wantMidday)
			}
			if got := octopusSellRate(t, tt.month, 18); got != tt.wantEvening {
				t.Errorf("evening: got %v, want %v", got, tt.wantEvening)
			}
			if got := octopusSellRate(t, tt.month, 21); got != tt.wantLateEvening {
				t.Errorf("lateEvening: got %v, want %v", got, tt.wantLateEvening)
			}
		})
	}
}

func TestBuildTOUTariffPeakWindow(t *testing.T) {
	tests := []struct {
		name                                             string
		hour, min                                        int
		wantFromHour, wantFromMin, wantToHour, wantToMin int
	}{
		{"on the hour", 3, 0, 3, 0, 4, 30},
		{"early in half hour", 3, 5, 3, 0, 4, 30},
		{"mid half hour", 3, 15, 3, 0, 5, 0},
		{"just before half hour", 3, 29, 3, 0, 5, 0},
		{"on the half hour", 3, 30, 3, 30, 5, 0},
		{"late in half hour", 3, 45, 3, 30, 5, 30},
		{"midnight", 0, 0, 0, 0, 1, 30},
		{"before midnight", 23, 0, 23, 0, 0, 30},
		{"before midnight half hour", 23, 30, 23, 30, 1, 0},
		{"late before midnight", 23, 45, 23, 30, 1, 30},
		// User-specified examples
		{"user example 0:30", 0, 30, 0, 30, 2, 0},
		{"user example 0:31", 0, 31, 0, 30, 2, 0},
		{"user example 0:45", 0, 45, 0, 30, 2, 30},
		{"user example 0:59", 0, 59, 0, 30, 2, 30},
		{"user example 1:00", 1, 0, 1, 0, 2, 30},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Date(2026, 1, 1, tt.hour, tt.min, 0, 0, time.UTC)
			tariff := buildTOUTariff(now)

			seasons, ok := tariff["seasons"].(map[string]any)
			if !ok {
				t.Fatal("missing seasons")
			}
			allYear, ok := seasons["AllYear"].(map[string]any)
			if !ok {
				t.Fatal("missing AllYear")
			}
			touPeriods, ok := allYear["tou_periods"].(map[string]any)
			if !ok {
				t.Fatal("missing tou_periods")
			}
			onPeak, ok := touPeriods["ON_PEAK"].(map[string]any)
			if !ok {
				t.Fatal("missing ON_PEAK")
			}
			periods, ok := onPeak["periods"].([]any)
			if !ok {
				t.Fatal("missing ON_PEAK periods")
			}
			period, ok := periods[0].(map[string]any)
			if !ok {
				t.Fatal("invalid period")
			}

			if period["fromHour"] != tt.wantFromHour || period["fromMinute"] != tt.wantFromMin ||
				period["toHour"] != tt.wantToHour || period["toMinute"] != tt.wantToMin {
				t.Errorf("got %d:%02d–%d:%02d, want %d:%02d–%d:%02d",
					period["fromHour"], period["fromMinute"],
					period["toHour"], period["toMinute"],
					tt.wantFromHour, tt.wantFromMin,
					tt.wantToHour, tt.wantToMin)
			}
		})
	}
}
