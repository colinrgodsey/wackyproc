package proc_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/colinrgodsey/wackyproc/proc"
)

func TestMain(m *testing.M) {
	// Intercept supervisor fork when running under 'go test'
	if len(os.Args) >= 3 && os.Args[1] == "__supervise" {
		if err := proc.Supervise(os.Args[2]); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func setupTestEnv(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	toolsDir := filepath.Join(dir, proc.ToolsDirName)
	if err := os.MkdirAll(toolsDir, 0755); err != nil {
		t.Fatalf("failed to create tools dir: %v", err)
	}
	return dir
}

func createExecutable(t *testing.T, dir string, name string, scriptContent string) string {
	t.Helper()
	path := filepath.Join(dir, proc.ToolsDirName, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+scriptContent+"\n"), 0755); err != nil {
		t.Fatalf("failed to create tool %s: %v", name, err)
	}
	return path
}

func TestRun_StrictPathResolution(t *testing.T) {
	cwd := setupTestEnv(t)

	// 'sh' exists in PATH, but not in ./tools/ -> must fail
	_, err := proc.Run(cwd, "sh", nil, nil)
	if err == nil {
		t.Fatalf("expected error running tool not in tools/, got nil")
	}
	if !strings.Contains(err.Error(), "no PATH fallback") {
		t.Errorf("expected 'no PATH fallback' error, got %v", err)
	}

	// Create tool in ./tools/ -> must succeed
	createExecutable(t, cwd, "mytool", "echo 'hello'")
	id, err := proc.Run(cwd, "mytool", nil, nil)
	if err != nil {
		t.Fatalf("expected success running tool in tools/, got %v", err)
	}
	if len(id) != proc.IDLength {
		t.Errorf("expected ID length %d, got %q", proc.IDLength, id)
	}
}

func TestRun_DetachedExecution_And_Get(t *testing.T) {
	cwd := setupTestEnv(t)

	createExecutable(t, cwd, "io-test", `
echo "stdout output line 1"
echo "stdout output line 2"
echo "stderr output line 1" >&2
`)

	id, err := proc.Run(cwd, "io-test", nil, nil)
	if err != nil {
		t.Fatalf("proc.Run failed: %v", err)
	}

	// Wait for process to complete
	completedID, err := proc.Wait(cwd, 5, id)
	if err != nil {
		t.Fatalf("proc.Wait failed: %v", err)
	}
	if completedID != id {
		t.Fatalf("expected completed ID %q, got %q", id, completedID)
	}

	var stdoutBuf, stderrBuf bytes.Buffer
	if err := proc.Get(cwd, id, &stdoutBuf, &stderrBuf); err != nil {
		t.Fatalf("proc.Get failed: %v", err)
	}

	stdoutStr := stdoutBuf.String()
	stderrStr := stderrBuf.String()

	if !strings.Contains(stdoutStr, "stdout output line 1") || !strings.Contains(stdoutStr, "stdout output line 2") {
		t.Errorf("unexpected stdout: %q", stdoutStr)
	}
	if !strings.Contains(stderrStr, "stderr output line 1") {
		t.Errorf("unexpected stderr: %q", stderrStr)
	}
}

func TestRun_SupervisedEnvVar_SetInChildren(t *testing.T) {
	cwd := setupTestEnv(t)

	// The tool prints the attested var and its own pid; the supervisor records its own
	// pid in supervisor_pid. The env value MUST be that supervisor pid (provenance),
	// not the tool's pid and not empty.
	createExecutable(t, cwd, "env-supervise", `
echo "SUPERVISED=$WACKYPROC_SUPERVISED"
echo "MYPID=$$"
`)

	id, err := proc.Run(cwd, "env-supervise", nil, nil)
	if err != nil {
		t.Fatalf("proc.Run failed: %v", err)
	}

	completedID, err := proc.Wait(cwd, 5, id)
	if err != nil {
		t.Fatalf("proc.Wait failed: %v", err)
	}
	if completedID != id {
		t.Fatalf("expected completed ID %q, got %q", id, completedID)
	}

	var stdoutBuf, stderrBuf bytes.Buffer
	if err := proc.Get(cwd, id, &stdoutBuf, &stderrBuf); err != nil {
		t.Fatalf("proc.Get failed: %v", err)
	}
	stdout := stdoutBuf.String()

	var supervised string
	var myPID string
	for _, line := range strings.Split(stdout, "\n") {
		if v, ok := strings.CutPrefix(line, "SUPERVISED="); ok {
			supervised = v
		}
		if v, ok := strings.CutPrefix(line, "MYPID="); ok {
			myPID = v
		}
	}

	if supervised == "" {
		t.Fatalf("WACKYPROC_SUPERVISED not present in child env; stdout: %q (stderr: %q)", stdout, stderrBuf.String())
	}
	supPID, err := strconv.Atoi(supervised)
	if err != nil || supPID <= 0 {
		t.Fatalf("WACKYPROC_SUPERVISED = %q, want a positive pid", supervised)
	}
	if supervised == myPID {
		t.Fatalf("WACKYPROC_SUPERVISED = %q must be the SUPERVISOR's pid, not the tool's own pid", supervised)
	}

	// The value must match the supervisor pid the machinery recorded for liveness.
	recorded := readSupervisorPidFile(cwd, id)
	if recorded <= 0 {
		t.Fatalf("supervisor_pid record missing/zero for %s", id)
	}
	if supPID != recorded {
		t.Fatalf("WACKYPROC_SUPERVISED = %d, want recorded supervisor pid %d", supPID, recorded)
	}
}

// readSupervisorPidFile reads .proc/<id>/supervisor_pid directly (external test package).
func readSupervisorPidFile(cwd, id string) int {
	data, err := os.ReadFile(filepath.Join(cwd, proc.ProcDirName, id, proc.SupervisorPIDFileName))
	if err != nil {
		return 0
	}
	val, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0
	}
	return val
}

func TestRun_WithStdin(t *testing.T) {
	cwd := setupTestEnv(t)

	createExecutable(t, cwd, "cat-stdin", `
read -r line
echo "RECEIVED: $line"
`)

	input := strings.NewReader("sample stdin payload\n")
	id, err := proc.Run(cwd, "cat-stdin", nil, input)
	if err != nil {
		t.Fatalf("proc.Run failed: %v", err)
	}

	completedID, err := proc.Wait(cwd, 5, id)
	if err != nil {
		t.Fatalf("proc.Wait failed: %v", err)
	}
	if completedID != id {
		t.Fatalf("expected completed ID %q, got %q", id, completedID)
	}

	var stdoutBuf, stderrBuf bytes.Buffer
	if err := proc.Get(cwd, id, &stdoutBuf, &stderrBuf); err != nil {
		t.Fatalf("proc.Get failed: %v", err)
	}

	if !strings.Contains(stdoutBuf.String(), "RECEIVED: sample stdin payload") {
		t.Errorf("expected stdin to be received by tool, got stdout: %q", stdoutBuf.String())
	}
}

func TestList_And_ExitStatus(t *testing.T) {
	cwd := setupTestEnv(t)

	createExecutable(t, cwd, "success-tool", "exit 0")
	createExecutable(t, cwd, "fail-tool", "exit 42")
	createExecutable(t, cwd, "sleep-tool", "sleep 0.5")

	idSleep, err := proc.Run(cwd, "sleep-tool", nil, nil)
	if err != nil {
		t.Fatalf("failed to run sleep-tool: %v", err)
	}

	idSuccess, err := proc.Run(cwd, "success-tool", nil, nil)
	if err != nil {
		t.Fatalf("failed to run success-tool: %v", err)
	}

	idFail, err := proc.Run(cwd, "fail-tool", nil, nil)
	if err != nil {
		t.Fatalf("failed to run fail-tool: %v", err)
	}

	// Give a moment for commands to execute
	time.Sleep(100 * time.Millisecond)

	list, err := proc.List(cwd)
	if err != nil {
		t.Fatalf("proc.List failed: %v", err)
	}

	if len(list) < 3 {
		t.Fatalf("expected at least 3 processes in list, got %d", len(list))
	}

	// Wait until all processes have finished running
	deadline := time.Now().Add(3 * time.Second)
	var listAfter []proc.ProcessInfo
	for time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		listAfter, err = proc.List(cwd)
		if err != nil {
			t.Fatalf("proc.List failed: %v", err)
		}
		allDone := true
		for _, p := range listAfter {
			if p.Status == proc.StatusRunning {
				allDone = false
				break
			}
		}
		if allDone {
			break
		}
	}

	for _, p := range listAfter {
		switch p.ID {
		case idSuccess:
			if p.Status != proc.StatusCompleted || p.ExitCode == nil || *p.ExitCode != 0 {
				t.Errorf("expected idSuccess completed with 0, got status=%s, exit=%v", p.Status, p.ExitCode)
			}
		case idFail:
			if p.Status != proc.StatusFailed || p.ExitCode == nil || *p.ExitCode != 42 {
				t.Errorf("expected idFail failed with 42, got status=%s, exit=%v", p.Status, p.ExitCode)
			}
		case idSleep:
			if p.Status != proc.StatusCompleted || p.ExitCode == nil || *p.ExitCode != 0 {
				t.Errorf("expected idSleep completed with 0, got status=%s, exit=%v", p.Status, p.ExitCode)
			}
		}
	}
}

