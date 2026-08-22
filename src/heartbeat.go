package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Liveness heartbeat.
//
// powerctl listens on no port, so from the outside a healthy process and one
// that is wedged look identical. Liveness is therefore defined as *the control
// loop turning*, and the event that proves it is statsWorker's DisplayData
// broadcast: it is gated on every expected topic having arrived, so it stops
// while powerctl is still starting, stops if the loop wedges, and is unaffected
// by whether powerctl_enabled happens to be on.
//
// It used to be defined as outgoing publishes, reasoning that inverter-10 must
// be fed continuously so publishes prove the loop is turning. That was wrong.
// cerboKeepaliveWorker publishes every 50s from its own ticker with no
// DisplayData dependency — it has to, or the Cerbo stops reporting voltage — so
// it refreshed the mark twice per deadline forever while every other worker sat
// starved on an empty channel. One topic that never arrived left powerctl
// reporting healthy indefinitely while controlling nothing. A signal produced as
// a side effect of unrelated code cannot fail closed; an owned one can, which is
// why healthWorker (health_worker.go) now does the marking.
//
// A writer goroutine snapshots the mark to heartbeatFileName every
// heartbeatWriteInterval, and a second process (`powerctl --healthcheck`, run as
// the k8s exec liveness probe) reads the file back and fails if the mark has
// gone stale or was never written. It is a separate process because the
// from-scratch container image has no shell — there is no `test`/`stat` to point
// a probe at, so the binary has to be able to check itself.
//
// Nothing is stamped before the first broadcast, so a process that never becomes
// ready never satisfies the probe and kubelet restarts it. That is deliberate:
// the usual cause is a topic that will not arrive on its own, and a loud
// CrashLoopBackOff is worth far more than a pod that quietly controls nothing.

// heartbeatFileName is the snapshot the writer produces and --healthcheck reads.
// It lives under the heartbeat dir (see main), which is expected to be
// ephemeral, node-local scratch — not the durable state volume. It is rewritten
// every few seconds and carries nothing worth persisting, and a value left over
// from a previous process must never be able to satisfy a probe.
const heartbeatFileName = "heartbeat"

// heartbeatWriteInterval is how often the mark is flushed to disk. Well under
// heartbeatDeadline, so a healthy process is never mistaken for a stale one
// just because the snapshot lagged.
const heartbeatWriteInterval = 15 * time.Second

// heartbeatDeadline is the maximum silence tolerated before --healthcheck
// reports unhealthy. Normal cadence is single-digit seconds (inverter-10 keeps
// demanding output), so two minutes is far above anything legitimate while
// still catching a wedge well inside the pod's usefulness window.
const heartbeatDeadline = 2 * time.Minute

// resolveDir picks a directory from, in order: an explicit flag value, an
// environment variable, then a fallback.
func resolveDir(flagValue, envVar, fallback string) string {
	if flagValue != "" {
		return flagValue
	}
	if fromEnv := os.Getenv(envVar); fromEnv != "" {
		return fromEnv
	}
	return fallback
}

// resolveHeartbeatDir resolves the heartbeat directory, falling back to the data
// directory so a plain local run needs no extra configuration. Both the main
// process and --healthcheck call this with the same flags, so the writer and the
// probe cannot disagree about where the file lives.
func resolveHeartbeatDir(heartbeatFlag, dataDirFlag string) string {
	return resolveDir(heartbeatFlag, "POWERCTL_HEARTBEAT_DIR",
		resolveDir(dataDirFlag, "POWERCTL_DATA_DIR", "."))
}

// Heartbeat records the last time powerctl broadcast a DisplayData.
type Heartbeat struct {
	path string
	// unix seconds of the last broadcast, 0 if there has never been one; atomic
	// because Mark is called from healthWorker while Run reads it on a timer.
	last atomic.Int64
}

