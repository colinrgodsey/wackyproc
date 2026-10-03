//go:build !linux

package proc_test

import (
	"testing"
	"time"

	"github.com/colinrgodsey/wackyproc/proc"
)

// TestWait_NaturalFallbackOnNonLinux verifies the natural (no hook) path on non-Linux:
// pidfd is unsupported so PidfdSupportedOnThisPlatform is false and Wait MUST use the
// polling ramp without the hook - a real exit is still detected promptly.
func TestWait_NaturalFallbackOnNonLinux(t *testing.T) {
	if proc.PidfdSupportedOnThisPlatform() {
		t.Skip("pidfd supported - not a non-Linux fallback path")
	}

	cwd := setupTestEnv(t)
	createExecutable(t, cwd, "nl-fallback", "sleep 0.2")
	id, err := proc.Run(cwd, "nl-fallback", nil, nil)
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
	if elapsed > 1500*time.Millisecond {
		t.Errorf("natural fallback took %v to detect a 200ms exit", elapsed)
	}
}