func TestStop(t *testing.T) {
	cwd := setupTestEnv(t)

	createExecutable(t, cwd, "long-runner", `
trap 'exit 15' TERM
sleep 30
`)

	id, err := proc.Run(cwd, "long-runner", nil, nil)
	if err != nil {
		t.Fatalf("proc.Run failed: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	// Verify running
	list, err := proc.List(cwd)
	if err != nil || len(list) == 0 {
		t.Fatalf("proc.List failed: %v", err)
	}
	if list[0].Status != proc.StatusRunning {
		t.Fatalf("expected running status before stop, got %s", list[0].Status)
	}

	// Stop
	if err := proc.Stop(cwd, id, 2); err != nil {
		t.Fatalf("proc.Stop failed: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	listAfter, _ := proc.List(cwd)
	if len(listAfter) == 0 || listAfter[0].Status == proc.StatusRunning {
		t.Errorf("expected process to be stopped, got status %v", listAfter[0].Status)
	}
}

func TestCrashedDetection(t *testing.T) {
	cwd := setupTestEnv(t)

	createExecutable(t, cwd, "crash-test", `
sleep 30
`)

	_, err := proc.Run(cwd, "crash-test", nil, nil)
	if err != nil {
		t.Fatalf("proc.Run failed: %v", err)
	}

	time.Sleep(150 * time.Millisecond)

	list, err := proc.List(cwd)
	if err != nil || len(list) == 0 {
		t.Fatalf("proc.List failed: %v", err)
	}
	pid := list[0].PID
	if pid <= 0 {
		t.Fatalf("expected valid PID, got %d", pid)
	}

	// Kill with SIGKILL to simulate abrupt crash / OOM
	_ = syscall.Kill(pid, syscall.SIGKILL)
	time.Sleep(100 * time.Millisecond)

	listAfter, err := proc.List(cwd)
	if err != nil || len(listAfter) == 0 {
		t.Fatalf("proc.List after kill failed: %v", err)
	}

	// Liveness check should mark it FAILED or CRASHED
	if listAfter[0].Status != proc.StatusCrashed && listAfter[0].Status != proc.StatusFailed {
		t.Errorf("expected CRASHED or FAILED, got status %s", listAfter[0].Status)
	}
}

func TestPIDReuseDetection(t *testing.T) {
	cwd := setupTestEnv(t)

	// We simulate a PID that is alive (current process PID) but with a fake mismatched start time
	currentPID := os.Getpid()
	procID := "fake"
	procDir := filepath.Join(cwd, proc.ProcDirName, procID)
	_ = os.MkdirAll(procDir, 0755)

	meta := proc.Meta{
		ID:        procID,
		Tool:      "reused-proc",
		StartedAt: time.Now().Unix(),
		StartTime: "non-matching-historical-start-time",
	}
	metaBytes, _ := os.ReadFile(filepath.Join(procDir, proc.MetaFileName))
	_ = metaBytes

	_ = os.WriteFile(filepath.Join(procDir, proc.PIDFileName), []byte(strconv.Itoa(currentPID)+"\n"), 0644)

	liveness, err := proc.CheckLiveness(procDir, &meta)
	if err != nil {
		t.Fatalf("CheckLiveness failed: %v", err)
	}
	status, exitCode := liveness.Status, liveness.ExitCode

	// Should be marked CRASHED because the recorded start time doesn't match the current process start time
	if status != proc.StatusCrashed || exitCode != nil {
		t.Errorf("expected CRASHED with nil exitCode due to start time mismatch, got status=%s, exit=%v", status, exitCode)
	}
	if _, err := os.Stat(filepath.Join(procDir, proc.CrashedFileName)); os.IsNotExist(err) {
		t.Errorf("expected crashed marker file to exist")
	}
	if _, err := os.Stat(filepath.Join(procDir, proc.ExitCodeFileName)); !os.IsNotExist(err) {
		t.Errorf("expected exit_code file to not exist for observed crash")
	}
}

func TestWait_Timeout(t *testing.T) {
	cwd := setupTestEnv(t)

	createExecutable(t, cwd, "sleeper", `
sleep 10
`)
	id, err := proc.Run(cwd, "sleeper", nil, nil)
	if err != nil {
		t.Fatalf("proc.Run failed: %v", err)
	}

	// Wait 1 second (should timeout and return "")
	resID, err := proc.Wait(cwd, 1, id)
	if err != nil {
		t.Fatalf("proc.Wait failed: %v", err)
	}
	if resID != "" {
		t.Errorf("expected empty string on timeout, got %q", resID)
	}
}

func TestClaimUniqueProcessDir_AtomicCollision(t *testing.T) {
	procBaseDir := t.TempDir()

	// Claim 50 unique directories
	seen := make(map[string]bool)
	for i := 0; i < 50; i++ {
		id, dirPath, err := proc.ClaimUniqueProcessDir(procBaseDir)
		if err != nil {
			t.Fatalf("ClaimUniqueProcessDir failed: %v", err)
		}
		if seen[id] {
			t.Fatalf("duplicate ID returned: %s", id)
		}
		seen[id] = true

		if _, err := os.Stat(dirPath); os.IsNotExist(err) {
			t.Fatalf("expected directory %s to exist atomically", dirPath)
		}
	}
}

func TestRun_Concurrent(t *testing.T) {
	cwd := setupTestEnv(t)
	createExecutable(t, cwd, "quick-exit", "exit 0")

	const numProcs = 20
	idsChan := make(chan string, numProcs)
	errChan := make(chan error, numProcs)

	var wg sync.WaitGroup
	for i := 0; i < numProcs; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, err := proc.Run(cwd, "quick-exit", nil, nil)
			if err != nil {
				errChan <- err
				return
			}
			idsChan <- id
		}()
	}
	wg.Wait()
	close(idsChan)
	close(errChan)

	for err := range errChan {
		t.Fatalf("concurrent Run failed: %v", err)
	}

	seenIDs := make(map[string]bool)
	for id := range idsChan {
		if seenIDs[id] {
			t.Fatalf("concurrent collision detected: ID %s generated twice", id)
		}
		seenIDs[id] = true
	}
	if len(seenIDs) != numProcs {
		t.Fatalf("expected %d unique IDs, got %d", numProcs, len(seenIDs))
	}

	// Wait for all background supervisors to finish executing before test cleanup
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		list, err := proc.List(cwd)
		if err != nil {
			break
		}
		allDone := true
		for _, p := range list {
			if p.Status == proc.StatusRunning {
				allDone = false
				break
			}
		}
		if allDone && len(list) == numProcs {
			break
		}
	}
}

func TestSkill_FileContent(t *testing.T) {
	skillPath := filepath.Join("..", "skills", "wackyproc", "SKILL.md")
	content, err := os.ReadFile(skillPath)
	if err != nil {
		t.Fatalf("failed to read skill file %s: %v", skillPath, err)
	}
	skillStr := string(content)
	if !strings.Contains(skillStr, "name: wackyproc") {
		t.Errorf("expected skill to contain 'name: wackyproc'")
	}
	if !strings.Contains(skillStr, "# WackyProc Process Management & Long-Running Command Guide") {
		t.Errorf("expected skill to contain guide title")
	}
	if !strings.Contains(skillStr, "wackyproc wait 10 a1b2 c3d4") {
		t.Errorf("expected skill to document the first-completed wait form")
	}
	if !strings.Contains(skillStr, "--all") {
		t.Errorf("expected skill to document the --all barrier")
	}
	if !strings.Contains(skillStr, "the old bare any-mode wait is removed") {
		t.Errorf("expected skill to document the any-mode removal")
	}
	if strings.Contains(skillStr, "(recorded as 137)") {
		t.Errorf("expected skill to drop obsolete '(recorded as 137)'")
	}
}

func TestRun_RecursiveToolResolution(t *testing.T) {
	cwd := setupTestEnv(t)
	toolsDir := filepath.Join(cwd, proc.ToolsDirName)

	// 1. Nested subdirectory tool: tools/nested/sub/subtool
	subDir := filepath.Join(toolsDir, "nested", "sub")
	if err := os.MkdirAll(subDir, 0755); err != nil {
		t.Fatalf("MkdirAll failed: %v", err)
	}
	subToolPath := filepath.Join(subDir, "subtool")
	if err := os.WriteFile(subToolPath, []byte("#!/bin/sh\necho 'from subtool'\n"), 0755); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	// 2. External directory symlinked into tools/: tools/pack -> <externalDir>
	extDir := t.TempDir()
	extToolPath := filepath.Join(extDir, "packtool")
	if err := os.WriteFile(extToolPath, []byte("#!/bin/sh\necho 'from packtool'\n"), 0755); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	packSymlink := filepath.Join(toolsDir, "pack")
	if err := os.Symlink(extDir, packSymlink); err != nil {
		t.Fatalf("Symlink dir failed: %v", err)
	}

	// 3. File symlink directly in tools/: tools/symfile -> <extDir>/filetool
	extFileToolPath := filepath.Join(extDir, "filetool")
	if err := os.WriteFile(extFileToolPath, []byte("#!/bin/sh\necho 'from filetool'\n"), 0755); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	fileSymlink := filepath.Join(toolsDir, "symfile")
	if err := os.Symlink(extFileToolPath, fileSymlink); err != nil {
		t.Fatalf("Symlink file failed: %v", err)
	}

	// 4. Non-executable file in tools/ -> should fail
	nonExec := filepath.Join(toolsDir, "README.txt")
	_ = os.WriteFile(nonExec, []byte("plain text"), 0644)

	// Test case A: discover nested tool by base name
	id1, err := proc.Run(cwd, "subtool", nil, nil)
	if err != nil {
		t.Fatalf("failed to run nested subtool: %v", err)
	}
	if len(id1) != proc.IDLength {
		t.Errorf("expected ID length %d, got %s", proc.IDLength, id1)
	}

	// Test case B: direct relative path nested tool
	id2, err := proc.Run(cwd, "nested/sub/subtool", nil, nil)
	if err != nil {
		t.Fatalf("failed to run direct relative subtool: %v", err)
	}
	if len(id2) != proc.IDLength {
		t.Errorf("expected ID length %d, got %s", proc.IDLength, id2)
	}

	// Test case C: discover tool inside directory symlink by base name
	id3, err := proc.Run(cwd, "packtool", nil, nil)
	if err != nil {
		t.Fatalf("failed to run packtool from directory symlink: %v", err)
	}
	if len(id3) != proc.IDLength {
		t.Errorf("expected ID length %d, got %s", proc.IDLength, id3)
	}

	// Test case D: file symlink
	id4, err := proc.Run(cwd, "symfile", nil, nil)
	if err != nil {
		t.Fatalf("failed to run symfile: %v", err)
	}
	if len(id4) != proc.IDLength {
		t.Errorf("expected ID length %d, got %s", proc.IDLength, id4)
	}

	// Test case E: non-executable file -> should fail
	if _, err := proc.Run(cwd, "README.txt", nil, nil); err == nil {
		t.Fatalf("expected non-executable file to fail")
	}

	// Test case F: path traversal -> should fail
	if _, err := proc.Run(cwd, "../../bin/sh", nil, nil); err == nil {
		t.Fatalf("expected path traversal attempt to fail")
	}

	// Clean up background supervisors
	time.Sleep(100 * time.Millisecond)
}

func TestResolveToolPath_CollisionMatchesWackypubD14(t *testing.T) {
	cwd := setupTestEnv(t)
	toolsDir := filepath.Join(cwd, proc.ToolsDirName)

	// tools/foo (top-level) and tools/sub/foo (nested) share the same base name.
	topPath := createExecutable(t, cwd, "foo", "echo 'top-level foo'")

	subDir := filepath.Join(toolsDir, "sub")
	if err := os.MkdirAll(subDir, 0755); err != nil {
		t.Fatalf("MkdirAll failed: %v", err)
	}
	nestedPath := filepath.Join(subDir, "foo")
	if err := os.WriteFile(nestedPath, []byte("#!/bin/sh\necho 'nested foo'\n"), 0755); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	resolved, err := proc.ResolveToolPath(cwd, "foo")
	if err != nil {
		t.Fatalf("ResolveToolPath failed: %v", err)
	}

	// Must match wackypub's own DiscoverAgentToolsMap (D14): last-write-wins
	// during the walk, so the nested tool overrides the top-level one - not
	// the top-level file that happens to be a direct match.
	if resolved != nestedPath {
		t.Errorf("expected collision to resolve to nested tool %q (matching wackypub's D14 discovery), got %q (top-level was %q)", nestedPath, resolved, topPath)
	}
}

func TestRun_VariableArgsAndFlags(t *testing.T) {
	cwd := setupTestEnv(t)

	createExecutable(t, cwd, "arg-echo", `
for a in "$@"; do
  echo "ARG: $a"
done
`)

	args := []string{"-c", "sleep 1", "--port", "8080", "--verbose", "extra arg with spaces"}
	id, err := proc.Run(cwd, "arg-echo", args, nil)
	if err != nil {
		t.Fatalf("proc.Run failed: %v", err)
	}

	completedID, err := proc.Wait(cwd, 5, id)
	if err != nil {
		t.Fatalf("proc.Wait failed: %v", err)
	}
	if completedID != id {
		t.Fatalf("expected completed ID %q, got %q", id, completedID)
	}

	var stdoutBuf, stderrBuf bytes.Buffer
	if err := proc.Get(cwd, id, &stdoutBuf, &stderrBuf); err != nil {
		t.Fatalf("proc.Get failed: %v", err)
	}

	output := stdoutBuf.String()
	for _, expectedArg := range args {
		if !strings.Contains(output, "ARG: "+expectedArg) {
			t.Errorf("expected arg %q to be passed to tool, got output:\n%s", expectedArg, output)
		}
	}
}

func TestWait_For_ReportsAlreadyTerminalImmediately(t *testing.T) {
	cwd := setupTestEnv(t)

	createExecutable(t, cwd, "quick-target", `exit 0`)
	id, err := proc.Run(cwd, "quick-target", nil, nil)
	if err != nil {
		t.Fatalf("proc.Run failed: %v", err)
	}

	// Wait until target is completed
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		list, _ := proc.List(cwd)
		if len(list) > 0 && list[0].Status == proc.StatusCompleted {
			break
		}
	}

	type result struct {
		id  string
		err error
	}
	resCh := make(chan result, 1)
	go func() {
		gotID, waitErr := proc.Wait(cwd, 5, id)
		resCh <- result{id: gotID, err: waitErr}
	}()

	// Baseline exclusion must NOT apply here: must report immediately rather than blocking
	select {
	case res := <-resCh:
		if res.err != nil {
			t.Fatalf("proc.Wait failed: %v", res.err)
		}
		if res.id != id {
			t.Fatalf("expected ID %q, got %q", id, res.id)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected proc.Wait --for to return immediately on already-terminal process, but it blocked")
	}
}

func TestWait_For_UnknownID_FailsImmediately(t *testing.T) {
	cwd := setupTestEnv(t)

	type result struct {
		id  string
		err error
	}
	resCh := make(chan result, 1)
	go func() {
		gotID, waitErr := proc.Wait(cwd, 5, "zz99")
		resCh <- result{id: gotID, err: waitErr}
	}()

	// Must fail immediately with "process %q not found"
	select {
	case res := <-resCh:
		if res.err == nil {
			t.Fatal("expected error waiting for unknown process ID, got nil")
		}
		expectedErr := fmt.Sprintf("process %q not found", "zz99")
		if res.err.Error() != expectedErr {
			t.Fatalf("expected error %q, got %q", expectedErr, res.err.Error())
		}
	case <-time.After(50 * time.Millisecond):
		t.Fatal("expected proc.Wait --for on unknown ID to fail immediately, but it blocked")
	}
}

func TestWait_For_BlocksUntilTargetFinishes(t *testing.T) {
	cwd := setupTestEnv(t)

	triggerFile := filepath.Join(cwd, "trigger_for")
	createExecutable(t, cwd, "controlled-for", fmt.Sprintf(`
while [ ! -f %q ]; do
  sleep 0.02
done
exit 0
`, triggerFile))

	targetID, err := proc.Run(cwd, "controlled-for", nil, nil)
	if err != nil {
		t.Fatalf("proc.Run failed: %v", err)
	}

	type result struct {
		id  string
		err error
	}
	resCh := make(chan result, 1)
	go func() {
		gotID, waitErr := proc.Wait(cwd, 5, targetID)
		resCh <- result{id: gotID, err: waitErr}
	}()

	// Assert that Wait blocks while target is running
	select {
	case res := <-resCh:
		t.Fatalf("proc.Wait returned prematurely: id=%q, err=%v", res.id, res.err)
	case <-time.After(50 * time.Millisecond):
		// Expected: still blocked
	}

	// Release target process
	if err := os.WriteFile(triggerFile, []byte("release"), 0644); err != nil {
		t.Fatalf("failed to write trigger file: %v", err)
	}

	// Assert that Wait completes within bounded time and returns targetID
	select {
	case res := <-resCh:
		if res.err != nil {
			t.Fatalf("proc.Wait failed: %v", res.err)
		}
		if res.id != targetID {
			t.Fatalf("expected ID %q, got %q", targetID, res.id)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for proc.Wait --for to complete")
	}
}

// TestWait_BareRequiresIDs pins the removal of the bare any-mode wait: a wait
// with no process IDs fails fast instead of blocking the timeout.
func TestWait_BareRequiresIDs(t *testing.T) {
	cwd := setupTestEnv(t)

	start := time.Now()
	gotID, err := proc.Wait(cwd, 600)
	elapsed := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "at least one process ID") {
		t.Fatalf("expected a missing-IDs error, got id=%q err=%v", gotID, err)
	}
	if gotID != "" {
		t.Fatalf("expected empty ID, got %q", gotID)
	}
	if elapsed > 100*time.Millisecond {
		t.Errorf("expected immediate return, took %v (elapsed)", elapsed)
	}
}
func TestWait_FirstCompletedOfMany(t *testing.T) {
	cwd := setupTestEnv(t)
	createExecutable(t, cwd, "fast-tool", "sleep 0.2")
	createExecutable(t, cwd, "slow-tool", "sleep 1")

	fastID, err := proc.Run(cwd, "fast-tool", nil, nil)
	if err != nil {
		t.Fatalf("proc.Run fast failed: %v", err)
	}
	slowID, err := proc.Run(cwd, "slow-tool", nil, nil)
	if err != nil {
		t.Fatalf("proc.Run slow failed: %v", err)
	}

	start := time.Now()
	gotID, err := proc.Wait(cwd, 5, fastID, slowID)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("proc.Wait failed: %v", err)
	}
	if gotID != fastID {
		t.Errorf("expected first completed %q, got %q", fastID, gotID)
	}
	if elapsed > 800*time.Millisecond {
		t.Errorf("expected first-completed return well before the slow process finishes, took %v", elapsed)
	}
	if gotID, err := proc.Wait(cwd, 5, slowID); err != nil || gotID != slowID {
		t.Fatalf("expected slow ID back, got %q err=%v", gotID, err)
	}
}

