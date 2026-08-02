package main

import (
	"path/filepath"
	"testing"
	"time"
)

// A fresh Heartbeat is healthy: NewHeartbeat seeds the mark with the start time
// so the process gets a full deadline of grace before the probe can fail on it.
func TestHeartbeatFreshIsHealthy(t *testing.T) {
	dir := t.TempDir()
	hb := NewHeartbeat(dir)
	if err := writeHeartbeatFile(hb.path, hb.last.Load()); err != nil {
		t.Fatalf("write: %v", err)
	}

	silence, stale, err := checkHeartbeat(hb.path, time.Now())
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if stale {
		t.Errorf("fresh heartbeat reported stale after %s", silence)
	}
}

// The whole point: once publishes stop for longer than the deadline, the probe
// must fail so kubelet restarts the pod.
func TestHeartbeatGoesStale(t *testing.T) {
	dir := t.TempDir()
	hb := NewHeartbeat(dir)
	if err := writeHeartbeatFile(hb.path, hb.last.Load()); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Check from a point past the deadline rather than sleeping.
	future := time.Now().Add(heartbeatDeadline + time.Second)
	silence, stale, err := checkHeartbeat(hb.path, future)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if !stale {
		t.Errorf("heartbeat silent for %s not reported stale (deadline %s)", silence, heartbeatDeadline)
	}
}

// Mark refreshes the deadline: a process still publishing stays healthy no
// matter how long it has been up.
func TestHeartbeatMarkRefreshes(t *testing.T) {
	dir := t.TempDir()
	hb := NewHeartbeat(dir)

	// Backdate well past the deadline, then publish.
	hb.last.Store(time.Now().Add(-heartbeatDeadline - time.Minute).Unix())
	hb.Mark()

	if err := writeHeartbeatFile(hb.path, hb.last.Load()); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, stale, err := checkHeartbeat(hb.path, time.Now())
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if stale {
		t.Error("heartbeat reported stale after Mark")
	}
}

// A missing file is unhealthy, not a pass. The writer creates it at startup, so
// its absence means the process never got that far — failing open here would
// make the probe useless during exactly the startup wedge it should catch.
func TestHeartbeatMissingFileIsUnhealthy(t *testing.T) {
	path := filepath.Join(t.TempDir(), heartbeatFileName)
	if _, _, err := checkHeartbeat(path, time.Now()); err == nil {
		t.Error("missing heartbeat file did not report an error")
	}
	if code := runHealthcheck(t.TempDir()); code == 0 {
		t.Error("runHealthcheck exited 0 with no heartbeat file")
	}
}

// The writer and the probe must agree on the path from the same flags/env,
// otherwise the probe silently checks a file nobody writes.
func TestResolveHeartbeatDirFallsBackToDataDir(t *testing.T) {
	if got := resolveHeartbeatDir("", "/data"); got != "/data" {
		t.Errorf("heartbeat dir = %q, want /data", got)
	}
	if got := resolveHeartbeatDir("/run/powerctl", "/data"); got != "/run/powerctl" {
		t.Errorf("heartbeat dir = %q, want /run/powerctl", got)
	}
	t.Setenv("POWERCTL_HEARTBEAT_DIR", "/run/env")
	if got := resolveHeartbeatDir("", "/data"); got != "/run/env" {
		t.Errorf("heartbeat dir = %q, want /run/env", got)
	}
}

// Round-trip through the file, since the probe is a separate process and the
// file is the only channel between them.
func TestHeartbeatFileRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), heartbeatFileName)
	want := time.Now().Truncate(time.Second)

	if err := writeHeartbeatFile(path, want.Unix()); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := readHeartbeatFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !got.Equal(want) {
		t.Errorf("round-trip = %s, want %s", got, want)
	}
}
