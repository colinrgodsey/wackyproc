package proc_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/colinrgodsey/wackyproc/proc"
)

// writeRecordFile creates a process record directory containing exactly the named files.
func writeRecordFile(t *testing.T, cwd, procID, name string, content []byte) string {
	t.Helper()
	dir := filepath.Join(cwd, proc.ProcDirName, procID)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("failed to create %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), content, 0644); err != nil {
		t.Fatalf("failed to write %s: %v", name, err)
	}
	return dir
}

// A record whose meta.json cannot be parsed must not be stopped on guesswork: the zero Meta
// has no StartTime, which is the state that skips the PID-recycling guard and signals a
// group that may belong to another process.
func TestStopAndSignalRefuseUnreadableMeta(t *testing.T) {
	cwd := t.TempDir()
	writeRecordFile(t, cwd, "scr1", proc.MetaFileName, []byte("{ not json"))

	err := proc.Stop(cwd, "scr1", 1)
	if err == nil {
		t.Fatalf("expected Stop to fail on unparsable %s, got nil", proc.MetaFileName)
	}
	if !strings.Contains(err.Error(), proc.MetaFileName) {
		t.Errorf("expected Stop error to name %s, got %v", proc.MetaFileName, err)
	}

	err = proc.Signal(cwd, "scr1", syscall.SIGTERM)
	if err == nil {
		t.Fatalf("expected Signal to fail on unparsable %s, got nil", proc.MetaFileName)
	}
	if !strings.Contains(err.Error(), proc.MetaFileName) {
		t.Errorf("expected Signal error to name %s, got %v", proc.MetaFileName, err)
	}
}

// Without a recorded PID there is nothing to aim at. Stop has to say so instead of
// reporting a successful stop of a process it never signalled.
func TestStopReportsMissingSignalTarget(t *testing.T) {
	cwd := t.TempDir()
	meta, err := json.Marshal(proc.Meta{ID: "scr2", Tool: "sleeper", StartedAt: time.Now().Unix()})
	if err != nil {
		t.Fatalf("failed to marshal meta: %v", err)
	}
	writeRecordFile(t, cwd, "scr2", proc.MetaFileName, meta)

	err = proc.Stop(cwd, "scr2", 1)
	if err == nil {
		t.Fatal("expected Stop to report an unusable PID/PGID, got nil")
	}
	if !strings.Contains(err.Error(), "invalid PID/PGID") {
		t.Errorf("expected an invalid PID/PGID error, got %v", err)
	}
}

// A present-but-unparseable .seq used to fall through as zero, restarting the Gen and
// ConsumedSeq sequence that separates fresh records from stale ones.
func TestNextSeqRefusesCorruptSequence(t *testing.T) {
	base := t.TempDir()
	seqPath := filepath.Join(base, ".seq")
	if err := os.WriteFile(seqPath, []byte("not-a-number\n"), 0644); err != nil {
		t.Fatalf("failed to seed corrupt .seq: %v", err)
	}

	seq, err := proc.NextSeq(base)
	if err == nil {
		t.Fatalf("expected NextSeq to refuse a corrupt .seq, got sequence %d", seq)
	}
	if !strings.Contains(err.Error(), "refusing to restart the sequence") {
		t.Errorf("expected the refusal to explain the risk, got %v", err)
	}

	// The offending file must survive for inspection rather than being silently replaced.
	data, readErr := os.ReadFile(seqPath)
	if readErr != nil {
		t.Fatalf("failed to re-read .seq: %v", readErr)
	}
	if strings.TrimSpace(string(data)) != "not-a-number" {
		t.Errorf("expected corrupt .seq to be left untouched, got %q", string(data))
	}

	// A healthy sequence resumes normally once the corrupt content is removed.
	if err := os.Remove(seqPath); err != nil {
		t.Fatalf("failed to remove corrupt .seq: %v", err)
	}
	if seq, err := proc.NextSeq(base); err != nil || seq != 1 {
		t.Errorf("expected sequence 1 after clearing the corrupt file, got %d (err %v)", seq, err)
	}
}

// The exit code write is the one whose absence turns a clean exit into a reported CRASHED,
// so Supervise must return it rather than exit 0 with a record that cannot prove anything.
func TestSuperviseReportsExitCodeWriteFailure(t *testing.T) {
	cwd := t.TempDir()
	procDir := writeRecordFile(t, cwd, "scr3", proc.MetaFileName, mustMarshalMeta(t, proc.Meta{
		ID:       "scr3",
		Tool:     "true",
		ToolPath: "/bin/true",
		Cwd:      cwd,
	}))
	// Blocking the write by occupying the destination with a directory is deterministic and
	// root-proof: WriteFile over a directory fails with EISDIR for any caller.
	if err := os.Mkdir(filepath.Join(procDir, proc.ExitCodeFileName), 0755); err != nil {
		t.Fatalf("failed to occupy %s: %v", proc.ExitCodeFileName, err)
	}

	err := proc.Supervise(procDir)
	if err == nil {
		t.Fatalf("expected Supervise to report the failed %s write, got nil", proc.ExitCodeFileName)
	}
	if !strings.Contains(err.Error(), proc.ExitCodeFileName) {
		t.Errorf("expected the error to name %s, got %v", proc.ExitCodeFileName, err)
	}
}