func TestWaitAll_Barrier(t *testing.T) {
	cwd := setupTestEnv(t)
	createExecutable(t, cwd, "fast-tool", "sleep 0.2")
	createExecutable(t, cwd, "slow-tool", "sleep 1")

	fastID, err := proc.Run(cwd, "fast-tool", nil, nil)
	if err != nil {
		t.Fatalf("proc.Run fast failed: %v", err)
	}
	slowID, err := proc.Run(cwd, "slow-tool", nil, nil)
	if err != nil {
		t.Fatalf("proc.Run slow failed: %v", err)
	}

	start := time.Now()
	gotID, err := proc.WaitAll(cwd, 5, fastID, slowID)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("proc.WaitAll failed: %v", err)
	}
	if gotID != slowID {
		t.Errorf("expected last completed %q, got %q", slowID, gotID)
	}
	if elapsed < 800*time.Millisecond {
		t.Errorf("expected the barrier to hold for both processes, returned after only %v", elapsed)
	}
	if elapsed > 4*time.Second {
		t.Errorf("barrier took too long: %v", elapsed)
	}
}

func TestWait_AlreadyTerminalAtEntry(t *testing.T) {
	cwd := setupTestEnv(t)
	createExecutable(t, cwd, "quick-tool", "exit 0")
	id, err := proc.Run(cwd, "quick-tool", nil, nil)
	if err != nil {
		t.Fatalf("proc.Run failed: %v", err)
	}
	if _, err := proc.Wait(cwd, 5, id); err != nil {
		t.Fatalf("initial wait failed: %v", err)
	}

	start := time.Now()
	gotID, err := proc.Wait(cwd, 1, id)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("proc.Wait failed: %v", err)
	}
	if gotID != id {
		t.Errorf("expected %q, got %q", id, gotID)
	}
	if elapsed > 300*time.Millisecond {
		t.Errorf("expected immediate return for already-terminal entry, took %v", elapsed)
	}
}