// NewHeartbeat returns an unmarked heartbeat: nothing is written until powerctl
// has actually broadcast. This used to seed the mark with the start time, which
// handed every fresh process a full heartbeatDeadline of health whether or not
// it ever worked — and silently defeated the probe during exactly the startup
// wedge it exists to catch. The probe's initialDelaySeconds is the startup
// grace, and is the right place for it.
func NewHeartbeat(dir string) *Heartbeat {
	return &Heartbeat{path: filepath.Join(dir, heartbeatFileName)}
}

// Mark records a broadcast. Only touches memory — the file is written on a timer
// by Run.
func (h *Heartbeat) Mark() {
	if h == nil {
		return
	}
	h.last.Store(time.Now().Unix())
}

// Run flushes the mark to disk until ctx is done. A write failure is logged but
// not fatal: the probe will see a stale file and restart the pod, which is the
// correct outcome if the heartbeat volume has become unwritable.
func (h *Heartbeat) Run(ctx context.Context) {
	ticker := time.NewTicker(heartbeatWriteInterval)
	defer ticker.Stop()

	write := func() {
		last := h.last.Load()
		if last == 0 {
			// Never broadcast. Leave the file absent rather than writing a zero
			// stamp: absence is precisely what --healthcheck reads as "powerctl
			// never finished starting".
			return
		}
		if err := writeHeartbeatFile(h.path, last); err != nil {
			log.Printf("Heartbeat write failed: %v\n", err)
		}
	}

	// Try immediately; a no-op until the first broadcast marks the heartbeat.
	write()

	for {
		select {
		case <-ticker.C:
			write()
		case <-ctx.Done():
			return
		}
	}
}

// writeHeartbeatFile writes the mark atomically (temp file + rename), matching
// writeStateFile, so --healthcheck can never read a half-written snapshot.
func writeHeartbeatFile(path string, unixSeconds int64) error {
	tmp := path + ".tmp"
	body := strconv.FormatInt(unixSeconds, 10) + "\n"
	if err := os.WriteFile(tmp, []byte(body), 0o600); err != nil { //nolint:gosec // path from operator-set --heartbeat-dir/env, not untrusted input
		return err
	}
	return os.Rename(tmp, path) //nolint:gosec // paths from operator-set --heartbeat-dir/env, not untrusted input
}

// readHeartbeatFile parses a snapshot written by writeHeartbeatFile.
func readHeartbeatFile(path string) (time.Time, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // path from operator-set --heartbeat-dir/env, not untrusted input
	if err != nil {
		return time.Time{}, err
	}
	unix, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("malformed heartbeat %q: %w", strings.TrimSpace(string(raw)), err)
	}
	return time.Unix(unix, 0), nil
}

// checkHeartbeat reports how long powerctl has been silent, and whether that is
// past the deadline, as of now.
func checkHeartbeat(path string, now time.Time) (silence time.Duration, stale bool, err error) {
	at, err := readHeartbeatFile(path)
	if err != nil {
		return 0, false, err
	}
	silence = now.Sub(at)
	return silence, silence > heartbeatDeadline, nil
}

// runHealthcheck backs the --healthcheck flag: the k8s exec liveness probe runs
// the binary in this mode inside the same container. Exit 0 means powerctl has
// published recently; any non-zero exit tells kubelet to restart the pod.
func runHealthcheck(dir string) int {
	path := filepath.Join(dir, heartbeatFileName)

	silence, stale, err := checkHeartbeat(path, time.Now())
	if err != nil {
		// Includes "file missing", which is the correct failure: nothing is
		// written until the first broadcast, so absence means powerctl never
		// finished starting. initialDelaySeconds covers the boot window.
		fmt.Fprintf(os.Stderr, "unhealthy: cannot read heartbeat %s: %v\n", path, err)
		return 1
	}
	if stale {
		fmt.Fprintf(os.Stderr, "unhealthy: no publish for %s (limit %s)\n",
			silence.Round(time.Second), heartbeatDeadline)
		return 1
	}
	return 0
}
