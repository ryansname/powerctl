package governor

// SteppedHysteresis provides stepped hysteresis with configurable thresholds.
// It converts a continuous value into discrete steps (0 to Steps) with hysteresis
// to prevent oscillation at threshold boundaries.
//
// For Ascending mode (value↑ → step↑):
//   - Increase thresholds should ascend: value must rise above each to increase step
//   - Decrease thresholds should descend: value must fall below each to decrease step
//
// For Descending mode (value↓ → step↑):
//   - Increase thresholds should descend: value must fall below each to increase step
//   - Decrease thresholds should ascend: value must rise above each to decrease step
//
// Thresholds are linearly interpolated from Start to End for steps 1 through Steps.
type SteppedHysteresis struct {
	Current int // Current step (0 to Steps)

	steps     int
	ascending bool

	increaseStart, increaseEnd float64
	decreaseStart, decreaseEnd float64
}

// NewSteppedHysteresis creates a stepped hysteresis controller.
func NewSteppedHysteresis(
	steps int,
	ascending bool,
	increaseStart, increaseEnd float64,
	decreaseStart, decreaseEnd float64,
) *SteppedHysteresis {
	return &SteppedHysteresis{
		steps:         steps,
		ascending:     ascending,
		increaseStart: increaseStart,
		increaseEnd:   increaseEnd,
		decreaseStart: decreaseStart,
		decreaseEnd:   decreaseEnd,
	}
}

// Update returns the new step based on the current value and hysteresis state.
// The step can only change when the value crosses a threshold; otherwise it stays
// in the hysteresis zone and returns the previous value.
func (s *SteppedHysteresis) Update(value float64) int {
	if s.steps <= 0 {
		return s.Current
	}

	increaseCount := countCrossed(value, s.steps, s.increaseStart, s.increaseEnd, s.ascending)
	decreaseCount := countCrossed(value, s.steps, s.decreaseStart, s.decreaseEnd, s.ascending)

	switch {
	case s.Current > decreaseCount:
		s.Current = decreaseCount
	case s.Current < increaseCount:
		s.Current = increaseCount
	}
	return s.Current
}

// countCrossed counts how many step thresholds the value has crossed.
// The comparison used depends on mode: >= for ascending, < for descending.
// The counting strategy depends on whether threshold order matches mode direction:
//   - Matching (asc thresholds + asc mode, or desc thresholds + desc mode): count consecutive from start
//   - Opposing (asc thresholds + desc mode, or desc thresholds + asc mode): find first success
func countCrossed(value float64, steps int, start, end float64, ascending bool) int {
	if steps <= 0 {
		return 0
	}

	crosses := func(threshold float64) bool {
		if ascending {
			return value >= threshold
		}
		return value < threshold
	}

	thresholdsAscending := end >= start
	orderMatchesMode := ascending == thresholdsAscending

	if orderMatchesMode {
		// Count consecutive successes from step 1
		for i := 1; i <= steps; i++ {
			if !crosses(threshold(start, end, i, steps)) {
				return i - 1
			}
		}
		return steps
	}

	// Find first success, return steps from there
	for i := 1; i <= steps; i++ {
		if crosses(threshold(start, end, i, steps)) {
			return steps - i + 1
		}
	}
	return 0
}

// threshold returns the threshold for step i out of n steps.
// Step 1 gets start, step n gets end, intermediate steps are linearly interpolated.
func threshold(start, end float64, i, n int) float64 {
	if n <= 1 {
		return start
	}
	return start + (end-start)*float64(i-1)/float64(n-1)
}