func TestWait_UnknownID(t *testing.T) {
	cwd := setupTestEnv(t)
	start := time.Now()
	gotID, err := proc.Wait(cwd, 5, "zzzzzzzz")
	elapsed := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not-found error, got id=%q err=%v", gotID, err)
	}
	if gotID != "" {
		t.Fatalf("expected empty ID, got %q", gotID)
	}
	if elapsed > 100*time.Millisecond {
		t.Errorf("expected immediate failure, took %v", elapsed)
	}
}
func TestPeek_UnknownProcID(t *testing.T) {
	cwd := setupTestEnv(t)
	var stdoutBuf, stderrBuf bytes.Buffer

	err := proc.Peek(cwd, "xxxx", 20, &stdoutBuf, &stderrBuf)
	if err == nil {
		t.Fatal("expected error for unknown proc ID, got nil")
	}
	if !strings.Contains(err.Error(), `"xxxx"`) {
		t.Errorf("expected error mentioning 'xxxx', got: %v", err)
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected 'not found' in error, got: %v", err)
	}
}

func TestPeek_FewerLinesThanN(t *testing.T) {
	cwd := setupTestEnv(t)

	createExecutable(t, cwd, "fewer-lines", `
echo "stdout line 1"
echo "stdout line 2"
echo "stderr line 1" >&2
`)

	id, err := proc.Run(cwd, "fewer-lines", nil, nil)
	if err != nil {
		t.Fatalf("proc.Run failed: %v", err)
	}

	completedID, err := proc.Wait(cwd, 5, id)
	if err != nil {
		t.Fatalf("proc.Wait failed: %v", err)
	}
	if completedID != id {
		t.Fatalf("expected ID %q, got %q", id, completedID)
	}

	var stdoutBuf, stderrBuf bytes.Buffer
	if err := proc.Peek(cwd, id, 10, &stdoutBuf, &stderrBuf); err != nil {
		t.Fatalf("proc.Peek failed: %v", err)
	}

	expectedStdout := "stdout line 1\nstdout line 2\n"
	expectedStderr := "stderr line 1\n"
	if stdoutBuf.String() != expectedStdout {
		t.Errorf("stdout = %q, want %q", stdoutBuf.String(), expectedStdout)
	}
	if stderrBuf.String() != expectedStderr {
		t.Errorf("stderr = %q, want %q", stderrBuf.String(), expectedStderr)
	}
}

func TestPeek_MoreLinesThanN(t *testing.T) {
	cwd := setupTestEnv(t)

	createExecutable(t, cwd, "more-lines", `
for i in 1 2 3 4 5; do
  echo "out $i"
  echo "err $i" >&2
done
`)

	id, err := proc.Run(cwd, "more-lines", nil, nil)
	if err != nil {
		t.Fatalf("proc.Run failed: %v", err)
	}

	completedID, err := proc.Wait(cwd, 5, id)
	if err != nil {
		t.Fatalf("proc.Wait failed: %v", err)
	}
	if completedID != id {
		t.Fatalf("expected ID %q, got %q", id, completedID)
	}

	var stdoutBuf, stderrBuf bytes.Buffer
	// Request exactly last 2 lines
	if err := proc.Peek(cwd, id, 2, &stdoutBuf, &stderrBuf); err != nil {
		t.Fatalf("proc.Peek failed: %v", err)
	}

	expectedStdout := "out 4\nout 5\n"
	expectedStderr := "err 4\nerr 5\n"
	if stdoutBuf.String() != expectedStdout {
		t.Errorf("stdout = %q, want %q", stdoutBuf.String(), expectedStdout)
	}
	if stderrBuf.String() != expectedStderr {
		t.Errorf("stderr = %q, want %q", stderrBuf.String(), expectedStderr)
	}
}

