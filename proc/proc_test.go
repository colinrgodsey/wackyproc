package proc_test

import (
	"bytes"
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
	completedID, err := proc.Wait(cwd, 5)
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

	completedID, err := proc.Wait(cwd, 5)
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

	status, _, _, exitCode, err := proc.CheckLiveness(procDir, &meta)
	if err != nil {
		t.Fatalf("CheckLiveness failed: %v", err)
	}

	// Should be marked CRASHED because the recorded start time doesn't match the current process start time
	if status != proc.StatusCrashed || exitCode == nil || *exitCode != proc.CrashedExitCode {
		t.Errorf("expected CRASHED with 137 due to start time mismatch, got status=%s, exit=%v", status, exitCode)
	}
}

func TestWait_Timeout(t *testing.T) {
	cwd := setupTestEnv(t)

	createExecutable(t, cwd, "sleeper", `
sleep 10
`)
	_, err := proc.Run(cwd, "sleeper", nil, nil)
	if err != nil {
		t.Fatalf("proc.Run failed: %v", err)
	}

	// Wait 1 second (should timeout and return "")
	resID, err := proc.Wait(cwd, 1)
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
	if !strings.Contains(string(content), "name: wackyproc") {
		t.Errorf("expected skill to contain 'name: wackyproc'")
	}
	if !strings.Contains(string(content), "# WackyProc Process Management & Long-Running Command Guide") {
		t.Errorf("expected skill to contain guide title")
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

	completedID, err := proc.Wait(cwd, 5)
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
