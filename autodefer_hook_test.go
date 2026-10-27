package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// Tests for examples/hooks/pre-run-command/10-auto-defer: the hook script that
// consumes wackypub's pre-run-command event and can hand the call to
// wackyproc run instead of letting it execute inline.

const autoDeferScript = "examples/hooks/pre-run-command/10-auto-defer"

const slowToolBody = `#!/bin/sh
sleep 0.2
echo the-result
`

const trivialToolBody = `#!/bin/sh
exit 0
`

type preRunCommandPayload struct {
	AgentID     string            `json:"agent_id"`
	Command     string            `json:"command"`
	CommandPath string            `json:"command_path"`
	Args        []string          `json:"args"`
	Cwd         string            `json:"cwd"`
	Env         map[string]string `json:"env"`
}

type hookAction struct {
	Type   string `json:"type"`
	Output string `json:"output,omitempty"`
	Reason string `json:"reason,omitempty"`
}

type hookOutput struct {
	Action *hookAction `json:"action,omitempty"`
}

var (
	wpBinOnce sync.Once
	wpBinPath string
	wpBinErr  error
)

func wackyprocBin(t *testing.T) string {
	t.Helper()
	wpBinOnce.Do(func() {
		dir, err := os.MkdirTemp("", "autodefer-bin")
		if err != nil {
			wpBinErr = err
			return
		}
		wpBinPath = filepath.Join(dir, "wackyproc")
		cmd := exec.Command("go", "build", "-o", wpBinPath, ".")
		if out, err := cmd.CombinedOutput(); err != nil {
			wpBinErr = fmt.Errorf(`%v
%s`, err, out)
			return
		}
	})
	if wpBinErr != nil {
		t.Fatalf("building wackyproc: %v", wpBinErr)
	}
	return wpBinPath
}

// setupAutoDefer builds a throwaway agent folder: tools/ with the named tool
// (unless toolName is empty) and a built wackyproc binary as tools/wackyproc
// (unless withWackyproc is false). Returns the agent dir.
func setupAutoDefer(t *testing.T, toolName, toolBody string, withWackyproc bool) string {
	t.Helper()
	agentDir := t.TempDir()
	toolsDir := filepath.Join(agentDir, "tools")
	if err := os.MkdirAll(toolsDir, 0o755); err != nil {
		t.Fatalf("mkdir tools: %v", err)
	}
	if toolName != "" {
		toolPath := filepath.Join(toolsDir, toolName)
		if err := os.WriteFile(toolPath, []byte(toolBody), 0o755); err != nil {
			t.Fatalf("write tool: %v", err)
		}
	}
	if withWackyproc {
		if err := os.Symlink(wackyprocBin(t), filepath.Join(toolsDir, "wackyproc")); err != nil {
			t.Fatalf("link wackyproc: %v", err)
		}
	}
	return agentDir
}

func runAutoDeferHook(t *testing.T, payload preRunCommandPayload, env ...string) (string, error) {
	t.Helper()
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	cmd := exec.Command("sh", autoDeferScript)
	cmd.Stdin = strings.NewReader(string(data))
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.Output()
	return string(out), err
}

func payloadFor(agentDir, command string, args []string, env map[string]string) preRunCommandPayload {
	return preRunCommandPayload{
		AgentID:     "autodefertest",
		Command:     command,
		CommandPath: filepath.Join(agentDir, "tools", command),
		Args:        args,
		Cwd:         agentDir,
		Env:         env,
	}
}

var procIDInNotice = regexp.MustCompile(`job ([a-z0-9]{8})`)

// TestAutoDeferMarkerDefersAndNamesJob is the acceptance case: a marked call is
// deferred, the model-visible output names the proc ID and the retrieval path,
// and wait/get on the ID returns the real result.
func TestAutoDeferMarkerDefersAndNamesJob(t *testing.T) {
	agentDir := setupAutoDefer(t, "slow-tool", slowToolBody, true)
	out, err := runAutoDeferHook(t, payloadFor(agentDir, "slow-tool", nil, map[string]string{"WACKYPROC_BG": "1"}),
		"WACKYPROC_AUTODEFER_POLICY=marker")
	if err != nil {
		t.Fatalf(`hook failed: %v
out=%s`, err, out)
	}
	var ho hookOutput
	if err := json.Unmarshal([]byte(out), &ho); err != nil {
		t.Fatalf(`hook output is not JSON: %v
%s`, err, out)
	}
	if ho.Action == nil || ho.Action.Type != "short_circuit" {
		t.Fatalf("expected a short_circuit action, got %+v", ho.Action)
	}
	m := procIDInNotice.FindStringSubmatch(ho.Action.Output)
	if m == nil {
		t.Fatalf(`output does not name a proc ID:
%s`, ho.Action.Output)
	}
	id := m[1]
	for _, want := range []string{"wackyproc get " + id, "wackyproc wait 30 " + id} {
		if !strings.Contains(ho.Action.Output, want) {
			t.Errorf(`output missing %q:
%s`, want, ho.Action.Output)
		}
	}
	var meta struct {
		ID   string   `json:"id"`
		Tool string   `json:"tool"`
		Args []string `json:"args"`
	}
	metaPath := filepath.Join(agentDir, ".proc", id, "meta.json")
	data, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("no record for deferred job: %v", err)
	} else if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatalf("bad meta.json: %v", err)
	} else if meta.Tool != "slow-tool" {
		t.Errorf("deferred the wrong tool: %q", meta.Tool)
	}
	wp := filepath.Join(agentDir, "tools", "wackyproc")
	waitCmd := exec.Command(wp, "wait", "10", id)
	waitCmd.Dir = agentDir
	out2, err := waitCmd.Output()
	if err != nil {
		t.Fatalf("wait failed: %v", err)
	} else if got := strings.TrimSpace(string(out2)); got != id {
		t.Fatalf("wait returned %q, want %q", got, id)
	}
	getCmd := exec.Command(wp, "get", id)
	getCmd.Dir = agentDir
	result, err := getCmd.Output()
	if err != nil {
		t.Fatalf("get failed: %v", err)
	}
	if !strings.Contains(string(result), "the-result") {
		t.Fatalf(`get did not return the tool output:
%s`, string(result))
	}
}

