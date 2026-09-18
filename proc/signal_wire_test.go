package proc

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// TestSignalAndKillDeliverToProcessGroup spawns a real child in its own process group
// (like the supervisor does) and verifies Signal dispatches a signal to the group and Kill
// force-terminates it. The process record is authored by hand in a temp cwd so the proc
// manager's file layout is exercised end to end.
func TestSignalAndKillDeliverToProcessGroup(t *testing.T) {
	cwd := t.TempDir()

	// A child that survives until killed: /bin/sh waits on SIGUSR1 by capturing it.
	cmd := exec.Command("sh", "-c", "trap 'echo got_usr1' USR1; while :; do sleep 1; done")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot spawn child for signal test: %v", err)
	}

	procDir := filepath.Join(cwd, ProcDirName, "sigtest")
	if err := os.MkdirAll(procDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(procDir, PIDFileName), []byte(strconv.Itoa(cmd.Process.Pid)), 0o600); err != nil {
		t.Fatal(err)
	}
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(procDir, PGIDFileName), []byte(strconv.Itoa(pgid)), 0o600); err != nil {
		t.Fatal(err)
	}

	defer func() {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		_ = cmd.Wait()
	}()

	// Signal a USR1 to the group; the shell trap runs and the child keeps running.
	if err := Signal(cwd, "sigtest", syscall.SIGUSR1); err != nil {
		t.Fatalf("Signal(SIGUSR1): %v", err)
	}

	// Give the trap a moment to execute, then confirm the child is still alive.
	time.Sleep(200 * time.Millisecond)
	if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("child died after SIGUSR1, want alive: %v", err)
	}

	// Kill force-terminates the group.
	if err := Kill(cwd, "sigtest"); err != nil {
		t.Fatalf("Kill: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("child did not exit after Kill")
	}
}
