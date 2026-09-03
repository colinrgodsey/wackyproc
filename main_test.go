package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
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

// resetWaitFlags clears cobra's package-level flag state between CLI tests.
//
// rootCmd and waitCmd are process-global singletons and pflag never resets
// Flag.Changed, so any test that passes --for leaves the flag marked as set for
// every later Execute() in the same test binary, with waitFor retaining its
// stale value. Without this call, a bare-mode test added after a --for test
// would silently take the targeted branch and fail on a "process not found"
// error unrelated to what it is testing.
// Calling it clears incoming state and registers the same reset to run again on
// exit, so every CLI test both starts and ends clean no matter where a future
// test is inserted or how -shuffle orders them. Doing this only at the start of
// the two --for tests would leave the hazard open for any test that copies the
// bare-mode test below as its template and never learns the convention exists.
func resetWaitFlags(t *testing.T) {
	t.Helper()
	clearWaitFlagState()
	t.Cleanup(clearWaitFlagState)
}

func clearWaitFlagState() {
	if f := waitCmd.Flags().Lookup("for"); f != nil {
		f.Changed = false
	}
	waitFor = ""
	rootCmd.SetOut(os.Stdout)
	rootCmd.SetErr(os.Stderr)
}

func TestWaitCmd_CLI_PositionalAsTimeout(t *testing.T) {
	resetWaitFlags(t)

	tmpDir := t.TempDir()
	origCwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get cwd: %v", err)
	}
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatalf("failed to chdir to tmpDir: %v", err)
	}
	defer os.Chdir(origCwd)

	// wackyproc wait 1 (1 second) with no tracked processes
	// Should time out and fail with "timeout waiting for process"
	var stdoutBuf, stderrBuf bytes.Buffer
	rootCmd.SetOut(&stdoutBuf)
	rootCmd.SetErr(&stderrBuf)
	rootCmd.SetArgs([]string{"wait", "1"})

	err = rootCmd.Execute()
	if err == nil {
		t.Fatal("expected error on timeout, got nil")
	}
	if !strings.Contains(err.Error(), "timeout waiting for process") {
		t.Errorf("expected 'timeout waiting for process' error, got %v", err)
	}

	// The SilenceUsage contract is a claim about what is printed, so assert on the
	// capture rather than on the struct field that produces it.
	if strings.Contains(stderrBuf.String(), "Usage:") {
		t.Errorf("expected no usage block on timeout, got stderr:\n%s", stderrBuf.String())
	}
	if !strings.Contains(stderrBuf.String(), "timeout waiting for process") {
		t.Errorf("expected the diagnostic on stderr, got:\n%s", stderrBuf.String())
	}
}

func TestWaitCmd_CLI_ForUnknownID_FailsImmediately(t *testing.T) {
	resetWaitFlags(t)

	tmpDir := t.TempDir()
	origCwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get cwd: %v", err)
	}
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatalf("failed to chdir to tmpDir: %v", err)
	}
	defer os.Chdir(origCwd)

	var stdoutBuf, stderrBuf bytes.Buffer
	rootCmd.SetOut(&stdoutBuf)
	rootCmd.SetErr(&stderrBuf)
	rootCmd.SetArgs([]string{"wait", "--for", "zz99", "5"})

	start := time.Now()
	err = rootCmd.Execute()
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error for unknown target process ID, got nil")
	}
	expectedErr := `process "zz99" not found`
	if !strings.Contains(err.Error(), expectedErr) {
		t.Errorf("expected %q error, got %v", expectedErr, err)
	}
	if elapsed > 200*time.Millisecond {
		t.Errorf("expected immediate failure, took %v", elapsed)
	}
}

func TestWaitCmd_CLI_Success(t *testing.T) {
	resetWaitFlags(t)

	tmpDir := t.TempDir()
	toolsDir := filepath.Join(tmpDir, proc.ToolsDirName)
	if err := os.MkdirAll(toolsDir, 0755); err != nil {
		t.Fatalf("failed to create tools dir: %v", err)
	}
	scriptPath := filepath.Join(toolsDir, "quick-cli")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatalf("failed to write quick-cli: %v", err)
	}

	origCwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get cwd: %v", err)
	}
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatalf("failed to chdir to tmpDir: %v", err)
	}
	defer os.Chdir(origCwd)

	id, err := proc.Run(tmpDir, "quick-cli", nil, nil)
	if err != nil {
		t.Fatalf("proc.Run failed: %v", err)
	}

	// Test --for <id> with CLI
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe failed: %v", err)
	}
	origStdout := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = origStdout }()

	rootCmd.SetArgs([]string{"wait", "--for", id, "5"})
	execErr := rootCmd.Execute()

	w.Close()

	var outBuf bytes.Buffer
	_, _ = io.Copy(&outBuf, r)

	if execErr != nil {
		t.Fatalf("rootCmd.Execute failed: %v", execErr)
	}
	if !strings.Contains(outBuf.String(), id) {
		t.Errorf("expected stdout to contain %q, got %q", id, outBuf.String())
	}
}