// TestAutoDeferUnmarkedCallRunsInline: without the marker the script is silent,
// so the command executes normally and no job is spawned.
func TestAutoDeferUnmarkedCallRunsInline(t *testing.T) {
	agentDir := setupAutoDefer(t, "slow-tool", slowToolBody, true)
	out, err := runAutoDeferHook(t, payloadFor(agentDir, "slow-tool", nil, map[string]string{}),
		"WACKYPROC_AUTODEFER_POLICY=marker")
	if err != nil {
		t.Fatalf(`hook failed: %v
out=%s`, err, out)
	}
	if out != "" {
		t.Fatalf(`unmarked call must not be deferred, got:
%s`, out)
	}
	if _, err := os.Stat(filepath.Join(agentDir, ".proc")); !os.IsNotExist(err) {
		t.Fatalf("no wackyproc job may be spawned for an unmarked call")
	}
}

// TestAutoDeferPolicyOff: the kill switch wins even over a marker.
func TestAutoDeferPolicyOff(t *testing.T) {
	agentDir := setupAutoDefer(t, "slow-tool", slowToolBody, true)
	out, err := runAutoDeferHook(t, payloadFor(agentDir, "slow-tool", nil, map[string]string{"WACKYPROC_BG": "1"}),
		"WACKYPROC_AUTODEFER_POLICY=off")
	if err != nil {
		t.Fatalf(`hook failed: %v
out=%s`, err, out)
	}
	if out != "" {
		t.Fatalf(`policy off must never defer, got:
%s`, out)
	}
}

// TestAutoDeferAlwaysDefersButNotWackyproc: always defers ordinary tools but
// never the background machinery itself.
func TestAutoDeferAlwaysDefersButNotWackyproc(t *testing.T) {
	agentDir := setupAutoDefer(t, "slow-tool", slowToolBody, true)
	out, err := runAutoDeferHook(t, payloadFor(agentDir, "slow-tool", nil, nil),
		"WACKYPROC_AUTODEFER_POLICY=always")
	if err != nil {
		t.Fatalf(`hook failed: %v
out=%s`, err, out)
	}
	if !procIDInNotice.MatchString(out) {
		t.Fatalf(`always policy must defer an ordinary tool, got:
%s`, out)
	}
	out, err = runAutoDeferHook(t, payloadFor(agentDir, "wackyproc", []string{"run", "slow-tool"}, nil),
		"WACKYPROC_AUTODEFER_POLICY=always")
	if err != nil {
		t.Fatalf(`hook failed: %v
out=%s`, err, out)
	}
	if out != "" {
		t.Fatalf(`the wackyproc tool itself must never be deferred, got:
%s`, out)
	}
}

// TestAutoDeferHeuristic: the documented heuristic defers known long-running
// shapes and leaves everything else inline.
func TestAutoDeferHeuristic(t *testing.T) {
	agentDir := setupAutoDefer(t, "bash", trivialToolBody, true)
	out, err := runAutoDeferHook(t, payloadFor(agentDir, "bash", []string{"-c", "go test ./..."}, nil),
		"WACKYPROC_AUTODEFER_POLICY=heuristic")
	if err != nil {
		t.Fatalf(`hook failed: %v
out=%s`, err, out)
	}
	if !procIDInNotice.MatchString(out) {
		t.Errorf(`heuristic must defer bash -c go test, got:
%s`, out)
	}
	out, err = runAutoDeferHook(t, payloadFor(agentDir, "bash", []string{"-c", "echo hi"}, nil),
		"WACKYPROC_AUTODEFER_POLICY=heuristic")
	if err != nil {
		t.Fatalf(`hook failed: %v
out=%s`, err, out)
	}
	if out != "" {
		t.Errorf(`heuristic must leave an ordinary echo inline, got:
%s`, out)
	}
}

// TestAutoDeferDegradesWhenWackyprocIsMissing (P5): no wackyproc anywhere means
// the hook stays silent and the command runs inline.
func TestAutoDeferDegradesWhenWackyprocIsMissing(t *testing.T) {
	agentDir := setupAutoDefer(t, "slow-tool", slowToolBody, false)
	out, err := runAutoDeferHook(t, payloadFor(agentDir, "slow-tool", nil, nil),
		"WACKYPROC_AUTODEFER_POLICY=always", "PATH=/usr/bin:/bin")
	if err != nil {
		t.Fatalf(`hook must exit 0 and degrade, got: %v
out=%s`, err, out)
	}
	if out != "" {
		t.Fatalf(`no wackyproc means no deferral, got:
%s`, out)
	}
}
