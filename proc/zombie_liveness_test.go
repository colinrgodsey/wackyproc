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

// startZombie launches a child that exits immediately and deliberately never reaps
// it, so the child sits in state Z for the life of the test binary. The returned func
// reaps it for cleanup.
func startZombie(t *testing.T) (int, func()) {
	t.Helper()
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("zombie state is Linux-specific")
	}
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start zombie child: %v", err)
	}
	pid := cmd.Process.Pid
	for i := 0; i < 200; i++ {
		if isZombie(pid) {
			return pid, func() { _ = cmd.Wait() }
		}
		time.Sleep(10 * time.Millisecond)
	}
	_ = cmd.Wait()
	t.Skip("kernel reaped the child before it could be observed as a zombie")
	return 0, func() {}
}

// pidGone returns a pid that has been reaped and is therefore unreachable.
func pidGone(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("/bin/true")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Wait()
	if p, err := os.FindProcess(pid); err == nil && p.Signal(syscall.Signal(0)) == nil {
		t.Fatalf("pid %d is still reachable after reaping", pid)
	}
	return pid
}

func zombieRecordDir(t *testing.T, toolPID int, supPID int, writeSupervisor bool) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), ProcDirName, "zombierec")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir record dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, PIDFileName), []byte(strconv.Itoa(toolPID)), 0644); err != nil {
		t.Fatalf("write pid: %v", err)
	}
	if writeSupervisor {
		if err := os.WriteFile(filepath.Join(dir, SupervisorPIDFileName), []byte(strconv.Itoa(supPID)), 0644); err != nil {
			t.Fatalf("write supervisor pid: %v", err)
		}
	}
	return dir
}

func TestCheckLiveness_ZombieToolWithLiveSupervisorStaysRunning(t *testing.T) {
	pid, stop := startZombie(t)
	defer stop()

	dir := zombieRecordDir(t, pid, os.Getpid(), true)
	lv, err := CheckLiveness(dir, &Meta{ID: "zombierec"})
	if err != nil {
		t.Fatalf("CheckLiveness: %v", err)
	}
	if lv.Status != StatusRunning {
		t.Errorf("zombie tool with a live supervisor must stay RUNNING while the supervisor finalizes, got %s", lv.Status)
	}
	if _, err := os.Stat(filepath.Join(dir, CrashedFileName)); !os.IsNotExist(err) {
		t.Errorf("the crashed marker must not be written while finalizing: it outranks exit_code and would shadow the real status permanently")
	}
}

func TestCheckLiveness_ZombieToolWithDeadSupervisorIsCrashed(t *testing.T) {
	pid, stop := startZombie(t)
	defer stop()

	t.Run("supervisor already exited", func(t *testing.T) {
		dir := zombieRecordDir(t, pid, pidGone(t), true)
		lv, err := CheckLiveness(dir, &Meta{ID: "zombierec"})
		if err != nil {
			t.Fatalf("CheckLiveness: %v", err)
		}
		if lv.Status != StatusCrashed {
			t.Errorf("expected CRASHED when nobody can write the exit code, got %s", lv.Status)
		}
	})

	t.Run("no supervisor recorded", func(t *testing.T) {
		dir := zombieRecordDir(t, pid, 0, false)
		lv, err := CheckLiveness(dir, &Meta{ID: "zombierec"})
		if err != nil {
			t.Fatalf("CheckLiveness: %v", err)
		}
		if lv.Status != StatusCrashed {
			t.Errorf("expected CRASHED with no supervisor recorded, got %s", lv.Status)
		}
	})
}
