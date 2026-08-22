package main

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// The signal is the broadcast and nothing else: a worker that receives one marks,
// and one that receives none leaves the heartbeat unmarked so the probe fails.
func TestHealthWorkerMarksOnlyOnBroadcast(t *testing.T) {
	hb := NewHeartbeat(t.TempDir())
	dataChan := make(chan DisplayData, 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go healthWorker(ctx, dataChan, hb)

	// Nothing broadcast yet — this is the state powerctl sits in while a topic it
	// is waiting on has not arrived.
	time.Sleep(20 * time.Millisecond)
	assert.Zero(t, hb.last.Load(), "marked without a broadcast")

	dataChan <- DisplayData{}
	assert.Eventually(t, func() bool { return hb.last.Load() != 0 },
		time.Second, 5*time.Millisecond, "broadcast did not mark the heartbeat")
}