func clearPeekFlagState() {
	if f := peekCmd.Flags().Lookup("lines"); f != nil {
		f.Changed = false
		_ = f.Value.Set("20")
	}
	peekLines = 20
	rootCmd.SetOut(os.Stdout)
	rootCmd.SetErr(os.Stderr)
}

func resetPeekFlags(t *testing.T) {
	t.Helper()
	clearPeekFlagState()
	t.Cleanup(clearPeekFlagState)
}

func TestPeekCmd_CLI_Validation(t *testing.T) {
	resetPeekFlags(t)

	// Test --lines 0
	rootCmd.SetArgs([]string{"peek", "xxxx", "--lines", "0"})
	err := rootCmd.Execute()
	if err == nil {
		t.Fatal("expected error for --lines 0, got nil")
	}
	if !strings.Contains(err.Error(), "--lines must be >= 1") {
		t.Errorf("expected '--lines must be >= 1', got: %v", err)
	}

	// Test --lines negative
	resetPeekFlags(t)
	rootCmd.SetArgs([]string{"peek", "xxxx", "--lines", "-5"})
	err = rootCmd.Execute()
	if err == nil {
		t.Fatal("expected error for --lines -5, got nil")
	}
	if !strings.Contains(err.Error(), "--lines must be >= 1") {
		t.Errorf("expected '--lines must be >= 1', got: %v", err)
	}

	// Test missing argument
	resetPeekFlags(t)
	rootCmd.SetArgs([]string{"peek"})
	err = rootCmd.Execute()
	if err == nil {
		t.Fatal("expected error for missing proc_id argument, got nil")
	}

	// Test too many arguments
	resetPeekFlags(t)
	rootCmd.SetArgs([]string{"peek", "a", "b"})
	err = rootCmd.Execute()
	if err == nil {
		t.Fatal("expected error for too many arguments, got nil")
	}
}

func TestPeekCmd_CLI_Execution(t *testing.T) {
	resetPeekFlags(t)

	tmpDir := t.TempDir()
	origCwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get cwd: %v", err)
	}
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatalf("failed to chdir to tmpDir: %v", err)
	}
	defer os.Chdir(origCwd)

	toolsDir := filepath.Join(tmpDir, "tools")
	if err := os.MkdirAll(toolsDir, 0755); err != nil {
		t.Fatalf("mkdir tools: %v", err)
	}
	toolScript := "#!/bin/sh\nfor i in $(seq 1 25); do echo \"line $i\"; done\n"
	if err := os.WriteFile(filepath.Join(toolsDir, "gen-lines"), []byte(toolScript), 0755); err != nil {
		t.Fatalf("write tool: %v", err)
	}

	id, err := proc.Run(tmpDir, "gen-lines", nil, nil)
	if err != nil {
		t.Fatalf("proc.Run failed: %v", err)
	}

	// Wait for process to complete
	if _, err := proc.Wait(tmpDir, 5); err != nil {
		t.Fatalf("proc.Wait failed: %v", err)
	}

	// 1. Default --lines (20)
	{
		resetPeekFlags(t)
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatalf("os.Pipe: %v", err)
		}
		origStdout := os.Stdout
		os.Stdout = w

		rootCmd.SetArgs([]string{"peek", id})
		execErr := rootCmd.Execute()
		w.Close()
		os.Stdout = origStdout

		var outBuf bytes.Buffer
		_, _ = io.Copy(&outBuf, r)

		if execErr != nil {
			t.Fatalf("peek default failed: %v", execErr)
		}

		outStr := outBuf.String()
		lines := strings.Split(strings.TrimSuffix(outStr, "\n"), "\n")
		if len(lines) != 20 {
			t.Errorf("expected 20 lines by default, got %d:\n%s", len(lines), outStr)
		}
		if lines[0] != "line 6" || lines[19] != "line 25" {
			t.Errorf("expected lines 6 through 25, got start=%q end=%q", lines[0], lines[19])
		}
	}

	// 2. Custom --lines 5
	{
		resetPeekFlags(t)
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatalf("os.Pipe: %v", err)
		}
		origStdout := os.Stdout
		os.Stdout = w

		rootCmd.SetArgs([]string{"peek", id, "--lines", "5"})
		execErr := rootCmd.Execute()
		w.Close()
		os.Stdout = origStdout

		var outBuf bytes.Buffer
		_, _ = io.Copy(&outBuf, r)

		if execErr != nil {
			t.Fatalf("peek --lines 5 failed: %v", execErr)
		}

		outStr := outBuf.String()
		lines := strings.Split(strings.TrimSuffix(outStr, "\n"), "\n")
		if len(lines) != 5 {
			t.Errorf("expected 5 lines, got %d:\n%s", len(lines), outStr)
		}
		if lines[0] != "line 21" || lines[4] != "line 25" {
			t.Errorf("expected lines 21 through 25, got start=%q end=%q", lines[0], lines[4])
		}
	}
}

