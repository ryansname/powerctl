package governor

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

var emaEpoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func emaAt(s int) time.Time { return emaEpoch.Add(time.Duration(s) * time.Second) }

func TestEMA_Empty(t *testing.T) {
	e := NewEMA(10 * time.Second)
	assert.Equal(t, 0.0, e.Value())
}

func TestEMA_FirstUpdateSeedsValue(t *testing.T) {
	e := NewEMA(10 * time.Second)
	// No cold-start ramp from zero: first sample is taken as-is.
	assert.Equal(t, 100.0, e.updateAt(100, emaAt(0)))
	assert.Equal(t, 100.0, e.Value())
}

func TestEMA_SingleStepUsesElapsedTime(t *testing.T) {
	e := NewEMA(10 * time.Second)
	e.updateAt(100, emaAt(0))
	got := e.updateAt(200, emaAt(1)) // dt=1s, tau=10s -> alpha = 1-exp(-0.1)
	alpha := 1 - math.Exp(-0.1)
	assert.InDelta(t, 100+alpha*100, got, 1e-9)
}

func TestEMA_LargerGapMovesFurther(t *testing.T) {
	e := NewEMA(10 * time.Second)
	e.updateAt(100, emaAt(0))
	got := e.updateAt(200, emaAt(10)) // dt=tau -> alpha = 1-exp(-1) ≈ 0.632
	assert.InDelta(t, 100+(1-math.Exp(-1))*100, got, 1e-9)
}

// For a constant input the result depends only on total elapsed time, not on how
// the interval is subdivided. This is the key property of the time-aware alpha.
func TestEMA_SubdivisionInvariantForConstantInput(t *testing.T) {
	coarse := NewEMA(8 * time.Second)
	coarse.updateAt(50, emaAt(0))
	coarse.updateAt(60, emaAt(10))

	fine := NewEMA(8 * time.Second)
	fine.updateAt(50, emaAt(0))
	for s := 1; s <= 10; s++ {
		fine.updateAt(60, emaAt(s))
	}
	assert.InDelta(t, coarse.Value(), fine.Value(), 1e-9)
}

func TestEMA_ConvergesToConstant(t *testing.T) {
	e := NewEMA(5 * time.Second)
	e.updateAt(0, emaAt(0))
	for s := 1; s <= 200; s++ {
		e.updateAt(42, emaAt(s))
	}
	assert.InDelta(t, 42.0, e.Value(), 1e-6)
}

func TestEMA_NonIncreasingTimeIgnored(t *testing.T) {
	e := NewEMA(10 * time.Second)
	e.updateAt(100, emaAt(5))
	// Same instant and an earlier instant must not move the value.
	assert.Equal(t, 100.0, e.updateAt(200, emaAt(5)))
	assert.Equal(t, 100.0, e.updateAt(200, emaAt(3)))
}

func TestEMA_ZeroTauPassThrough(t *testing.T) {
	e := NewEMA(0)
	assert.Equal(t, 5.0, e.updateAt(5, emaAt(0)))
	assert.Equal(t, 9.0, e.updateAt(9, emaAt(1)))
	assert.Equal(t, 9.0, e.Value())
}