func TestPeek_FinalPartialLineWithoutNewline(t *testing.T) {
	cwd := setupTestEnv(t)

	createExecutable(t, cwd, "partial-line", `
printf "line 1\nline 2\npartial ending"
`)

	id, err := proc.Run(cwd, "partial-line", nil, nil)
	if err != nil {
		t.Fatalf("proc.Run failed: %v", err)
	}

	completedID, err := proc.Wait(cwd, 5, id)
	if err != nil {
		t.Fatalf("proc.Wait failed: %v", err)
	}
	if completedID != id {
		t.Fatalf("expected ID %q, got %q", id, completedID)
	}

	// 1. Peek last 1 line -> should be exactly "partial ending"
	var stdout1, stderr1 bytes.Buffer
	if err := proc.Peek(cwd, id, 1, &stdout1, &stderr1); err != nil {
		t.Fatalf("proc.Peek(1) failed: %v", err)
	}
	if got := stdout1.String(); got != "partial ending" {
		t.Errorf("Peek(1) stdout = %q, want %q", got, "partial ending")
	}

	// 2. Peek last 2 lines -> should be "line 2\npartial ending"
	var stdout2, stderr2 bytes.Buffer
	if err := proc.Peek(cwd, id, 2, &stdout2, &stderr2); err != nil {
		t.Fatalf("proc.Peek(2) failed: %v", err)
	}
	if got := stdout2.String(); got != "line 2\npartial ending" {
		t.Errorf("Peek(2) stdout = %q, want %q", got, "line 2\npartial ending")
	}

	// 3. Peek with N > total lines -> should be full output
	var stdoutAll, stderrAll bytes.Buffer
	if err := proc.Peek(cwd, id, 10, &stdoutAll, &stderrAll); err != nil {
		t.Fatalf("proc.Peek(10) failed: %v", err)
	}
	if got := stdoutAll.String(); got != "line 1\nline 2\npartial ending" {
		t.Errorf("Peek(10) stdout = %q, want %q", got, "line 1\nline 2\npartial ending")
	}
}

func TestPeek_EmptyStreams(t *testing.T) {
	cwd := setupTestEnv(t)

	createExecutable(t, cwd, "silent-tool", `
exit 0
`)

	id, err := proc.Run(cwd, "silent-tool", nil, nil)
	if err != nil {
		t.Fatalf("proc.Run failed: %v", err)
	}

	completedID, err := proc.Wait(cwd, 5, id)
	if err != nil {
		t.Fatalf("proc.Wait failed: %v", err)
	}
	if completedID != id {
		t.Fatalf("expected ID %q, got %q", id, completedID)
	}

	var stdoutBuf, stderrBuf bytes.Buffer
	if err := proc.Peek(cwd, id, 20, &stdoutBuf, &stderrBuf); err != nil {
		t.Fatalf("proc.Peek failed: %v", err)
	}

	if stdoutBuf.Len() != 0 {
		t.Errorf("expected empty stdout, got: %q", stdoutBuf.String())
	}
	if stderrBuf.Len() != 0 {
		t.Errorf("expected empty stderr, got: %q", stderrBuf.String())
	}
}

func TestPeek_InvalidLinesParam(t *testing.T) {
	cwd := setupTestEnv(t)
	var stdoutBuf, stderrBuf bytes.Buffer

	for _, invalidLines := range []int{0, -1, -100} {
		err := proc.Peek(cwd, "xxxx", invalidLines, &stdoutBuf, &stderrBuf)
		if err == nil {
			t.Errorf("expected error for lines=%d, got nil", invalidLines)
		} else if !strings.Contains(err.Error(), "--lines must be >= 1") {
			t.Errorf("expected '--lines must be >= 1', got: %v", err)
		}
	}
}

func TestPeek_PureReader_NoStateModified(t *testing.T) {
	cwd := setupTestEnv(t)

	createExecutable(t, cwd, "state-tool", `
echo "test output"
`)

	id, err := proc.Run(cwd, "state-tool", nil, nil)
	if err != nil {
		t.Fatalf("proc.Run failed: %v", err)
	}

	completedID, err := proc.Wait(cwd, 5, id)
	if err != nil {
		t.Fatalf("proc.Wait failed: %v", err)
	}
	if completedID != id {
		t.Fatalf("expected ID %q, got %q", id, completedID)
	}

	procDir := filepath.Join(cwd, proc.ProcDirName, id)
	entriesBefore, err := os.ReadDir(procDir)
	if err != nil {
		t.Fatalf("readdir before: %v", err)
	}
	var filesBefore []string
	for _, e := range entriesBefore {
		filesBefore = append(filesBefore, e.Name())
	}

	metaBefore, err := os.ReadFile(filepath.Join(procDir, proc.MetaFileName))
	if err != nil {
		t.Fatalf("read meta: %v", err)
	}
	exitBefore, err := os.ReadFile(filepath.Join(procDir, proc.ExitCodeFileName))
	if err != nil {
		t.Fatalf("read exit_code: %v", err)
	}

	var stdoutBuf, stderrBuf bytes.Buffer
	if err := proc.Peek(cwd, id, 20, &stdoutBuf, &stderrBuf); err != nil {
		t.Fatalf("proc.Peek failed: %v", err)
	}

	entriesAfter, err := os.ReadDir(procDir)
	if err != nil {
		t.Fatalf("readdir after: %v", err)
	}
	var filesAfter []string
	for _, e := range entriesAfter {
		filesAfter = append(filesAfter, e.Name())
	}

	if strings.Join(filesBefore, ",") != strings.Join(filesAfter, ",") {
		t.Errorf("procDir directory listing modified by Peek: before=%v, after=%v", filesBefore, filesAfter)
	}

	metaAfter, err := os.ReadFile(filepath.Join(procDir, proc.MetaFileName))
	if err != nil {
		t.Fatalf("read meta after: %v", err)
	}
	exitAfter, err := os.ReadFile(filepath.Join(procDir, proc.ExitCodeFileName))
	if err != nil {
		t.Fatalf("read exit_code after: %v", err)
	}

	if string(metaBefore) != string(metaAfter) {
		t.Errorf("meta.json modified by Peek: before=%q, after=%q", string(metaBefore), string(metaAfter))
	}
	if string(exitBefore) != string(exitAfter) {
		t.Errorf("exit_code modified by Peek: before=%q, after=%q", string(exitBefore), string(exitAfter))
	}

	if string(metaBefore) != string(metaAfter) {
		t.Errorf("Peek modified meta.json: before=%s after=%s", metaBefore, metaAfter)
	}
}

func seedProcessRecord(t *testing.T, cwd string, id string, tool string, status string, gen uint64) {
	t.Helper()
	procDir := filepath.Join(cwd, proc.ProcDirName, id)
	if err := os.MkdirAll(procDir, 0755); err != nil {
		t.Fatalf("failed to create proc dir %s: %v", id, err)
	}
	meta := proc.Meta{
		ID:        id,
		Tool:      tool,
		StartedAt: time.Now().Unix(),
		Gen:       gen,
	}
	metaData, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		t.Fatalf("failed to marshal meta: %v", err)
	}
	if err := os.WriteFile(filepath.Join(procDir, proc.MetaFileName), metaData, 0644); err != nil {
		t.Fatalf("failed to write meta: %v", err)
	}

	if status != proc.StatusRunning {
		if status == proc.StatusCrashed {
			_ = os.WriteFile(filepath.Join(procDir, proc.CrashedFileName), []byte(""), 0644)
		} else {
			exitCode := 0
			if status == proc.StatusFailed {
				exitCode = 1
			}
			_ = os.WriteFile(filepath.Join(procDir, proc.ExitCodeFileName), []byte(fmt.Sprintf("%d\n", exitCode)), 0644)
		}
	} else {
		// Running process: point PID to current process so kill -0 returns 0 (running)
		pid := os.Getpid()
		_ = os.WriteFile(filepath.Join(procDir, proc.PIDFileName), []byte(fmt.Sprintf("%d\n", pid)), 0644)
	}
	_ = os.WriteFile(filepath.Join(procDir, proc.StdoutFileName), []byte("stdout\n"), 0644)
	_ = os.WriteFile(filepath.Join(procDir, proc.StderrFileName), []byte("stderr\n"), 0644)
}

