package governor

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// Helper functions to create test configs matching the three use cases

// Overflow mode: ascending (value↑ → step↑)
// Increase (ascending): 95.75 → 99.5
// Decrease (descending): 98.5 → 95.0
func newOverflowHysteresis() *SteppedHysteresis {
	return NewSteppedHysteresis(4, true, 95.75, 99.5, 98.5, 95.0)
}

// Powerwall Low mode: descending (value↓ → step↑)
// Increase (descending): 41 → 25
// Decrease (ascending): 28 → 44
func newPowerwallLowHysteresis() *SteppedHysteresis {
	return NewSteppedHysteresis(9, false, 41, 25, 28, 44)
}

// SOC Limits mode: ascending (value↑ → step↑)
// Increase (ascending): 15 → 25
// Decrease (ascending): 12.5 → 22.5
func newSOCLimitsHysteresis() *SteppedHysteresis {
	return NewSteppedHysteresis(3, true, 15, 25, 12.5, 22.5)
}

func TestOverflowMode(t *testing.T) {
	t.Run("rising SOC enables inverters", func(t *testing.T) {
		h := newOverflowHysteresis()

		// Below first threshold - stays at 0
		assert.Equal(t, 0, h.Update(95.0))

		// Crosses first threshold
		assert.Equal(t, 1, h.Update(96.0))

		// Crosses second threshold
		assert.Equal(t, 2, h.Update(97.5))

		// Crosses third threshold
		assert.Equal(t, 3, h.Update(98.5))

		// Crosses fourth threshold
		assert.Equal(t, 4, h.Update(99.6))
	})

	t.Run("falling SOC disables inverters", func(t *testing.T) {
		h := newOverflowHysteresis()
		h.Current = 4

		// Still above all decrease thresholds
		assert.Equal(t, 4, h.Update(99.0))

		// Falls below first decrease threshold
		assert.Equal(t, 3, h.Update(98.0))

		// Falls further
		assert.Equal(t, 2, h.Update(97.0))

		// Falls below all decrease thresholds
		assert.Equal(t, 0, h.Update(94.0))
	})

	t.Run("hysteresis prevents oscillation", func(t *testing.T) {
		h := newOverflowHysteresis()
		h.Current = 2

		// Value in hysteresis band - stays at 2
		// Increase threshold for step 3 is 98.25
		// Decrease threshold for step 2 is 97.33
		// So 97.5 is in the band
		assert.Equal(t, 2, h.Update(97.5))
		assert.Equal(t, 2, h.Update(97.8))
		assert.Equal(t, 2, h.Update(97.4))

		// Crosses increase threshold for step 3
		assert.Equal(t, 3, h.Update(98.3))
	})
}

func TestPowerwallLowMode(t *testing.T) {
	t.Run("falling SOC enables inverters", func(t *testing.T) {
		h := newPowerwallLowHysteresis()

		// Above first threshold - stays at 0
		assert.Equal(t, 0, h.Update(42.0))

		// Falls below first threshold (41%)
		assert.Equal(t, 1, h.Update(40.0))

		// Falls below more thresholds
		assert.Equal(t, 3, h.Update(36.0))

		// Falls below all thresholds
		assert.Equal(t, 9, h.Update(24.0))
	})

	t.Run("rising SOC disables inverters", func(t *testing.T) {
		h := newPowerwallLowHysteresis()
		h.Current = 9

		// Still below first decrease threshold (28%)
		assert.Equal(t, 9, h.Update(27.0))

		// Rises above first decrease threshold
		assert.Equal(t, 8, h.Update(29.0))

		// Rises above more thresholds (36 < 38, so 4 inverters can stay)
		assert.Equal(t, 4, h.Update(36.0))

		// Rises above all decrease thresholds (44%)
		assert.Equal(t, 0, h.Update(45.0))
	})

	t.Run("hysteresis prevents oscillation", func(t *testing.T) {
		h := newPowerwallLowHysteresis()
		h.Current = 3

		// Value at 36% is in hysteresis band for step 3
		// Increase threshold for step 4 is 35%
		// Decrease threshold for step 3 is 34%
		assert.Equal(t, 3, h.Update(36.0))

		// Still in band
		assert.Equal(t, 3, h.Update(35.5))

		// Falls below increase threshold for step 4
		assert.Equal(t, 4, h.Update(34.5))
	})
}

