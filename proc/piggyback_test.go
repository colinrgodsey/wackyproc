package proc_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/colinrgodsey/wackyproc/proc"
)

// eotPayload builds the payload exactly as wackypub publishes it, so a rename on either
// side of the wire contract shows up as a failing test rather than a silently idle hook.
func eotPayload(canContinue bool, stopReason string, continuationCount, maxContinuations int) []byte {
	return []byte(fmt.Sprintf(`{"agent_id":"piggy-test","session_path":"/tmp/session.jsonl","stop_reason":%q,"duration_ms":42,"continuation_count":%d,"max_continuations":%d,"can_continue":%t}`,
		stopReason, continuationCount, maxContinuations, canContinue))
}

func piggybackAwaitTerminal(t *testing.T, cwd, id string) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		list, err := proc.List(cwd)
		if err != nil {
			t.Fatalf("list failed: %v", err)
		}
		for _, p := range list {
			if p.ID == id && p.Status != proc.StatusRunning {
				return
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("job %s never reached a terminal state", id)
}

func finishedJob(t *testing.T, toolName string) (string, string) {
	t.Helper()
	cwd := setupTestEnv(t)
	createExecutable(t, cwd, toolName, "echo finished")
	id, err := proc.Run(cwd, toolName, nil, nil)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	piggybackAwaitTerminal(t, cwd, id)
	return cwd, id
}

// TestPiggybackHandsOverWhenTurnCanContinue is the D2 case D1 deliberately skips: the agent
// was busy for the whole life of the job, the idle wake stepped aside, and the end of the
// turn is where the completion finally reaches it.
func TestPiggybackHandsOverWhenTurnCanContinue(t *testing.T) {
	cwd, id := finishedJob(t, "pig-tool")

	var stderr bytes.Buffer
	res := proc.EndOfTurnPiggyback(cwd, eotPayload(true, "completed", 0, 4), &stderr)
	if stderr.Len() != 0 {
		t.Errorf("unexpected warnings: %s", stderr.String())
	}
	if len(res.Surfaced) != 1 || res.Surfaced[0] != id {
		t.Fatalf("expected the finished job to be handed over, got %v", res.Surfaced)
	}
	if res.Output.Action == nil || res.Output.Action.Type != "continue" {
		t.Fatalf("expected a continuation request, got %+v", res.Output.Action)
	}
	for _, want := range []string{id, "pig-tool", proc.StatusCompleted, "wackyproc get " + id} {
		if !strings.Contains(res.Output.Action.Output, want) {
			t.Errorf("hand-off text missing %q; got:\n%s", want, res.Output.Action.Output)
		}
	}
	if res.Output.Action.Reason == "" {
		t.Error("the request must carry a reason so the continuation is attributable")
	}

	info, err := proc.Describe(cwd, id)
	if err != nil {
		t.Fatalf("describe failed: %v", err)
	}
	if !info.Notified {
		t.Error("handing the job over must mark it notified, or the next turn reports it twice")
	}
}

// TestPiggybackLeavesJobsOnNotifyOnlyPath guards the contract that matters most: wackypub
// discards a continuation on its notify-only paths, so consuming the notification there
// would announce the job to nobody and lose it.
func TestPiggybackLeavesJobsOnNotifyOnlyPath(t *testing.T) {
	for _, stopReason := range []string{"failure_breaker", "max_tool_turns", "compacted_bail", "deferred_image", "completed"} {
		t.Run(stopReason, func(t *testing.T) {
			cwd, id := finishedJob(t, "pig-busy")

			var stderr bytes.Buffer
			res := proc.EndOfTurnPiggyback(cwd, eotPayload(false, stopReason, 2, 2), &stderr)
			if res.Output.Action != nil {
				t.Errorf("notify-only path must request nothing, got %+v", res.Output.Action)
			}
			if len(res.Surfaced) != 0 {
				t.Errorf("notify-only path must mark nothing notified, marked %v", res.Surfaced)
			}
			if !strings.Contains(res.Skipped, "notify-only") {
				t.Errorf("expected an auditable notify-only reason, got %q", res.Skipped)
			}

			info, err := proc.Describe(cwd, id)
			if err != nil {
				t.Fatalf("describe failed: %v", err)
			}
			if info.Notified {
				t.Fatal("the job was marked notified for a continuation that would have been dropped")
			}
			pending, err := proc.ListPending(cwd)
			if err != nil {
				t.Fatalf("pending failed: %v", err)
			}
			if len(pending) != 1 || pending[0].ID != id {
				t.Errorf("the job must stay pending for a later turn, got %v", pending)
			}
		})
	}
}

func TestPiggybackQuietWhenNothingPending(t *testing.T) {
	t.Run("no records at all", func(t *testing.T) {
		cwd := setupTestEnv(t)
		res := proc.EndOfTurnPiggyback(cwd, eotPayload(true, "completed", 0, 4), os.Stderr)
		if res.Output.Action != nil || len(res.Surfaced) != 0 {
			t.Errorf("a fresh agent folder must produce nothing, got %+v", res)
		}
		if !strings.Contains(res.Skipped, "no un-notified completed jobs") {
			t.Errorf("expected the quiet reason, got %q", res.Skipped)
		}
	})

	t.Run("already surfaced by a manual read", func(t *testing.T) {
		cwd, id := finishedJob(t, "pig-read")
		if err := proc.Get(cwd, id, &noopWriter{}, &noopWriter{}); err != nil {
			t.Fatalf("get failed: %v", err)
		}
		res := proc.EndOfTurnPiggyback(cwd, eotPayload(true, "completed", 0, 4), os.Stderr)
		if res.Output.Action != nil {
			t.Errorf("a job the agent already read must not be reported again: %+v", res.Output.Action)
		}
	})
}

func TestPiggybackIgnoresRunningJobs(t *testing.T) {
	cwd := setupTestEnv(t)
	createExecutable(t, cwd, "pig-sleep", "sleep 20")
	id, err := proc.Run(cwd, "pig-sleep", nil, nil)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	defer func() { _ = proc.Stop(cwd, id, 2) }()

	res := proc.EndOfTurnPiggyback(cwd, eotPayload(true, "completed", 0, 4), os.Stderr)
	if res.Output.Action != nil || len(res.Surfaced) != 0 {
		t.Errorf("a still-running job must not be reported as finished: %+v", res.Output.Action)
	}
}

func TestPiggybackDegradesOnGarbagePayload(t *testing.T) {
	cwd, id := finishedJob(t, "pig-garbage")
	res := proc.EndOfTurnPiggyback(cwd, []byte("this is not json"), os.Stderr)
	if res.Output.Action != nil || len(res.Surfaced) != 0 {
		t.Errorf("a garbage payload must produce no action and no marking, got %+v", res)
	}
	if res.Skipped == "" {
		t.Error("the failure must still be explained")
	}
	info, err := proc.Describe(cwd, id)
	if err != nil {
		t.Fatalf("describe failed: %v", err)
	}
	if info.Notified {
		t.Error("a broken hand-off must leave the job pending")
	}
}

func TestPiggybackOutputIsWackypubHookShape(t *testing.T) {
	cwd, id := finishedJob(t, "pig-shape")
	res := proc.EndOfTurnPiggyback(cwd, eotPayload(true, "completed", 0, 4), os.Stderr)

	var buf bytes.Buffer
	if err := proc.WriteHookOutput(&buf, res.Output); err != nil {
		t.Fatalf("write hook output: %v", err)
	}
	var parsed struct {
		Env    map[string]any `json:"env,omitempty"`
		Text   *string        `json:"text,omitempty"`
		Action *struct {
			Type   string `json:"type"`
			Output string `json:"output"`
			Stderr string `json:"stderr"`
			Reason string `json:"reason"`
		} `json:"action"`
	}
	if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("wackypub could not parse our output %q: %v", buf.String(), err)
	}
	if parsed.Action == nil || parsed.Action.Type != "continue" {
		t.Fatalf("expected action.type continue, got %q", buf.String())
	}
	if !strings.Contains(parsed.Action.Output, id) {
		t.Errorf("parsed action output lost the job id: %q", parsed.Action.Output)
	}

	var empty bytes.Buffer
	if err := proc.WriteHookOutput(&empty, proc.HookOutput{}); err != nil {
		t.Fatalf("write empty hook output: %v", err)
	}
	if strings.TrimSpace(empty.String()) != "{}" {
		t.Errorf("an empty decision must still print a valid object, got %q", empty.String())
	}
}

// TestPiggybackComposesWithIdleWake pins P7 across both notification paths: whichever path
// surfaces the job first, the other must stay silent, because the flag is shared.
func TestPiggybackComposesWithIdleWake(t *testing.T) {
	cwd := setupTestEnv(t)
	createExecutable(t, cwd, "pig-wake", "echo finished")
	dump := filepath.Join(cwd, "woke.txt")
	writeCompletionHook(t, cwd, "10-wake.sh", "echo woke >> \"$DUMP\"")
	t.Setenv("DUMP", dump)

	id, err := proc.Run(cwd, "pig-wake", nil, nil)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if _, ok := waitForFile(t, dump, 8*time.Second); !ok {
		t.Fatalf("the idle wake never fired, so this says nothing about composition")
	}

	res := proc.EndOfTurnPiggyback(cwd, eotPayload(true, "completed", 0, 4), os.Stderr)
	if res.Output.Action != nil {
		t.Errorf("the piggyback must not re-report a job the idle wake already delivered: %+v", res.Output.Action)
	}
	if !strings.Contains(res.Skipped, "no un-notified completed jobs") {
		t.Errorf("expected the quiet reason after the wake fired, got %q", res.Skipped)
	}
	_ = id
}
