package main

import (
	"bytes"
	"encoding/json"
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
// Flag.Changed, so any test that passes --all leaves the flag marked as set for
// every later Execute() in the same test binary, with allWait retaining its
// stale value. Without this call, a first-completed test added after a --all
// test would silently take the barrier branch and fail on an error unrelated to
// what it is testing.
// Calling it clears incoming state and registers the same reset to run again on
// exit, so every CLI test both starts and ends clean no matter where a future
// test is inserted or how -shuffle orders them.
func resetWaitFlags(t *testing.T) {
	t.Helper()
	clearWaitFlagState()
	t.Cleanup(clearWaitFlagState)
}

func clearWaitFlagState() {
	if f := waitCmd.Flags().Lookup("all"); f != nil {
		f.Changed = false
	}
	allWait = false
	rootCmd.SetOut(os.Stdout)
	rootCmd.SetErr(os.Stderr)
}

func TestWaitCmd_CLI_PositionalAsTimeout(t *testing.T) {
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

	// "wait 1 <id>": the all-digit first argument must parse as the timeout
	// (seconds), not as a process ID. The record reaches terminal state fast, so
	// the wait returns it well inside the 1s window.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe failed: %v", err)
	}
	origStdout := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = origStdout }()

	rootCmd.SetArgs([]string{"wait", "1", id})
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

func TestWaitCmd_CLI_UnknownID_FailsImmediately(t *testing.T) {
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
	rootCmd.SetArgs([]string{"wait", "5", "zz99"})

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

	rootCmd.SetArgs([]string{"wait", "5", id})
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

func TestWaitCmd_CLI_NegativeTimeout(t *testing.T) {
	resetWaitFlags(t)

	tests := []struct {
		name        string
		args        []string
		expectError string
	}{
		{
			name:        "bare negative timeout",
			args:        []string{"wait", "-5"},
			expectError: "wait: timeout must be a non-negative integer (got -5)",
		},
		{
			name:        "negative timeout before an id",
			args:        []string{"wait", "-5", "anyid"},
			expectError: "wait: timeout must be a non-negative integer (got -5)",
		},
		{
			name:        "positional negative timeout via separator",
			args:        []string{"wait", "--", "-5"},
			expectError: "wait: timeout must be a non-negative integer (got -5)",
		},
		{
			name:        "non-integer flag returns original error",
			args:        []string{"wait", "-x"},
			expectError: "unknown shorthand flag: 'x' in -x",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resetWaitFlags(t)
			var stdoutBuf, stderrBuf bytes.Buffer
			rootCmd.SetOut(&stdoutBuf)
			rootCmd.SetErr(&stderrBuf)
			rootCmd.SetArgs(tc.args)

			err := rootCmd.Execute()
			if err == nil {
				t.Fatalf("expected error for args %v, got nil", tc.args)
			}
			if !strings.Contains(err.Error(), tc.expectError) {
				t.Errorf("expected error containing %q, got %q", tc.expectError, err.Error())
			}
		})
	}
}

func TestWaitCmd_CLI_ZeroTimeout_Succeeds(t *testing.T) {
	resetWaitFlags(t)

	tmpDir := t.TempDir()
	toolsDir := filepath.Join(tmpDir, proc.ToolsDirName)
	if err := os.MkdirAll(toolsDir, 0755); err != nil {
		t.Fatalf("failed to create tools dir: %v", err)
	}
	scriptPath := filepath.Join(toolsDir, "instant-exit")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatalf("failed to write instant-exit: %v", err)
	}

	origCwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get cwd: %v", err)
	}
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatalf("failed to chdir to tmpDir: %v", err)
	}
	defer os.Chdir(origCwd)

	id, err := proc.Run(tmpDir, "instant-exit", nil, nil)
	if err != nil {
		t.Fatalf("proc.Run failed: %v", err)
	}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe failed: %v", err)
	}
	origStdout := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = origStdout }()

	rootCmd.SetArgs([]string{"wait", "0", id})
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
	if _, err := proc.Wait(tmpDir, 5, id); err != nil {
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
	_, _ = proc.Wait(tmpDir, 5, id)

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

func TestDescribeCmd_CLI(t *testing.T) {
	tmpDir := t.TempDir()
	origCwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	defer os.Chdir(origCwd)

	toolsDir := filepath.Join(tmpDir, "tools")
	if err := os.MkdirAll(toolsDir, 0755); err != nil {
		t.Fatalf("mkdir tools: %v", err)
	}
	toolScript := "#!/bin/sh\necho done\n"
	if err := os.WriteFile(filepath.Join(toolsDir, "cli-tool"), []byte(toolScript), 0755); err != nil {
		t.Fatalf("write tool: %v", err)
	}

	args := []string{"--prompt", strings.Repeat("z", 5000), "tail"}
	id, err := proc.Run(tmpDir, "cli-tool", args, nil)
	if err != nil {
		t.Fatalf("proc.Run: %v", err)
	}
	if _, err := proc.Wait(tmpDir, 5, id); err != nil {
		t.Fatalf("proc.Wait: %v", err)
	}

	// 1. Text describe shows the full args. fmt.Printf goes to os.Stdout, so capture via pipe.
	assertDescribeOutput := func(args ...string) string {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatalf("os.Pipe: %v", err)
		}
		origStdout := os.Stdout
		os.Stdout = w
		rootCmd.SetArgs(args)
		execErr := rootCmd.Execute()
		w.Close()
		os.Stdout = origStdout
		if execErr != nil {
			t.Fatalf("command %v: %v", args, execErr)
		}
		var buf bytes.Buffer
		io.Copy(&buf, r)
		return buf.String()
	}

	out := assertDescribeOutput("describe", id)
	if !strings.Contains(out, "Args:") {
		t.Errorf("describe text missing Args section: %q", out)
	}
	if !strings.Contains(out, "--prompt") || !strings.Contains(out, "tail") {
		t.Errorf("describe text missing full args: %q", out)
	}

	// 2. describe --json emits valid JSON with args but no consumption.
	var infos []map[string]any
	jsonOut := assertDescribeOutput("describe", id, "--json")
	if err := json.Unmarshal([]byte(jsonOut), &infos); err != nil {
		t.Fatalf("describe --json not valid JSON: %v (out: %s)", err, jsonOut)
	}
	if len(infos) != 1 {
		t.Fatalf("expected 1 describe record, got %d", len(infos))
	}
	if _, hasArgs := infos[0]["args"]; !hasArgs {
		t.Errorf("describe json missing args key: %v", infos[0])
	}

	// 3. list --json stays compact (no args) even for this big-arg record.
	listOut := assertDescribeOutput("list", "--json")
	if strings.Contains(listOut, "zzzzzzzzz") {
		t.Fatal("list --json contains the huge arg - compact contract violated")
	}
}