func TestGet_MarksNothing(t *testing.T) {
	cwd := setupTestEnv(t)
	createExecutable(t, cwd, "quick-tool", "echo 'hello world'")

	id, err := proc.Run(cwd, "quick-tool", nil, nil)
	if err != nil {
		t.Fatalf("proc.Run failed: %v", err)
	}
	if _, err := proc.Wait(cwd, 5, id); err != nil {
		t.Fatalf("waiting for terminal: %v", err)
	}

	metaPath := filepath.Join(cwd, proc.ProcDirName, id, proc.MetaFileName)
	metaBefore, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("read meta: %v", err)
	}

	var stdoutBuf, stderrBuf bytes.Buffer
	if err := proc.Get(cwd, id, &stdoutBuf, &stderrBuf); err != nil {
		t.Fatalf("proc.Get failed: %v", err)
	}
	if got := strings.TrimSpace(stdoutBuf.String()); got != "hello world" {
		t.Errorf("expected stdout %q, got %q", "hello world", got)
	}

	// Get is a pure stream: the on-disk record is byte-identical afterwards.
	metaAfter, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("re-read meta: %v", err)
	}
	if string(metaBefore) != string(metaAfter) {
		t.Errorf("Get modified meta.json: before=%s after=%s", metaBefore, metaAfter)
	}

	// The cap still retires old records with no consumed state: seed the store
	// past the cap with this record as the oldest, then let List self-heal.
	// The record must be the one retired.
	for i := 1; i <= proc.MaxTerminalEntries; i++ {
		seedProcessRecord(t, cwd, fmt.Sprintf("z%03d", i), "tool-seed", proc.StatusCompleted, uint64(i)+1)
	}
	oldStderr := os.Stderr
	rPipe, wPipe, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stderr = wPipe
	list, listErr := proc.List(cwd)
	wPipe.Close()
	os.Stderr = oldStderr
	if listErr != nil {
		t.Fatalf("proc.List failed: %v", listErr)
	}
	var retireErr bytes.Buffer
	_, _ = io.Copy(&retireErr, rPipe)
	if !strings.Contains(retireErr.String(), fmt.Sprintf("retired terminal record %s (cap %d)", id, proc.MaxTerminalEntries)) {
		t.Errorf("expected the oldest record to be retired, stderr: %s", retireErr.String())
	}
	if len(list) != proc.MaxTerminalEntries {
		t.Fatalf("expected List to self-heal to %d records, got %d", proc.MaxTerminalEntries, len(list))
	}
	for _, p := range list {
		if p.ID == id {
			t.Fatalf("expected the oldest (gen 1) record to be retired by the cap, but %s is still listed", id)
		}
	}
}

func TestSeq_ConcurrentAndGenOrdering(t *testing.T) {
	cwd := setupTestEnv(t)
	procBaseDir := filepath.Join(cwd, proc.ProcDirName)
	if err := os.MkdirAll(procBaseDir, 0755); err != nil {
		t.Fatalf("failed to create procBaseDir: %v", err)
	}

	// 1. N concurrent NextSeq calls yield N distinct values
	const n = 50
	var wg sync.WaitGroup
	var mu sync.Mutex
	seqs := make(map[uint64]bool)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			seq, err := proc.NextSeq(procBaseDir)
			if err != nil {
				t.Errorf("NextSeq failed: %v", err)
				return
			}
			mu.Lock()
			seqs[seq] = true
			mu.Unlock()
		}()
	}
	wg.Wait()

	if len(seqs) != n {
		t.Errorf("expected %d distinct sequence numbers, got %d", n, len(seqs))
	}

	// 2. Gen ordering: two back-to-back spawns get distinct, ascending Gen even within the same second
	createExecutable(t, cwd, "quick-tool", "echo 1")
	idA, err := proc.Run(cwd, "quick-tool", nil, nil)
	if err != nil {
		t.Fatalf("first proc.Run failed: %v", err)
	}
	idB, err := proc.Run(cwd, "quick-tool", nil, nil)
	if err != nil {
		t.Fatalf("second proc.Run failed: %v", err)
	}

	metaBytesA, _ := os.ReadFile(filepath.Join(procBaseDir, idA, proc.MetaFileName))
	metaBytesB, _ := os.ReadFile(filepath.Join(procBaseDir, idB, proc.MetaFileName))
	var metaA, metaB proc.Meta
	_ = json.Unmarshal(metaBytesA, &metaA)
	_ = json.Unmarshal(metaBytesB, &metaB)

	if metaB.Gen <= metaA.Gen {
		t.Errorf("expected metaB.Gen (%d) > metaA.Gen (%d)", metaB.Gen, metaA.Gen)
	}
}

func TestCapRetiresOldestRegardless(t *testing.T) {
	t.Run("run_growth_point", func(t *testing.T) {
		cwd := setupTestEnv(t)
		createExecutable(t, cwd, "quick-tool", "echo 1")

		for i := 1; i <= 105; i++ {
			seedProcessRecord(t, cwd, fmt.Sprintf("x%03d", i), "tool-seed", proc.StatusCompleted, uint64(i))
		}
		seedProcessRecord(t, cwd, "r001", "tool-running", proc.StatusRunning, 106)

		// The seq counter allocates the new record's Gen; seed it past the
		// explicit Gens above so the fresh record is the newest.
		if err := os.WriteFile(filepath.Join(cwd, proc.ProcDirName, proc.SeqFileName), []byte("106\n"), 0644); err != nil {
			t.Fatalf("seed .seq: %v", err)
		}

		oldStderr := os.Stderr
		rPipe, wPipe, err := os.Pipe()
		if err != nil {
			t.Fatalf("os.Pipe: %v", err)
		}
		os.Stderr = wPipe
		newID, runErr := proc.Run(cwd, "quick-tool", nil, nil)
		wPipe.Close()
		os.Stderr = oldStderr
		if runErr != nil {
			t.Fatalf("proc.Run failed: %v", runErr)
		}
		var stderrBuf bytes.Buffer
		_, _ = io.Copy(&stderrBuf, rPipe)
		stderrOut := stderrBuf.String()

		// Exactly the five oldest terminal records are retired at the growth point.
		for i := 1; i <= 5; i++ {
			if !strings.Contains(stderrOut, fmt.Sprintf("retired terminal record x%03d (cap %d)", i, proc.MaxTerminalEntries)) {
				t.Errorf("expected retirement line for x%03d, got: %s", i, stderrOut)
			}
		}
		if strings.Contains(stderrOut, "retired terminal record x006") {
			t.Errorf("x006 must not have been retired: %s", stderrOut)
		}
		if strings.Contains(stderrOut, "warning") || strings.Contains(stderrOut, "0 disposable") {
			t.Errorf("expected no warning spam, got: %s", stderrOut)
		}

		list, err := proc.List(cwd)
		if err != nil {
			t.Fatalf("proc.List failed: %v", err)
		}
		byID := map[string]bool{}
		for _, p := range list {
			byID[p.ID] = true
		}
		for i := 1; i <= 5; i++ {
			if byID[fmt.Sprintf("x%03d", i)] {
				t.Errorf("x%03d should have been retired", i)
			}
		}
		if !byID["r001"] {
			t.Errorf("running record r001 must not be retired")
		}
		if !byID[newID] {
			t.Errorf("newly created record %s missing from list", newID)
		}
	})

	t.Run("list_self_heal", func(t *testing.T) {
		cwd := setupTestEnv(t)
		for i := 1; i <= 105; i++ {
			seedProcessRecord(t, cwd, fmt.Sprintf("y%03d", i), "tool-seed", proc.StatusCompleted, uint64(i))
		}
		oldStderr := os.Stderr
		rPipe, wPipe, err := os.Pipe()
		if err != nil {
			t.Fatalf("os.Pipe: %v", err)
		}
		os.Stderr = wPipe
		list, listErr := proc.List(cwd)
		wPipe.Close()
		os.Stderr = oldStderr
		if listErr != nil {
			t.Fatalf("proc.List failed: %v", listErr)
		}
		var stderrBuf bytes.Buffer
		_, _ = io.Copy(&stderrBuf, rPipe)
		if len(list) != proc.MaxTerminalEntries {
			t.Fatalf("expected List to self-heal to %d records, got %d", proc.MaxTerminalEntries, len(list))
		}
		for i := 1; i <= 5; i++ {
			for _, p := range list {
				if p.ID == fmt.Sprintf("y%03d", i) {
					t.Errorf("y%03d should have been retired", i)
				}
			}
		}
		if s := stderrBuf.String(); strings.Contains(s, "warning") || strings.Contains(s, "0 disposable") {
			t.Errorf("expected no warning spam, got: %s", s)
		}
	})
}