// A supervisor that cannot record its own PID leaves CheckLiveness unable to tell "child
// exiting" from "supervisor gone", so it must fail before starting anything.
func TestSuperviseReportsSupervisorPIDWriteFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("directory permissions do not deny writes to root")
	}
	cwd := t.TempDir()
	procDir := writeRecordFile(t, cwd, "scr4", proc.MetaFileName, mustMarshalMeta(t, proc.Meta{
		ID:       "scr4",
		Tool:     "true",
		ToolPath: "/bin/true",
		Cwd:      cwd,
	}))
	if err := os.Chmod(procDir, 0500); err != nil {
		t.Fatalf("failed to make %s read-only: %v", procDir, err)
	}
	t.Cleanup(func() { _ = os.Chmod(procDir, 0755) })

	err := proc.Supervise(procDir)
	if err == nil {
		t.Fatal("expected Supervise to report the failed supervisor_pid write, got nil")
	}
	if !strings.Contains(err.Error(), proc.SupervisorPIDFileName) {
		t.Errorf("expected the error to name %s, got %v", proc.SupervisorPIDFileName, err)
	}
}

// The struct returned by CheckLiveness has to carry the recorded target, since that is the
// whole reason the five-value return went away.
func TestCheckLivenessReportsRecordedTarget(t *testing.T) {
	cwd := t.TempDir()
	procDir := writeRecordFile(t, cwd, "scr5", proc.MetaFileName, mustMarshalMeta(t, proc.Meta{
		ID:        "scr5",
		Tool:      "self",
		StartedAt: time.Now().Unix(),
	}))
	pid := os.Getpid()
	pgid, err := syscall.Getpgid(pid)
	if err != nil {
		t.Fatalf("failed to read own pgid: %v", err)
	}
	for name, val := range map[string]string{proc.PIDFileName: strconv.Itoa(pid), proc.PGIDFileName: strconv.Itoa(pgid)} {
		if err := os.WriteFile(filepath.Join(procDir, name), []byte(val+"\n"), 0644); err != nil {
			t.Fatalf("failed to write %s: %v", name, err)
		}
	}

	meta := proc.Meta{ID: "scr5", Tool: "self", StartTime: proc.GetProcessStartTime(pid)}
	liveness, err := proc.CheckLiveness(procDir, &meta)
	if err != nil {
		t.Fatalf("CheckLiveness failed: %v", err)
	}
	if liveness.Status != proc.StatusRunning {
		t.Errorf("expected %s for our own live process, got %s", proc.StatusRunning, liveness.Status)
	}
	if liveness.PID != pid || liveness.PGID != pgid {
		t.Errorf("expected recorded target %d/%d, got %d/%d", pid, pgid, liveness.PID, liveness.PGID)
	}
}

// End to end on a real process group: Stop has to terminate it and report success, which is
// what pins the ESRCH tolerance and the post-SIGKILL verification against false failures.
func TestStopTerminatesDetachedProcessGroup(t *testing.T) {
	sleeper := newDetachedSleeper(t)
	cwd := t.TempDir()
	procDir := writeRecordFile(t, cwd, "scr6", proc.MetaFileName, mustMarshalMeta(t, proc.Meta{
		ID:        "scr6",
		Tool:      "sleep",
		ToolPath:  "/usr/bin/setsid",
		Args:      []string{"sleep", "120"},
		Cwd:       cwd,
		StartTime: proc.GetProcessStartTime(sleeper),
	}))
	for name, val := range map[string]string{proc.PIDFileName: strconv.Itoa(sleeper), proc.PGIDFileName: strconv.Itoa(sleeper)} {
		if err := os.WriteFile(filepath.Join(procDir, name), []byte(val+"\n"), 0644); err != nil {
			t.Fatalf("failed to write %s: %v", name, err)
		}
	}

	if err := proc.Stop(cwd, "scr6", 2); err != nil {
		t.Fatalf("Stop failed: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(sleeper, 0); err != nil {
			return // gone, as reported
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("Stop reported success but pid %d is still signalable", sleeper)
}

// newDetachedSleeper starts a sleep in its own session and returns its pid. It must not be a
// child of the test: an unreaped child survives as a zombie, and a zombie answers kill -0,
// so a parent that does not wait for its child reports it as running. Detaching it makes init
// the reaper, matching what the supervisor does for the processes wackyproc tracks.
func newDetachedSleeper(t *testing.T) int {
	t.Helper()
	setsid, err := exec.LookPath("setsid")
	if err != nil {
		t.Skip("setsid is not available, cannot start a detached process")
	}
	pidPath := filepath.Join(t.TempDir(), "sleeper.pid")
	cmd := exec.Command("sh", "-c", setsid+" sleep 120 < /dev/null > /dev/null 2>&1 & echo $! > "+strconv.Quote(pidPath))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("cannot start detached sleeper: %v (%s)", err, out)
	}
	data, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatalf("failed to read sleeper pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("failed to parse sleeper pid %q: %v", strings.TrimSpace(string(data)), err)
	}
	// setsid forks when it is already a process group leader, in which case the pid we were
	// handed exited straight away and there is nothing for Stop to act on.
	if err := syscall.Kill(pid, 0); err != nil {
		t.Skipf("detached sleeper did not stay alive: %v", err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	return pid
}

func mustMarshalMeta(t *testing.T, meta proc.Meta) []byte {
	t.Helper()
	data, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("failed to marshal meta: %v", err)
	}
	return data
}