func TestDescribeCmd_CLI_MissingArgFails(t *testing.T) {
	rootCmd.SetArgs([]string{"describe"})
	err := rootCmd.Execute()
	if err == nil {
		t.Fatal("expected error for describe with no id")
	}
}

// TestDescribeCmd_CLI_MultiID pins the multi-id form (card item 4 of
// tasks/wackyproc/list-describe-split): space-separated IDs produce one human block
// per record and a JSON array of records in --json mode.
func TestDescribeCmd_CLI_MultiID(t *testing.T) {
	// describeJSON is a package-level flag var; earlier tests leave it true when they
	// run describe --json, which would make the human-path capture below emit JSON.
	describeJSON = false
	defer func() { describeJSON = false }()
	tmpDir := t.TempDir()
	origCwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	defer os.Chdir(origCwd)

	toolsDir := filepath.Join(tmpDir, "tools")
	if err := os.MkdirAll(toolsDir, 0755); err != nil {
		t.Fatalf("mkdir tools: %v", err)
	}
	for _, name := range []string{"mt-one", "mt-two"} {
		tool := "#!/bin/sh\necho done\n"
		if err := os.WriteFile(filepath.Join(toolsDir, name), []byte(tool), 0755); err != nil {
			t.Fatalf("write tool: %v", err)
		}
	}

	id1, err := proc.Run(tmpDir, "mt-one", nil, nil)
	if err != nil {
		t.Fatalf("proc.Run mt-one: %v", err)
	}
	id2, err := proc.Run(tmpDir, "mt-two", nil, nil)
	if err != nil {
		t.Fatalf("proc.Run mt-two: %v", err)
	}
	if _, err := proc.WaitAll(tmpDir, 5, id1, id2); err != nil {
		t.Fatalf("proc.Wait: %v", err)
	}

	capture := func(args ...string) string {
		t.Helper()
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatalf("os.Pipe: %v", err)
		}
		origStdout := os.Stdout
		os.Stdout = w
		rootCmd.SetArgs(args)
		execErr := rootCmd.Execute()
		w.Close()
		os.Stdout = origStdout
		if execErr != nil {
			t.Fatalf("command %v: %v", args, execErr)
		}
		var buf bytes.Buffer
		io.Copy(&buf, r)
		return buf.String()
	}

	// Human path: one block per ID, both IDs present.
	out := capture("describe", id1, id2)
	if !strings.Contains(out, "ID:        "+id1) || !strings.Contains(out, "ID:        "+id2) {
		t.Fatalf("multi-id describe missing one of %s/%s in output:\n%s", id1, id2, out)
	}
	if !strings.Contains(out, "Tool:      mt-one") || !strings.Contains(out, "Tool:      mt-two") {
		t.Fatalf("multi-id describe missing tool names:\n%s", out)
	}

	// JSON path: array of exactly 2 records.
	jsonOut := capture("describe", id1, id2, "--json")
	var infos []map[string]any
	if err := json.Unmarshal([]byte(jsonOut), &infos); err != nil {
		t.Fatalf("describe --json not valid JSON array: %v (out: %s)", err, jsonOut)
	}
	if len(infos) != 2 {
		t.Fatalf("expected 2 describe records, got %d", len(infos))
	}
}