func TestPrune(t *testing.T) {
	cwd := setupTestEnv(t)
	procBaseDir := filepath.Join(cwd, proc.ProcDirName)
	_ = os.MkdirAll(procBaseDir, 0755)

	// Seed 3 terminal records (mix consumed and unconsumed) and 1 running record
	seedProcessRecord(t, cwd, "t001", "tool-a", proc.StatusCompleted, 1)
	seedProcessRecord(t, cwd, "t002", "tool-b", proc.StatusFailed, 2)
	seedProcessRecord(t, cwd, "t003", "tool-c", proc.StatusCrashed, 3)
	seedProcessRecord(t, cwd, "r001", "tool-run", proc.StatusRunning, 4)

	// Seed non-record items
	_ = os.MkdirAll(filepath.Join(procBaseDir, proc.SeqLockDirName), 0755)
	_ = os.WriteFile(filepath.Join(procBaseDir, proc.SeqFileName), []byte("4\n"), 0644)

	var report bytes.Buffer
	if err := proc.Prune(cwd, &report); err != nil {
		t.Fatalf("proc.Prune failed: %v", err)
	}

	// Terminal records must be gone
	for _, id := range []string{"t001", "t002", "t003"} {
		if _, err := os.Stat(filepath.Join(procBaseDir, id)); !os.IsNotExist(err) {
			t.Errorf("expected terminal record %s to be pruned, but it still exists", id)
		}
	}
	// Running record must still exist
	if _, err := os.Stat(filepath.Join(procBaseDir, "r001")); os.IsNotExist(err) {
		t.Errorf("expected running record r001 to remain, but it was deleted")
	}
	// Non-record items must remain
	if _, err := os.Stat(filepath.Join(procBaseDir, proc.SeqLockDirName)); os.IsNotExist(err) {
		t.Errorf("expected .seq.lock to remain, but it was deleted")
	}
	if _, err := os.Stat(filepath.Join(procBaseDir, proc.SeqFileName)); os.IsNotExist(err) {
		t.Errorf("expected .seq to remain, but it was deleted")
	}

	reportStr := report.String()
	for _, id := range []string{"t001", "t002", "t003"} {
		if !strings.Contains(reportStr, id) {
			t.Errorf("expected prune report to contain %s, got: %q", id, reportStr)
		}
	}

	// Empty .proc no-op
	report.Reset()
	if err := proc.Prune(cwd, &report); err != nil {
		t.Fatalf("second proc.Prune failed: %v", err)
	}
	if report.Len() != 0 {
		t.Errorf("expected empty report on second prune, got: %q", report.String())
	}
}

func TestGet_JustRemovedRecordDir(t *testing.T) {
	cwd := setupTestEnv(t)
	createExecutable(t, cwd, "quick-tool", "echo 'quick'")

	id, err := proc.Run(cwd, "quick-tool", nil, nil)
	if err != nil {
		t.Fatalf("proc.Run failed: %v", err)
	}
	_, _ = proc.Wait(cwd, 5, id)

	// Simulate concurrent disposal right before Get
	_ = os.RemoveAll(filepath.Join(cwd, proc.ProcDirName, id))

	var stdoutBuf, stderrBuf bytes.Buffer
	err = proc.Get(cwd, id, &stdoutBuf, &stderrBuf)
	if err == nil {
		t.Fatalf("expected error getting just-removed process, got nil")
	}
	expectedMsg := fmt.Sprintf("process %q not found", id)
	if !strings.Contains(err.Error(), expectedMsg) {
		t.Errorf("expected error %q, got: %v", expectedMsg, err)
	}
}

func TestGet_MissingIndividualFiles(t *testing.T) {
	// 1. Missing stdout: procDir exists, stderr and meta.json exist, stdout removed.
	// Documented behavior: stdout write is skipped, Get succeeds, meta marked consumed.
	{
		cwd := setupTestEnv(t)
		createExecutable(t, cwd, "tool-out", "echo out; echo err >&2")
		id, err := proc.Run(cwd, "tool-out", nil, nil)
		if err != nil {
			t.Fatalf("proc.Run failed: %v", err)
		}
		_, _ = proc.Wait(cwd, 5, id)

		procDir := filepath.Join(cwd, proc.ProcDirName, id)
		_ = os.Remove(filepath.Join(procDir, proc.StdoutFileName))

		var stdoutBuf, stderrBuf bytes.Buffer
		err = proc.Get(cwd, id, &stdoutBuf, &stderrBuf)
		if err != nil {
			t.Errorf("expected Get with missing stdout to succeed, got: %v", err)
		}
		if stdoutBuf.Len() != 0 {
			t.Errorf("expected empty stdout, got: %q", stdoutBuf.String())
		}
		if !strings.Contains(stderrBuf.String(), "err") {
			t.Errorf("expected stderr to contain 'err', got: %q", stderrBuf.String())
		}
	}

	// 2. Missing stderr: procDir exists, stdout and meta.json exist, stderr removed.
	// Documented behavior: stderr write is skipped, Get succeeds, meta marked consumed.
	{
		cwd := setupTestEnv(t)
		createExecutable(t, cwd, "tool-err", "echo out; echo err >&2")
		id, err := proc.Run(cwd, "tool-err", nil, nil)
		if err != nil {
			t.Fatalf("proc.Run failed: %v", err)
		}
		_, _ = proc.Wait(cwd, 5, id)

		procDir := filepath.Join(cwd, proc.ProcDirName, id)
		_ = os.Remove(filepath.Join(procDir, proc.StderrFileName))

		var stdoutBuf, stderrBuf bytes.Buffer
		err = proc.Get(cwd, id, &stdoutBuf, &stderrBuf)
		if err != nil {
			t.Errorf("expected Get with missing stderr to succeed, got: %v", err)
		}
		if !strings.Contains(stdoutBuf.String(), "out") {
			t.Errorf("expected stdout to contain 'out', got: %q", stdoutBuf.String())
		}
		if stderrBuf.Len() != 0 {
			t.Errorf("expected empty stderr, got: %q", stderrBuf.String())
		}
	}

	// 3. Missing meta.json (e.g. concurrent disposal between initial stat and meta read):
	// procDir exists, stdout exists, but meta.json removed.
	// Documented behavior: must return 'process "<id>" not found' error.
	{
		cwd := setupTestEnv(t)
		createExecutable(t, cwd, "tool-meta", "echo out")
		id, err := proc.Run(cwd, "tool-meta", nil, nil)
		if err != nil {
			t.Fatalf("proc.Run failed: %v", err)
		}
		_, _ = proc.Wait(cwd, 5, id)

		procDir := filepath.Join(cwd, proc.ProcDirName, id)
		_ = os.Remove(filepath.Join(procDir, proc.MetaFileName))

		var stdoutBuf, stderrBuf bytes.Buffer
		err = proc.Get(cwd, id, &stdoutBuf, &stderrBuf)
		if err == nil {
			t.Fatalf("expected error for missing meta.json, got nil")
		}
		expectedMsg := fmt.Sprintf("process %q not found", id)
		if !strings.Contains(err.Error(), expectedMsg) {
			t.Errorf("expected %q error, got: %v", expectedMsg, err)
		}
	}
}

