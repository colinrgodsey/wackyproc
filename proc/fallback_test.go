package proc_test

import (
	"testing"
	"time"

	"github.com/colinrgodsey/wackyproc/proc"
)

// TestWait_PollingFallbackDetectsExit forces the polling-ramp fallback (even on Linux
// where pidfd is supported) and verifies it still detects process exit correctly with
// the ramp tuning from #15: the first polls run at 100ms, so a short task is caught
// within a couple of intervals, and longer waits settle without burning CPU.
func TestWait_PollingFallbackDetectsExit(t *testing.T) {
	proc.ForcePollingFallbackForTest = true
	defer func() { proc.ForcePollingFallbackForTest = false }()

	cwd := setupTestEnv(t)
	createExecutable(t, cwd, "fallback-short", "sleep 0.2")
	id, err := proc.Run(cwd, "fallback-short", nil, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// A 200ms task detected under the 100ms-start ramp: expect well under a second.
	start := time.Now()
	res, werr := proc.Wait(cwd, 5, id)
	elapsed := time.Since(start)
	if werr != nil {
		t.Fatalf("Wait: %v", werr)
	}
	if res != id {
		t.Fatalf("Wait returned %q, want %q", res, id)
	}
	if elapsed < 100*time.Millisecond {
		t.Errorf("fallback returned suspiciously fast (%v) - pidfd path may have engaged despite the hook", elapsed)
	}
	if elapsed > 1500*time.Millisecond {
		t.Errorf("fallback took %v to detect a 200ms exit (ramp tuning broken?)", elapsed)
	}
}

// TestWait_PollingFallbackTimeout verifies the fallback respects the wait deadline.
func TestWait_PollingFallbackTimeout(t *testing.T) {
	proc.ForcePollingFallbackForTest = true
	defer func() { proc.ForcePollingFallbackForTest = false }()

	cwd := setupTestEnv(t)
	createExecutable(t, cwd, "fallback-long", "sleep 30")
	id, err := proc.Run(cwd, "fallback-long", nil, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer func() { _ = proc.Stop(cwd, id, 1) }()

	start := time.Now()
	res, werr := proc.Wait(cwd, 1, id)
	elapsed := time.Since(start)
	if werr != nil {
		t.Fatalf("Wait: %v", werr)
	}
	if res != "" {
		t.Fatalf("expected timeout (empty), got %q", res)
	}
	// The ramp starts at 100ms and eases; a 1s timeout should return near the deadline.
	if elapsed > 3*time.Second {
		t.Errorf("fallback timeout took %v, exceeds bound", elapsed)
	}
}