func TestPruneCmd_CLI(t *testing.T) {
	tmpDir := t.TempDir()
	toolsDir := filepath.Join(tmpDir, proc.ToolsDirName)
	if err := os.MkdirAll(toolsDir, 0755); err != nil {
		t.Fatalf("failed to create tools dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(toolsDir, "tool-p"), []byte("#!/bin/sh\necho done\n"), 0755); err != nil {
		t.Fatalf("failed to write tool: %v", err)
	}

	origCwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get cwd: %v", err)
	}
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatalf("failed to chdir to tmpDir: %v", err)
	}
	defer os.Chdir(origCwd)

	id, err := proc.Run(tmpDir, "tool-p", nil, nil)
	if err != nil {
		t.Fatalf("proc.Run failed: %v", err)
	}
	_, _ = proc.Wait(tmpDir, 5)

	// 1. Happy path: prune with no args
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	origStdout := os.Stdout
	os.Stdout = w

	rootCmd.SetArgs([]string{"prune"})
	execErr := rootCmd.Execute()
	w.Close()
	os.Stdout = origStdout

	var outBuf bytes.Buffer
	_, _ = io.Copy(&outBuf, r)

	if execErr != nil {
		t.Fatalf("prune command failed: %v", execErr)
	}
	if !strings.Contains(outBuf.String(), id) {
		t.Errorf("expected prune output to mention %s, got: %q", id, outBuf.String())
	}
	if _, err := os.Stat(filepath.Join(tmpDir, proc.ProcDirName, id)); !os.IsNotExist(err) {
		t.Errorf("expected %s to be removed, but directory still exists", id)
	}

	// 2. Extra args rejected (cobra.NoArgs)
	rootCmd.SetArgs([]string{"prune", "unexpected-arg"})
	err = rootCmd.Execute()
	if err == nil {
		t.Errorf("expected error passing args to prune, got nil")
	}
}

func TestUnconsumeCmd_CLI(t *testing.T) {
	tmpDir := t.TempDir()
	toolsDir := filepath.Join(tmpDir, proc.ToolsDirName)
	if err := os.MkdirAll(toolsDir, 0755); err != nil {
		t.Fatalf("failed to create tools dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(toolsDir, "tool-u"), []byte("#!/bin/sh\necho done\n"), 0755); err != nil {
		t.Fatalf("failed to write tool: %v", err)
	}

	origCwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get cwd: %v", err)
	}
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatalf("failed to chdir to tmpDir: %v", err)
	}
	defer os.Chdir(origCwd)

	id, err := proc.Run(tmpDir, "tool-u", nil, nil)
	if err != nil {
		t.Fatalf("proc.Run failed: %v", err)
	}
	_, _ = proc.Wait(tmpDir, 5)

	var stdoutBuf, stderrBuf bytes.Buffer
	if err := proc.Get(tmpDir, id, &stdoutBuf, &stderrBuf); err != nil {
		t.Fatalf("proc.Get failed: %v", err)
	}

	metaPath := filepath.Join(tmpDir, proc.ProcDirName, id, proc.MetaFileName)
	metaBytes, _ := os.ReadFile(metaPath)
	if !strings.Contains(string(metaBytes), "consumed_seq") {
		t.Fatalf("expected consumed_seq after get, got: %s", string(metaBytes))
	}

	// 1. Happy path: unconsume <id>
	rootCmd.SetArgs([]string{"unconsume", id})
	execErr := rootCmd.Execute()
	if execErr != nil {
		t.Fatalf("unconsume failed: %v", execErr)
	}

	metaBytes, _ = os.ReadFile(metaPath)
	if strings.Contains(string(metaBytes), "consumed_seq") {
		t.Errorf("expected consumed_seq to be removed after unconsume, got: %s", string(metaBytes))
	}

	// 2. Unknown ID
	rootCmd.SetArgs([]string{"unconsume", "zz99"})
	err = rootCmd.Execute()
	if err == nil {
		t.Fatalf("expected error for unknown ID, got nil")
	}
	if !strings.Contains(err.Error(), `process "zz99" not found`) {
		t.Errorf("expected 'process \"zz99\" not found', got: %v", err)
	}

	// 3. Missing arg (ExactArgs(1))
	rootCmd.SetArgs([]string{"unconsume"})
	err = rootCmd.Execute()
	if err == nil {
		t.Errorf("expected error with 0 args, got nil")
	}
}
