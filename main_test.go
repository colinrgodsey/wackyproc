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
