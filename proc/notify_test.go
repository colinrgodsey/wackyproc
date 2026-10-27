package proc_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/colinrgodsey/wackyproc/proc"
)

// writeCompletionHook installs an on-proc-complete script in the owning agent folder,
// using the same directory convention the gate discovers.
func writeCompletionHook(t *testing.T, agentDir, name, body string) string {
	t.Helper()
	dir := filepath.Join(agentDir, "hooks", proc.CompletionHookEvent)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("failed to create hooks dir: %v", err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatalf("failed to write hook %s: %v", name, err)
	}
	return path
}

// holdSessionLock takes the agent session lock the way a live turn does, in a separate
// process, and releases it when the returned function is called. It holds the lock through
// fcntl flock, the same interface wackypub's session.lock uses, so the gate sees
// genuine contention. The holder is a single process with no children: flock is held by an
// open file description, so a child that inherits the fd would keep the lock after the
// holder was killed, which is a test artefact rather than a busy agent.
func holdSessionLock(t *testing.T, agentDir string) func() {
	t.Helper()
	lockPath := filepath.Join(agentDir, "session.lock")
	cmd := exec.Command("python3", "-c", "import fcntl,sys,time; f=open(sys.argv[1], 'a+'); fcntl.flock(f, fcntl.LOCK_EX); time.sleep(30)", lockPath)
	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start lock holder: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		busy, err := proc.SessionLockBusy(agentDir)
		if err != nil {
			t.Fatalf("lock probe failed: %v", err)
		}
		if busy {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	return func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}
}

func waitForFile(t *testing.T, path string, timeout time.Duration) (string, bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil && len(data) > 0 {
			return string(data), true
		}
		time.Sleep(25 * time.Millisecond)
	}
	return "", false
}

func TestDiscoverCompletionHooksOrderAndFilter(t *testing.T) {
	agentDir := t.TempDir()
	writeCompletionHook(t, agentDir, "20-second.sh", "echo second")
	writeCompletionHook(t, agentDir, "10-first.sh", "echo first")

	hookDir := filepath.Join(agentDir, "hooks", proc.CompletionHookEvent)
	if err := os.WriteFile(filepath.Join(hookDir, "notes.txt"), []byte("not executable"), 0o644); err != nil {
		t.Fatalf("failed to write non-hook file: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(hookDir, "nested.sh"), 0o755); err != nil {
		t.Fatalf("failed to write directory named like a hook: %v", err)
	}

	got, err := proc.DiscoverCompletionHooks(agentDir)
	if err != nil {
		t.Fatalf("discovery failed: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 hooks (non-executable file and directory excluded), got %v", got)
	}
	if base := []string{filepath.Base(got[0]), filepath.Base(got[1])}; base[0] != "10-first.sh" || base[1] != "20-second.sh" {
		t.Errorf("expected leading-number order, got %v", base)
	}

	missing, err := proc.DiscoverCompletionHooks(filepath.Join(agentDir, "no-such-agent"))
	if err != nil {
		t.Errorf("absent hooks dir must not be an error, got %v", err)
	}
	if len(missing) != 0 {
		t.Errorf("absent hooks dir must yield no scripts, got %v", missing)
	}
}

func TestCompletionNoticeNamesJobAndRetrievalCommand(t *testing.T) {
	code := 3
	notice := proc.CompletionNotice(proc.Meta{ID: "abcd1234", Tool: "build-tool"}, proc.StatusFailed, &code)
	for _, want := range []string{"abcd1234", "build-tool", proc.StatusFailed, "wackyproc get abcd1234", "3"} {
		if !strings.Contains(notice, want) {
			t.Errorf("notice missing %q; got:\n%s", want, notice)
		}
	}
}

func TestSessionLockBusyTracksAnotherHolder(t *testing.T) {
	agentDir := t.TempDir()
	if busy, err := proc.SessionLockBusy(agentDir); err != nil || busy {
		t.Fatalf("fresh agent folder must read idle, got busy=%v err=%v", busy, err)
	}

	release := holdSessionLock(t, agentDir)
	if busy, err := proc.SessionLockBusy(agentDir); err != nil || !busy {
		t.Errorf("expected the held session lock to read busy, got busy=%v err=%v", busy, err)
	}
	release()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		busy, err := proc.SessionLockBusy(agentDir)
		if err != nil {
			t.Fatalf("lock probe failed: %v", err)
		}
		if !busy {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Error("session lock still read busy after the holder exited")
}

// TestIdleWakeFiresWhenAgentIsIdle is the acceptance case: a job that finishes while the
// owning agent is not mid-turn wakes it, and the wake says which job it is about.
func TestIdleWakeFiresWhenAgentIsIdle(t *testing.T) {
	cwd := setupTestEnv(t)
	createExecutable(t, cwd, "wake-tool", "echo finished")
	dump := filepath.Join(cwd, "woke.txt")
	writeCompletionHook(t, cwd, "10-wake.sh", ""+
		"echo id=$WACKYPROC_PROC_ID >> \"$DUMP\"\n"+
		"echo status=$WACKYPROC_STATUS >> \"$DUMP\"\n"+
		"echo exit=$WACKYPROC_EXIT_CODE >> \"$DUMP\"\n")
	t.Setenv("DUMP", dump)

	id, err := proc.Run(cwd, "wake-tool", nil, nil)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}

	got, ok := waitForFile(t, dump, 8*time.Second)
	if !ok {
		hookLog, _ := os.ReadFile(filepath.Join(cwd, proc.ProcDirName, id, "hook.log"))
		t.Fatalf("hook never ran; hook.log: %s", string(hookLog))
	}
	if !strings.Contains(got, "id="+id) {
		t.Errorf("hook did not receive the proc id; saw:\n%s", got)
	}
	if !strings.Contains(got, "status="+proc.StatusCompleted) {
		t.Errorf("hook did not receive the terminal status; saw:\n%s", got)
	}
	noticeData, noticeErr := os.ReadFile(filepath.Join(cwd, proc.ProcDirName, id, "notice.txt"))
	if noticeErr != nil {
		t.Fatalf("the gate must leave the notice text for the woken turn: %v", noticeErr)
	}
	if !strings.Contains(string(noticeData), "wackyproc get "+id) {
		t.Errorf("notice must name the retrieval command, got %s", noticeData)
	}

	info, err := proc.Describe(cwd, id)
	if err != nil {
		t.Fatalf("describe failed: %v", err)
	}
	if !info.Notified {
		t.Error("firing the wake must mark the record notified")
	}
}

// TestIdleWakeSkipsWhileAgentIsMidTurn is P6: the busy case skips rather than queues, and
// the job stays un-notified so a manual read still surfaces it.
func TestIdleWakeSkipsWhileAgentIsMidTurn(t *testing.T) {
	cwd := setupTestEnv(t)
	createExecutable(t, cwd, "busy-tool", "echo finished")
	dump := filepath.Join(cwd, "woke-while-busy.txt")
	writeCompletionHook(t, cwd, "10-wake.sh", "echo woke >> \"$DUMP\"")
	t.Setenv("DUMP", dump)

	release := holdSessionLock(t, cwd)
	defer release()

	id, err := proc.Run(cwd, "busy-tool", nil, nil)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}

	hookLogPath := filepath.Join(cwd, proc.ProcDirName, id, "hook.log")
	got, ok := waitForFile(t, hookLogPath, 8*time.Second)
	if !ok {
		t.Fatalf("gate left no verdict in hook.log")
	}
	if !strings.Contains(got, "mid-turn") {
		t.Errorf("expected a skip-not-queue verdict, got:\n%s", got)
	}
	if data, err := os.ReadFile(dump); err == nil {
		t.Errorf("hook must not run while the agent is mid-turn, saw:\n%s", data)
	}

	info, err := proc.Describe(cwd, id)
	if err != nil {
		t.Fatalf("describe failed: %v", err)
	}
	if info.Notified {
		t.Error("a skipped wake must leave the job un-notified for the piggyback path")
	}
}

