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
	defer func() { proc.ForcePollingFallbackForTest = true }()

	cwd := setupTestEnv(t)
	// A SHORT (~50ms) task separates the two paths: pidfd detects it at ~50-65ms
	// (immediate kernel wake), while the polling ramp quantizes to the first
	// >=100ms tick no matter when in the interval the task exits. Asserting a lower
	// bound of ~90ms therefore proves the pidfd fast path was genuinely bypassed - a
	// broken hook (pidfd engaged) would complete in ~50-65ms and fail the bound.
	createExecutable(t, cwd, "fallback-short", "sleep 0.05")
	id, err := proc.Run(cwd, "fallback-short", nil, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	start := time.Now()
	res, werr := proc.Wait(cwd, 5, id)
	elapsed := time.Since(start)
	if werr != nil {
		t.Fatalf("Wait: %v", werr)
	}
	if res != id {
		t.Fatalf("Wait returned %q, want %q", res, id)
	}
	// Bypass invariant: the polling ramp cannot detect a 50ms exit before its first
	// 100ms tick, so a return under ~90ms means the pidfd path engaged despite the hook.
	if elapsed < 90*time.Millisecond {
		t.Errorf("fallback returned in %v - under the 100ms first tick, pidfd must have engaged (hook broken)", elapsed)
	}
	if elapsed > 1500*time.Millisecond {
		t.Errorf("fallback took %v to detect a 50ms exit (ramp tuning broken?)", elapsed)
	}
}

// TestWait_PollingFallbackTimeout verifies the fallback respects the wait deadline.
func TestWait_PollingFallbackTimeout(t *testing.T) {
	proc.ForcePollingFallbackForTest = true
	defer func() { proc.ForcePollingFallbackForTest = true }()

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
