package main

import (
	"testing"
	"time"
)

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
				t.Fatal("missing periods")
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
