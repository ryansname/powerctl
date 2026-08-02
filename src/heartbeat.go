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
// that is wedged — connected but deaf, or with a stage stuck — look identical.
// The usual failure is an MQTT subscription that stops delivering: paho stays
// "connected", nothing errors, and powerctl quietly stops controlling anything.
//
// So liveness is defined as *work observed recently*, not as a socket being
// open, and the work we watch is outgoing publishes. That single signal is
// enough: inverter-10 has to be fed continuously to keep running, so powerctl
// publishes every few seconds in normal operation. Anything that wedges the
// process — dead subscription starving the control loop, stuck worker, broker
// gone — stops those publishes, whatever the cause. Watching the inbound side
// separately would add a second thing to tune for no extra coverage.
//
// The publish path marks a timestamp in memory; a writer goroutine snapshots it
// to heartbeatFileName every heartbeatWriteInterval, and a second process
// (`powerctl --healthcheck`, run as the k8s exec liveness probe) reads the file
// back and fails if the mark has gone stale. It is a separate process because
// the from-scratch container image has no shell — there is no `test`/`stat` to
// point a probe at, so the binary has to be able to check itself.

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

// Heartbeat records the last time powerctl published to the broker.
type Heartbeat struct {
	path string
	// unix seconds of the last publish; atomic because Mark is called from the
	// publish hot path while Run reads it on a timer.
	last atomic.Int64
}

// NewHeartbeat seeds the mark with the current time, so process start doubles
// as the startup grace period: powerctl gets a full heartbeatDeadline to make
// its first publish before the probe can fail on it.
func NewHeartbeat(dir string) *Heartbeat {
	h := &Heartbeat{path: filepath.Join(dir, heartbeatFileName)}
	h.last.Store(time.Now().Unix())
	return h
}

// Mark records a publish. Called on the hot path, so it only touches memory —
// the file is written on a timer by Run.
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
		if err := writeHeartbeatFile(h.path, h.last.Load()); err != nil {
			log.Printf("Heartbeat write failed: %v\n", err)
		}
	}

	// Write immediately so the file exists before the probe's first check.
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
		// Includes "file missing", which is the correct failure: the writer
		// creates it at startup, so its absence means the process never got
		// that far. initialDelaySeconds on the probe covers the boot window.
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
