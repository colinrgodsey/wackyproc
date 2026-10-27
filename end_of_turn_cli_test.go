package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/colinrgodsey/wackyproc/proc"
)

// cliFinishedJob runs a tool to completion in a throwaway agent folder and returns its id.
// It deliberately does not call wait or get, so the record reaches the CLI still un-notified.
func cliFinishedJob(t *testing.T, toolName string) (string, string) {
	t.Helper()
	tmpDir := t.TempDir()
	toolsDir := filepath.Join(tmpDir, proc.ToolsDirName)
	if err := os.MkdirAll(toolsDir, 0755); err != nil {
		t.Fatalf("failed to create tools dir: %v", err)
	}
	scriptPath := filepath.Join(toolsDir, toolName)
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\necho finished\n"), 0755); err != nil {
		t.Fatalf("failed to write tool: %v", err)
	}

	id, err := proc.Run(tmpDir, toolName, nil, nil)
	if err != nil {
		t.Fatalf("proc.Run failed: %v", err)
	}
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		list, err := proc.List(tmpDir)
		if err != nil {
			t.Fatalf("proc.List failed: %v", err)
		}
		for _, p := range list {
			if p.ID == id && p.Status != proc.StatusRunning {
				return tmpDir, id
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("job %s never reached a terminal state", id)
	return "", ""
}

func runEndOfTurnCLI(t *testing.T, cwd string, payload string) string {
	t.Helper()
	origCwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get cwd: %v", err)
	}
	if err := os.Chdir(cwd); err != nil {
		t.Fatalf("failed to chdir: %v", err)
	}
	defer os.Chdir(origCwd)

	var out, errBuf bytes.Buffer
	rootCmd.SetIn(strings.NewReader(payload))
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&errBuf)
	rootCmd.SetArgs([]string{"end-of-turn"})
	defer func() {
		rootCmd.SetOut(os.Stdout)
		rootCmd.SetErr(os.Stderr)
	}()

	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("rootCmd.Execute failed: %v (stderr: %s)", err, errBuf.String())
	}
	if !strings.HasPrefix(strings.TrimSpace(out.String()), "{") {
		t.Fatalf("hook stdout must be pure JSON for wackypub to parse, got %q", out.String())
	}
	return out.String()
}

func payloadCanContinue(canContinue bool, stopReason string) string {
	return fmt.Sprintf(`{"agent_id":"cli","session_path":"/tmp/s.jsonl","stop_reason":%q,"duration_ms":1,"continuation_count":0,"max_continuations":4,"can_continue":%t}`, stopReason, canContinue)
}

func TestEndOfTurnCmd_CLI_HandsOverAndMarks(t *testing.T) {
	cwd, id := cliFinishedJob(t, "cli-pig")
	out := runEndOfTurnCLI(t, cwd, payloadCanContinue(true, "completed"))

	if !strings.Contains(out, `"type":"continue"`) {
		t.Errorf("expected a continuation request, got %q", out)
	}
	if !strings.Contains(out, id) || !strings.Contains(out, "wackyproc get "+id) {
		t.Errorf("the request must name the job and how to read it, got %q", out)
	}
	info, err := proc.Describe(cwd, id)
	if err != nil {
		t.Fatalf("describe failed: %v", err)
	}
	if !info.Notified {
		t.Error("the CLI hand-off must mark the job notified")
	}
}

func TestEndOfTurnCmd_CLI_NotifyOnlyLeavesPending(t *testing.T) {
	cwd, id := cliFinishedJob(t, "cli-pig-busy")
	out := runEndOfTurnCLI(t, cwd, payloadCanContinue(false, "failure_breaker"))

	if strings.TrimSpace(out) != "{}" {
		t.Errorf("a notify-only path must ask for nothing, got %q", out)
	}
	info, err := proc.Describe(cwd, id)
	if err != nil {
		t.Fatalf("describe failed: %v", err)
	}
	if info.Notified {
		t.Fatal("a notify-only path consumed the notification for a continuation that would be dropped")
	}
}

func TestEndOfTurnCmd_CLI_NoRecordsPrintsEmptyObject(t *testing.T) {
	cwd := t.TempDir()
	if out := runEndOfTurnCLI(t, cwd, payloadCanContinue(true, "completed")); strings.TrimSpace(out) != "{}" {
		t.Errorf("an agent with no jobs must end the turn untouched, got %q", out)
	}
}