// TestManualReadSuppressesBothPaths is P7: a get read is notification, so the wake path
// must not surface the same completion again.
func TestManualReadSuppressesBothPaths(t *testing.T) {
	cwd := setupTestEnv(t)
	createExecutable(t, cwd, "read-tool", "echo result")
	dump := filepath.Join(cwd, "woke-after-read.txt")
	t.Setenv("DUMP", dump)

	id, err := proc.Run(cwd, "read-tool", nil, nil)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}

	terminal := false
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) && !terminal {
		list, err := proc.List(cwd)
		if err != nil {
			t.Fatalf("list failed: %v", err)
		}
		for _, p := range list {
			if p.ID == id && p.Status != proc.StatusRunning {
				terminal = true
			}
		}
		if !terminal {
			time.Sleep(25 * time.Millisecond)
		}
	}
	if !terminal {
		t.Fatalf("job %s never reached a terminal state", id)
	}

	if err := proc.Get(cwd, id, &noopWriter{}, &noopWriter{}); err != nil {
		t.Fatalf("get failed: %v", err)
	}
	info, err := proc.Describe(cwd, id)
	if err != nil {
		t.Fatalf("describe failed: %v", err)
	}
	if !info.Notified {
		t.Fatal("reading a terminal job result must mark it notified")
	}
	// Registered only now: the wake gate must be the only thing left that could surface
	// this job, and the manual read above already did.
	writeCompletionHook(t, cwd, "10-wake.sh", "echo woke >> \"$DUMP\"")

	result := proc.MaybeFireCompletionHooks(proc.Meta{ID: id, Cwd: cwd}, filepath.Join(cwd, proc.ProcDirName, id))
	if !strings.Contains(result.Skipped, "already notified") {
		t.Errorf("expected the notified flag to suppress the wake, got %+v", result)
	}
	if !result.Quiet {
		t.Errorf("an already-notified job must not write a log line, got %+v", result)
	}
	if data, err := os.ReadFile(dump); err == nil {
		t.Errorf("suppressed wake still ran the hook:\n%s", data)
	}
}

type noopWriter struct{}

func (noopWriter) Write(p []byte) (int, error) { return len(p), nil }

// TestWaitMarksCompletionAsNotified covers the other manual path: a successful wait hands
// the caller the completed job, which is notification, and must suppress the wake.
func TestWaitMarksCompletionAsNotified(t *testing.T) {
	cwd := setupTestEnv(t)
	createExecutable(t, cwd, "quick-tool", "echo done")

	id, err := proc.Run(cwd, "quick-tool", nil, nil)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	completed, err := proc.Wait(cwd, 10, id)
	if err != nil || completed != id {
		t.Fatalf("wait returned (%q, %v), want %q", completed, err, id)
	}
	info, err := proc.Describe(cwd, id)
	if err != nil {
		t.Fatalf("describe failed: %v", err)
	}
	if !info.Notified {
		t.Error("a wait that reported the completed job must mark it notified")
	}
}
