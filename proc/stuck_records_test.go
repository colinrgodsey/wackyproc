package proc

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Regression coverage for bugs/wackyproc/stuck-running-records (Colin hit the wait-hang
// live 2026-09-28): a record whose process died (or hung) outside the supervisor's
// capture window stayed RUNNING, wait burned the full timeout repeatedly, prune skipped
// it, and its ID was later re-used for an unrelated dispatch.

func stuckSetup(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	toolsDir := filepath.Join(dir, ToolsDirName)
	if err := os.MkdirAll(toolsDir, 0755); err != nil {
		t.Fatalf("failed to create tools dir: %v", err)
	}
	return dir
}

func createStuckTool(t *testing.T, cwd, name, script string) {
	t.Helper()
	path := filepath.Join(cwd, ToolsDirName, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0755); err != nil {
		t.Fatalf("failed to create tool %s: %v", name, err)
	}
}

func readProcInt(t *testing.T, cwd, id, name string) int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(cwd, ProcDirName, id, name))
	if err != nil {
		t.Fatalf("read %s for %s: %v", name, id, err)
	}
	v, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("parse %s for %s: %v", name, id, err)
	}
	return v
}

// Acceptance 1: a supervised command whose process is SIGKILLed outside the capture
// window leaves a record that wait() detects as dead immediately - not RUNNING-forever.
func TestWait_For_KilledOutsideCapture_ReturnsImmediately(t *testing.T) {
	cwd := stuckSetup(t)
	createStuckTool(t, cwd, "sleeper", "sleep 60")

	id, err := Run(cwd, "sleeper", nil, nil)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	time.Sleep(150 * time.Millisecond) // let the supervisor record pid + supervisor_pid

	toolPID := readProcInt(t, cwd, id, PIDFileName)
	supPID := readProcInt(t, cwd, id, SupervisorPIDFileName)
	// Kill both outside wackyproc's capture window: no exit_code gets written.
	if err := syscall.Kill(toolPID, syscall.SIGKILL); err != nil {
		t.Fatalf("kill tool: %v", err)
	}
	if err := syscall.Kill(supPID, syscall.SIGKILL); err != nil {
		t.Fatalf("kill supervisor: %v", err)
	}

	start := time.Now()
	got, err := Wait(cwd, 30, id)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Wait failed: %v", err)
	}
	if got != id {
		t.Fatalf("Wait returned %q, want %q", got, id)
	}
	if elapsed > 5*time.Second {
		t.Errorf("Wait took %v on a dead record; want immediate return (<5s)", elapsed)
	}

	list, err := List(cwd)
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	found := false
	for _, p := range list {
		if p.ID == id {
			found = true
			if !isTerminal(p.Status) {
				t.Errorf("record %s status %s after out-of-band kill; want terminal", id, p.Status)
			}
		}
	}
	if !found {
		t.Fatalf("record %s missing from List", id)
	}
}

// Acceptance 2: a stuck RUNNING record can be force-disposed without a process exit.
func TestRemove_StuckRunningRecord(t *testing.T) {
	cwd := stuckSetup(t)
	createStuckTool(t, cwd, "sleeper", "sleep 60")

	id, err := Run(cwd, "sleeper", nil, nil)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	time.Sleep(150 * time.Millisecond)

	// Read pids BEFORE Remove so the still-alive processes can be cleaned up after.
	toolPID := readProcInt(t, cwd, id, PIDFileName)
	supPID := readProcInt(t, cwd, id, SupervisorPIDFileName)

	toolName, err := Remove(cwd, id)
	if err != nil {
		t.Fatalf("Remove failed: %v", err)
	}
	if toolName != "sleeper" {
		t.Errorf("Remove reported tool %q, want %q", toolName, "sleeper")
	}
	if _, err := os.Stat(filepath.Join(cwd, ProcDirName, id)); !os.IsNotExist(err) {
		t.Errorf("record dir for %s should be gone after Remove", id)
	}
	list, err := List(cwd)
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	for _, p := range list {
		if p.ID == id {
			t.Errorf("record %s still listed after Remove", id)
		}
	}

	// The process was never signaled by Remove: clean it up out-of-band.
	_ = syscall.Kill(toolPID, syscall.SIGKILL)
	_ = syscall.Kill(supPID, syscall.SIGKILL)

	if _, err := Remove(cwd, id); err == nil {
		t.Errorf("second Remove of %s should fail not-found", id)
	}
}

// Acceptance 3: IDs of disposed records are not re-claimed.
func TestClaim_NeverReclaimsDisposedID(t *testing.T) {
	base := t.TempDir()
	seeded := GenerateRandomID()
	recordDisposedID(base, seeded)

	// Deterministic core: a generator that only ever proposes the disposed ID must be
	// rejected on every draw and exhaust its retries.
	if _, _, err := claimUniqueProcessDir(base, func() string { return seeded }); err == nil {
		t.Fatalf("claim accepted disposed ID %s; want retry-exhaustion error", seeded)
	}

	// Integration soak: real claims never hand out the disposed ID.
	for i := 0; i < 300; i++ {
		got, _, err := ClaimUniqueProcessDir(base)
		if err != nil {
			t.Fatalf("ClaimUniqueProcessDir #%d: %v", i, err)
		}
		if got == seeded {
			t.Fatalf("claim #%d re-claimed disposed ID %s", i, seeded)
		}
	}
}