func TestCrashMarker_Lifecycle(t *testing.T) {
	cwd := setupTestEnv(t)

	// 1. Crash observed -> marker written, CRASHED reported on every subsequent list/wait
	procID := "crsh"
	procDir := filepath.Join(cwd, proc.ProcDirName, procID)
	if err := os.MkdirAll(procDir, 0755); err != nil {
		t.Fatalf("failed to create procDir: %v", err)
	}

	deadPID := 999999
	_ = os.WriteFile(filepath.Join(procDir, proc.PIDFileName), []byte(fmt.Sprintf("%d\n", deadPID)), 0644)
	meta := proc.Meta{
		ID:        procID,
		Tool:      "crashed-tool",
		StartedAt: time.Now().Unix(),
	}
	metaData, _ := json.Marshal(meta)
	_ = os.WriteFile(filepath.Join(procDir, proc.MetaFileName), metaData, 0644)

	// Initial check: should observe crash, write marker, and return CRASHED with nil exitCode
	liveness, err := proc.CheckLiveness(procDir, &meta)
	if err != nil {
		t.Fatalf("CheckLiveness failed: %v", err)
	}
	status, exitCode := liveness.Status, liveness.ExitCode
	if status != proc.StatusCrashed {
		t.Errorf("expected status %s, got %s", proc.StatusCrashed, status)
	}
	if exitCode != nil {
		t.Errorf("expected exitCode nil, got %v", exitCode)
	}

	// Verify marker exists on disk and exit_code file was NOT written
	crashedPath := filepath.Join(procDir, proc.CrashedFileName)
	if _, err := os.Stat(crashedPath); os.IsNotExist(err) {
		t.Errorf("expected crashed marker file %s to exist", crashedPath)
	}
	exitCodePath := filepath.Join(procDir, proc.ExitCodeFileName)
	if _, err := os.Stat(exitCodePath); !os.IsNotExist(err) {
		t.Errorf("expected exit_code file to not exist for observed crash")
	}

	// Subsequent list: CRASHED reported
	list, err := proc.List(cwd)
	if err != nil {
		t.Fatalf("proc.List failed: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 process in list, got %d", len(list))
	}
	if list[0].Status != proc.StatusCrashed {
		t.Errorf("expected list status %s, got %s", proc.StatusCrashed, list[0].Status)
	}
	if list[0].ExitCode != nil {
		t.Errorf("expected list exitCode nil, got %v", list[0].ExitCode)
	}

	// Subsequent wait: CRASHED reported immediately
	waitedID, err := proc.Wait(cwd, 5, procID)
	if err != nil {
		t.Fatalf("proc.Wait failed: %v", err)
	}
	if waitedID != procID {
		t.Errorf("expected wait to return %s, got %s", procID, waitedID)
	}

	// 2. Marker survives process record restarts (fresh check from on-disk state)
	var reloadedMeta proc.Meta
	metaBytes, err := os.ReadFile(filepath.Join(procDir, proc.MetaFileName))
	if err != nil {
		t.Fatalf("failed to read meta: %v", err)
	}
	if err := json.Unmarshal(metaBytes, &reloadedMeta); err != nil {
		t.Fatalf("failed to unmarshal meta: %v", err)
	}

	recheckLiveness, err := proc.CheckLiveness(procDir, &reloadedMeta)
	if err != nil {
		t.Fatalf("CheckLiveness failed on re-check: %v", err)
	}
	recheckStatus, recheckExitCode := recheckLiveness.Status, recheckLiveness.ExitCode
	if recheckStatus != proc.StatusCrashed {
		t.Errorf("expected recheckStatus %s after restart, got %s", proc.StatusCrashed, recheckStatus)
	}
	if recheckExitCode != nil {
		t.Errorf("expected recheckExitCode nil after restart, got %v", recheckExitCode)
	}

	// And verify list reloads and sees CRASHED
	recheckList, err := proc.List(cwd)
	if err != nil {
		t.Fatalf("proc.List failed: %v", err)
	}
	var foundCrashed bool
	for _, p := range recheckList {
		if p.ID == procID {
			foundCrashed = true
			if p.Status != proc.StatusCrashed {
				t.Errorf("expected List status %s for restarted record, got %s", proc.StatusCrashed, p.Status)
			}
			if p.ExitCode != nil {
				t.Errorf("expected List exitCode nil for restarted record, got %v", p.ExitCode)
			}
		}
	}
	if !foundCrashed {
		t.Errorf("expected to find %s in list", procID)
	}

	// 3. Honest SIGKILL exit (137 without marker) -> FAILED
	sigkillID := "skil"
	sigkillDir := filepath.Join(cwd, proc.ProcDirName, sigkillID)
	if err := os.MkdirAll(sigkillDir, 0755); err != nil {
		t.Fatalf("failed to create sigkillDir: %v", err)
	}
	_ = os.WriteFile(filepath.Join(sigkillDir, proc.PIDFileName), []byte(fmt.Sprintf("%d\n", deadPID)), 0644)
	_ = os.WriteFile(filepath.Join(sigkillDir, proc.ExitCodeFileName), []byte("137\n"), 0644)
	sigkillMeta := proc.Meta{
		ID:        sigkillID,
		Tool:      "killed-tool",
		StartedAt: time.Now().Unix(),
	}
	sigkillMetaData, _ := json.Marshal(sigkillMeta)
	_ = os.WriteFile(filepath.Join(sigkillDir, proc.MetaFileName), sigkillMetaData, 0644)

	skLiveness, err := proc.CheckLiveness(sigkillDir, &sigkillMeta)
	if err != nil {
		t.Fatalf("CheckLiveness for SIGKILL failed: %v", err)
	}
	skStatus, skExitCode := skLiveness.Status, skLiveness.ExitCode
	if skStatus != proc.StatusFailed {
		t.Errorf("expected status %s for 137 without marker, got %s", proc.StatusFailed, skStatus)
	}
	if skExitCode == nil || *skExitCode != 137 {
		t.Errorf("expected exit code 137, got %v", skExitCode)
	}

	skList, err := proc.List(cwd)
	if err != nil {
		t.Fatalf("proc.List failed: %v", err)
	}
	var foundSIGKILL bool
	for _, p := range skList {
		if p.ID == sigkillID {
			foundSIGKILL = true
			if p.Status != proc.StatusFailed {
				t.Errorf("expected List status %s for 137 without marker, got %s", proc.StatusFailed, p.Status)
			}
			if p.ExitCode == nil || *p.ExitCode != 137 {
				t.Errorf("expected List exit code 137, got %v", p.ExitCode)
			}
		}
	}
	if !foundSIGKILL {
		t.Errorf("expected to find %s in list", sigkillID)
	}
}

// TestIsProcessRecordDir_LegacyAndSlug verifies the record-dir gate accepts both the
// legacy 4-char [0-9a-z] token and the new 8-char lowercase slug, and rejects non-record
// dirs.
func TestIsProcessRecordDir_LegacyAndSlug(t *testing.T) {
	for _, ok := range []string{"gk92", "katoruvo"} {
		if !proc.IsProcessRecordDir(ok) {
			t.Errorf("IsProcessRecordDir(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"", "abc", "abcdE", "a1b2c3d4e5", ".seq.lock", "tmp-"} {
		if proc.IsProcessRecordDir(bad) {
			t.Errorf("IsProcessRecordDir(%q) = true, want false", bad)
		}
	}
}

// TestList_CompactJSONExcludesArgs pins the list --json contract from
// tasks/wackyproc/list-describe-split: list JSON must stay compact no matter how large
// per-command args are (agent prompts). Args belong to Describe, never to list.
func TestList_CompactJSONExcludesArgs(t *testing.T) {
	cwd := setupTestEnv(t)
	createExecutable(t, cwd, "argtool", "exit 0")

	bigArg := strings.Repeat("p", 200*1024)
	if _, err := proc.Run(cwd, "argtool", []string{bigArg}, nil); err != nil {
		t.Fatalf("run argtool: %v", err)
	}

	list, err := proc.List(cwd)
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	data, err := json.Marshal(list)
	if err != nil {
		t.Fatalf("marshal list: %v", err)
	}

	// The 200KB arg MUST NOT appear in list JSON - the whole output must stay small.
	if strings.Contains(string(data), "ppppppppp") {
		t.Fatal("list JSON contains the large arg - compact contract violated")
	}
	if len(data) > 16*1024 {
		t.Fatalf("list JSON too large: %d bytes for a single record (200KB arg should not add size)", len(data))
	}

	// Compact shape: table fields + timestamps only.
	var decoded []map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal list: %v", err)
	}
	if len(decoded) == 0 {
		t.Fatal("expected records")
	}
	for _, rec := range decoded {
		if _, hasArgs := rec["args"]; hasArgs {
			t.Errorf("list JSON must not include args, got %v", rec)
		}
		for _, want := range []string{"id", "tool", "status", "started_at"} {
			if _, ok := rec[want]; !ok {
				t.Errorf("list JSON missing %q, got %v", want, rec)
			}
		}
	}
}

// TestDescribe_FullDetailsAndDoesNotConsume verifies describe returns complete args +
// metadata and never marks the record consumed (Get is the only consumer).
func TestDescribe_FullDetailsAndDoesNotConsume(t *testing.T) {
	cwd := setupTestEnv(t)
	createExecutable(t, cwd, "bigtool", "exit 0")

	args := []string{"--prompt", strings.Repeat("p", 5000), "second"}
	id, err := proc.Run(cwd, "bigtool", args, nil)
	if err != nil {
		t.Fatalf("run bigtool: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	info, err := proc.Describe(cwd, id)
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if info.ID != id {
		t.Errorf("describe id = %q, want %q", info.ID, id)
	}
	if len(info.Args) != len(args) || info.Args[0] != "--prompt" {
		t.Errorf("describe args = %v, want %v", info.Args, args)
	}
	if info.Cwd != cwd {
		t.Errorf("describe cwd = %q, want %q", info.Cwd, cwd)
	}
	if info.Status == "" {
		t.Errorf("describe status empty")
	}
	if info.StdoutFile == "" || info.StderrFile == "" {
		t.Errorf("expected output file locations, got stdout=%q stderr=%q", info.StdoutFile, info.StderrFile)
	}

}

// TestDescribe_NotFound verifies describe errors cleanly on a missing id.
func TestDescribe_NotFound(t *testing.T) {
	cwd := setupTestEnv(t)
	if _, err := proc.Describe(cwd, "no-such-record"); err == nil {
		t.Fatal("expected error describing missing record")
	}
}

// TestWait_ShortTaskLatency is the ramp acceptance for short tasks: a task finishing
// in ~200ms is still reported promptly, because the ramp starts at the 100ms interval
// (detection within ~2 intervals of completion).
func TestWait_ShortTaskLatency(t *testing.T) {
	cwd := setupTestEnv(t)

	createExecutable(t, cwd, "shorttool", `sleep 0.2`)
	id, err := proc.Run(cwd, "shorttool", nil, nil)
	if err != nil {
		t.Fatalf("proc.Run failed: %v", err)
	}

	start := time.Now()
	resID, err := proc.Wait(cwd, 10, id)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("proc.Wait failed: %v", err)
	}
	if resID != id {
		t.Fatalf("Wait returned %q, want %q", resID, id)
	}
	if elapsed > 1500*time.Millisecond {
		t.Errorf("short-task latency regressed: Wait took %v for a 200ms task (polling ramp broken?)", elapsed)
	}
}