func TestSOCLimitsMode(t *testing.T) {
	t.Run("rising SOC increases limit", func(t *testing.T) {
		h := newSOCLimitsHysteresis()

		// Below first threshold
		assert.Equal(t, 0, h.Update(14.0))

		// Crosses first threshold (15%)
		assert.Equal(t, 1, h.Update(16.0))

		// Crosses second threshold (20%)
		assert.Equal(t, 2, h.Update(21.0))

		// Crosses third threshold (25%)
		assert.Equal(t, 3, h.Update(26.0))
	})

	t.Run("falling SOC decreases limit", func(t *testing.T) {
		h := newSOCLimitsHysteresis()
		h.Current = 3

		// Above all decrease thresholds
		assert.Equal(t, 3, h.Update(24.0))

		// Falls below third decrease threshold (22.5%)
		assert.Equal(t, 2, h.Update(21.0))

		// Falls below second decrease threshold (17.5%)
		assert.Equal(t, 1, h.Update(16.0))

		// Falls below first decrease threshold (12.5%)
		assert.Equal(t, 0, h.Update(11.0))
	})

	t.Run("hysteresis prevents oscillation", func(t *testing.T) {
		h := newSOCLimitsHysteresis()
		h.Current = 1

		// Value at 18% is in hysteresis band for step 1
		// Increase threshold for step 2 is 20%
		// Decrease threshold for step 1 is 17.5%
		assert.Equal(t, 1, h.Update(18.0))
		assert.Equal(t, 1, h.Update(19.0))
		assert.Equal(t, 1, h.Update(18.0))

		// Crosses increase threshold
		assert.Equal(t, 2, h.Update(21.0))
	})
}

func TestEdgeCases(t *testing.T) {
	t.Run("zero steps preserves current", func(t *testing.T) {
		h := NewSteppedHysteresis(0, true, 0, 0, 0, 0)
		h.Current = 5
		assert.Equal(t, 5, h.Update(50.0))
	})

	t.Run("single step", func(t *testing.T) {
		h := NewSteppedHysteresis(1, true, 50, 50, 40, 40)

		// Below threshold
		assert.Equal(t, 0, h.Update(45.0))

		// Above increase threshold
		assert.Equal(t, 1, h.Update(55.0))

		// In hysteresis band
		assert.Equal(t, 1, h.Update(45.0))

		// Below decrease threshold
		assert.Equal(t, 0, h.Update(35.0))
	})

	t.Run("exact threshold values", func(t *testing.T) {
		h := newOverflowHysteresis()

		// At exact threshold in ascending mode (>=)
		assert.Equal(t, 1, h.Update(95.75))

		h2 := newPowerwallLowHysteresis()
		// At exact threshold in descending mode (<)
		assert.Equal(t, 0, h2.Update(41.0)) // Not < 41
		assert.Equal(t, 1, h2.Update(40.99))
	})
}

func TestCountCrossed(t *testing.T) {
	t.Run("ascending thresholds ascending mode", func(t *testing.T) {
		// Overflow increase: 95.75 → 99.5, 4 steps
		assert.Equal(t, 0, countCrossed(95.0, 4, 95.75, 99.5, true))
		assert.Equal(t, 1, countCrossed(96.0, 4, 95.75, 99.5, true))
		assert.Equal(t, 2, countCrossed(97.5, 4, 95.75, 99.5, true))
		assert.Equal(t, 4, countCrossed(100.0, 4, 95.75, 99.5, true))
	})

	t.Run("descending thresholds ascending mode", func(t *testing.T) {
		// Overflow decrease: 98.5 → 95.0, 4 steps
		assert.Equal(t, 0, countCrossed(94.0, 4, 98.5, 95.0, true))
		assert.Equal(t, 2, countCrossed(97.0, 4, 98.5, 95.0, true))
		assert.Equal(t, 4, countCrossed(99.0, 4, 98.5, 95.0, true))
	})

	t.Run("descending thresholds descending mode", func(t *testing.T) {
		// Powerwall Low increase: 41 → 25, 9 steps
		assert.Equal(t, 0, countCrossed(42.0, 9, 41, 25, false))
		assert.Equal(t, 1, countCrossed(40.0, 9, 41, 25, false))
		assert.Equal(t, 3, countCrossed(36.0, 9, 41, 25, false))
		assert.Equal(t, 9, countCrossed(24.0, 9, 41, 25, false))
	})

	t.Run("ascending thresholds descending mode", func(t *testing.T) {
		// Powerwall Low decrease: 28 → 44, 9 steps
		assert.Equal(t, 9, countCrossed(27.0, 9, 28, 44, false))
		assert.Equal(t, 4, countCrossed(36.0, 9, 28, 44, false))
		assert.Equal(t, 0, countCrossed(45.0, 9, 28, 44, false))
	})
}

func TestThreshold(t *testing.T) {
	// Ascending thresholds
	assert.Equal(t, 10.0, threshold(10, 20, 1, 3))
	assert.Equal(t, 15.0, threshold(10, 20, 2, 3))
	assert.Equal(t, 20.0, threshold(10, 20, 3, 3))

	// Descending thresholds
	assert.Equal(t, 20.0, threshold(20, 10, 1, 3))
	assert.Equal(t, 15.0, threshold(20, 10, 2, 3))
	assert.Equal(t, 10.0, threshold(20, 10, 3, 3))

	// Single step
	assert.Equal(t, 50.0, threshold(50, 100, 1, 1))
}
