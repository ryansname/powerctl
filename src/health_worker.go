package main

import (
	"context"
	"log"
)

// healthWorker owns powerctl's liveness signal.
//
// It marks the heartbeat on every DisplayData broadcast, which is true exactly when
// powerctl is working: statsWorker gates the broadcast on every expected topic having
// arrived, so nothing is marked while startup is incomplete or the loop has wedged, and
// nothing about it depends on whether powerctl_enabled happens to be on.
//
// Owning the signal in a worker — rather than stamping it from whichever code happens to
// be running — is the point. A side-effect signal keeps being produced by unrelated code
// when the system breaks; this one stops when the thing producing it stops. See
// heartbeat.go for the failure that taught us the difference.
func healthWorker(ctx context.Context, dataChan <-chan DisplayData, hb *Heartbeat) {
	log.Println("Health worker started")

	for {
		select {
		case <-dataChan:
			hb.Mark()

		case <-ctx.Done():
			log.Println("Health worker stopped")
			return
		}
	}
}
