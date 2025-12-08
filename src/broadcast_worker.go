package main

import (
	"context"
	"log"
)

// broadcastWorker receives DisplayData and fans out to multiple downstream workers
// This implements the actor pattern where the broadcast logic is isolated in a single worker
func broadcastWorker(ctx context.Context, inputChan <-chan DisplayData, outputChans []chan<- DisplayData) {
	for {
		select {
		case data := <-inputChan:
			// Fan out to all downstream workers using non-blocking sends
			for i, ch := range outputChans {
				select {
				case ch <- data:
					// Successfully sent
				case <-ctx.Done():
					return
				default:
					// Channel full, log warning but continue
					log.Printf("Warning: downstream worker %d channel full, dropping update\n", i)
				}
			}

		case <-ctx.Done():
			return
		}
	}
}
