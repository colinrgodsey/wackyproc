package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// a2aChainVars are the environment variables wackypub rebuilds a call chain from:
// Agent2AgentEnvVar (JSON metadata) and CallChainEnvVar (legacy CSV, read by
// ParseA2AMetadata when the JSON one is absent - pkg/agent/a2a.go). A wake hook that
// strips only one leaves the chain live, the self-dispatch is rejected as a cycle, and the
// completion notification is lost. If wackypub ever adds a third carrier, add it here: the
// test below names whatever is missing rather than passing quietly.
var a2aChainVars = []string{"AGENT2AGENT", "WACKYPUB_CALL_CHAIN"}

func isIdentByte(b byte) bool {
	return b == '_' || b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z' || b >= '0' && b <= '9'
}

// strippedEnvVars returns the names an env -u invocation removes from a script.
func strippedEnvVars(script string) map[string]bool {
	stripped := map[string]bool{}
	for _, line := range strings.Split(script, string([]byte{10})) {
		fields := strings.Fields(line)
		for i, f := range fields {
			if f == "-u" && i+1 < len(fields) {
				stripped[fields[i+1]] = true
			}
		}
	}
	return stripped
}

// walkExampleScripts visits every shipped hook script.
func walkExampleScripts(t *testing.T, visit func(path string, body string)) {
	t.Helper()
	err := filepath.Walk("examples", func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.Contains(path, "/hooks/") {
			return nil
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatalf("read %s: %v", path, readErr)
		}
		visit(path, string(body))
		return nil
	})
	if err != nil {
		t.Fatalf("walk examples: %v", err)
	}
}

// TestWakeHooksStripEveryChainCarrier is the F1 regression guard: a hook that wakes its own
// agent must drop every chain carrier, because an inherited chain makes the wake a cycle.
func TestWakeHooksStripEveryChainCarrier(t *testing.T) {
	checked := 0
	walkExampleScripts(t, func(path string, body string) {
		if !strings.Contains(body, "agent prompt") {
			return
		}
		checked++
		stripped := strippedEnvVars(body)
		for _, want := range a2aChainVars {
			if !stripped[want] {
				t.Errorf("%s dispatches a wake but does not strip %s; an inherited chain makes wackypub reject the self-dispatch as a cycle, losing the notification. Chain carriers: %s",
					path, want, strings.Join(a2aChainVars, ", "))
			}
		}
	})
	if checked == 0 {
		t.Fatal("no wake hook found under examples/, so this guard proves nothing")
	}
}

// TestExamplesNameOnlyRealChainVars catches the bug class that shipped: a chain variable
// spelled plausibly but existing nowhere (WACKYPROC_CALL_CHAIN), which strips nothing and
// leaves the real one inherited.
func TestExamplesNameOnlyRealChainVars(t *testing.T) {
	real := map[string]bool{"WACKYPUB_CALL_CHAIN": true}
	seen := map[string]string{}
	walkExampleScripts(t, func(path string, body string) {
		for _, token := range strings.Fields(body) {
			var b []byte
			for i := 0; i < len(token); i++ {
				if isIdentByte(token[i]) {
					b = append(b, token[i])
					continue
				}
				if len(b) > 0 {
					seen[string(b)] = path
					b = nil
				}
			}
			if len(b) > 0 {
				seen[string(b)] = path
			}
		}
	})
	for name, path := range seen {
		if !strings.Contains(name, "CALL_CHAIN") {
			continue
		}
		if !real[name] {
			t.Errorf("%s names %s, which is not a chain variable wackypub defines; env -u on a name that does not exist strips nothing. Real carriers: %s",
				path, name, strings.Join(a2aChainVars, ", "))
		}
	}
}
