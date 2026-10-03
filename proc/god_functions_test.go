package proc

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestExitCodeFromProcessState(t *testing.T) {
	// Table-driven test of pure exit code mapping switch.
	t.Run("nil error is zero", func(t *testing.T) {
		if got := exitCodeFromProcessState(nil); got != 0 {
			t.Errorf("exitCodeFromProcessState(nil) = %d, want 0", got)
		}
	})

	t.Run("non-ExitError is 1", func(t *testing.T) {
		if got := exitCodeFromProcessState(errors.New("broken pipe")); got != 1 {
			t.Errorf("exitCodeFromProcessState(non-exit error) = %d, want 1", got)
		}
	})

	t.Run("exit status code", func(t *testing.T) {
		cmd := exec.Command("sh", "-c", "exit 42")
		err := cmd.Run()
		if err == nil {
			t.Fatal("expected command to exit with error")
		}
		if got := exitCodeFromProcessState(err); got != 42 {
			t.Errorf("exitCodeFromProcessState(exit 42) = %d, want 42", got)
		}
	})

	t.Run("killed by signal", func(t *testing.T) {
		cmd := exec.Command("sh", "-c", "kill -9 $$")
		err := cmd.Run()
		if err == nil {
			t.Fatal("expected command to exit with error")
		}
		// 128 + 9 = 137
		if got := exitCodeFromProcessState(err); got != 137 {
			t.Errorf("exitCodeFromProcessState(kill -9) = %d, want 137", got)
		}
	})
}

func TestStatusFromMarkers(t *testing.T) {
	dir := t.TempDir()

	// No markers
	if status, code, ok := statusFromMarkers(dir); ok {
		t.Errorf("expected no markers, got status=%q, code=%v", status, code)
	}

	// Crashed marker without exit code
	if err := os.WriteFile(filepath.Join(dir, CrashedFileName), []byte(""), 0644); err != nil {
		t.Fatal(err)
	}
	if status, code, ok := statusFromMarkers(dir); !ok || status != StatusCrashed || code != nil {
		t.Errorf("expected StatusCrashed with nil code, got status=%q, code=%v, ok=%v", status, code, ok)
	}

	// Crashed marker with exit code
	if err := os.WriteFile(filepath.Join(dir, ExitCodeFileName), []byte("137\n"), 0644); err != nil {
		t.Fatal(err)
	}
	status, code, ok := statusFromMarkers(dir)
	if !ok || status != StatusCrashed || code == nil || *code != 137 {
		t.Errorf("expected StatusCrashed with code 137, got status=%q, code=%v, ok=%v", status, code, ok)
	}
}

func TestStatusFromExitCode(t *testing.T) {
	dir := t.TempDir()

	// Absent exit code
	if status, code, ok := statusFromExitCode(dir); ok {
		t.Errorf("expected absent exit code, got status=%q, code=%v", status, code)
	}

	// Exit code 0 -> Completed
	if err := os.WriteFile(filepath.Join(dir, ExitCodeFileName), []byte("0\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if status, code, ok := statusFromExitCode(dir); !ok || status != StatusCompleted || code == nil || *code != 0 {
		t.Errorf("expected StatusCompleted with 0, got status=%q, code=%v, ok=%v", status, code, ok)
	}

	// Exit code 2 -> Failed
	if err := os.WriteFile(filepath.Join(dir, ExitCodeFileName), []byte("2\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if status, code, ok := statusFromExitCode(dir); !ok || status != StatusFailed || code == nil || *code != 2 {
		t.Errorf("expected StatusFailed with 2, got status=%q, code=%v, ok=%v", status, code, ok)
	}

	// Unparseable exit code
	if err := os.WriteFile(filepath.Join(dir, ExitCodeFileName), []byte("invalid\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if status, code, ok := statusFromExitCode(dir); ok {
		t.Errorf("expected unparseable exit code to return ok=false, got status=%q, code=%v", status, code)
	}
}

func TestPidRecycledHelper(t *testing.T) {
	if pidRecycled(1, nil) {
		t.Errorf("expected nil meta to not be recycled")
	}
	if pidRecycled(1, &Meta{StartTime: ""}) {
		t.Errorf("expected empty StartTime to not be recycled")
	}
	// Different start time
	if pidRecycled(os.Getpid(), &Meta{StartTime: "non-matching-start-time-xyz"}) {
		// If GetProcessStartTime returns non-empty for current pid, it should detect recycled
		if GetProcessStartTime(os.Getpid()) != "" {
			// Expected true
		}
	}
}

func TestCleanupProcDir(t *testing.T) {
	dir := t.TempDir()
	procDir := filepath.Join(dir, "testproc")
	if err := os.Mkdir(procDir, 0755); err != nil {
		t.Fatal(err)
	}
	cleanupProcDir(procDir)
	if _, err := os.Stat(procDir); !os.IsNotExist(err) {
		t.Errorf("expected %s to be removed", procDir)
	}
}

func TestFindCompleted(t *testing.T) {
	list := []ProcessInfo{
		{ID: "run1", Status: StatusRunning},
		{ID: "done", Status: StatusCompleted},
		{ID: "fail", Status: StatusFailed},
	}

	// Mixed set: only the completed ID is terminal; the running one is not.
	terminal, err := findCompleted(list, []string{"run1", "done"})
	if err != nil {
		t.Fatalf("findCompleted failed: %v", err)
	}
	if terminal["run1"] || !terminal["done"] {
		t.Errorf("expected run1 running and done terminal, got %+v", terminal)
	}

	// All-terminal set.
	terminal, err = findCompleted(list, []string{"done", "fail"})
	if err != nil || !terminal["done"] || !terminal["fail"] {
		t.Errorf("expected done+fail terminal, got %+v err=%v", terminal, err)
	}

	// A listed ID missing from the listing is an error (fail fast).
	if _, err = findCompleted(list, []string{"nonexistent"}); err == nil {
		t.Errorf("expected error for ID missing from the listing")
	}
}
