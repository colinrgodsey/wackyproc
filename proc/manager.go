package proc

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// ResolveToolPath searches <cwd>/tools/ recursively for an executable matching toolName (matching D14 tool discovery).
// Resolves directory and file symlinks and follows them, preventing infinite symlink cycles.
// Rejects tool names attempting directory traversal outside tools/.
// Returns the resolved path to the executable or an error if not found in tools/.
func ResolveToolPath(cwd string, toolName string) (string, error) {
	if toolName == "" {
		return "", fmt.Errorf("tool name is required")
	}

	toolsDir := filepath.Join(cwd, ToolsDirName)
	if _, err := os.Stat(toolsDir); err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("tool %q not found in %s/ (no PATH fallback)", toolName, ToolsDirName)
		}
		return "", fmt.Errorf("failed to access %s directory: %w", ToolsDirName, err)
	}

	// 1. Direct relative-path lookup in tools/ (e.g. tools/sub/mytool if
	// toolName is "sub/mytool") - only for names that actually specify a
	// subpath. A bare name (no separator) always falls through to the
	// recursive walk below instead, so a same-named tool nested elsewhere
	// in the tree can shadow it, matching wackypub's own D14 discovery
	// (DiscoverAgentToolsMap) instead of silently preferring whatever
	// happens to sit directly under tools/.
	// Guard against path traversal outside tools/
	if strings.ContainsRune(toolName, '/') {
		cleanDirect := filepath.Clean(filepath.Join(toolsDir, toolName))
		cleanToolsDir := filepath.Clean(toolsDir)
		if strings.HasPrefix(cleanDirect, cleanToolsDir+string(filepath.Separator)) || cleanDirect == cleanToolsDir {
			if info, err := os.Stat(cleanDirect); err == nil && info.Mode().IsRegular() && info.Mode()&0111 != 0 {
				return cleanDirect, nil
			}
		}
	}

	// 2. Recursive discovery under <cwd>/tools/ following directory symlinks with cycle detection (D14)
	toolMap := make(map[string]string)
	visitedDirs := make(map[string]bool)

	var walk func(dir string) error
	walk = func(dir string) error {
		realDir, err := filepath.EvalSymlinks(dir)
		if err != nil {
			return nil // Skip unresolvable directory symlinks
		}
		if visitedDirs[realDir] {
			return nil // Prevent cycle
		}
		visitedDirs[realDir] = true

		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}

		for _, entry := range entries {
			path := filepath.Join(dir, entry.Name())
			info, err := os.Stat(path) // os.Stat follows symlinks
			if err != nil {
				continue // Skip broken symlinks or unreadable files
			}

			if info.IsDir() {
				if err := walk(path); err != nil {
					return err
				}
			} else if info.Mode().IsRegular() && info.Mode()&0111 != 0 {
				name := entry.Name()
				// Last-write-wins on collision, matching wackypub's own
				// DiscoverAgentToolsMap (D14) - a later-discovered tool
				// (e.g. a nested one) overrides an earlier same-named one,
				// so wackyproc resolves any given tool name identically to
				// run_command (D58).
				toolMap[name] = path
			}
		}
		return nil
	}

	if err := walk(toolsDir); err != nil {
		return "", fmt.Errorf("failed walking %s directory: %w", ToolsDirName, err)
	}

	if resolved, ok := toolMap[toolName]; ok {
		return resolved, nil
	}

	return "", fmt.Errorf("tool %q not found in %s/ (no PATH fallback)", toolName, ToolsDirName)
}

// Run spawns a background process detached from the current session.
// Resolves toolName recursively against <cwd>/tools/ following directory symlinks and nested folders
// with cycle detection (no $PATH fallback).
// Synchronously drains stdinReader to .proc/<id>/stdin before detaching the supervisor.
func Run(cwd string, toolName string, args []string, stdinReader io.Reader) (string, error) {
	if toolName == "" {
		return "", fmt.Errorf("tool name is required")
	}

	// 1. Strict tool resolution against <cwd>/tools/ recursively - NO $PATH fallback
	toolPath, err := ResolveToolPath(cwd, toolName)
	if err != nil {
		return "", err
	}

	procBaseDir := filepath.Join(cwd, ProcDirName)
	if err := os.MkdirAll(procBaseDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create %s directory: %w", ProcDirName, err)
	}

	// 2. Allocate unique process ID and atomically create its directory via os.Mkdir
	procID, procDir, err := ClaimUniqueProcessDir(procBaseDir)
	if err != nil {
		return "", err
	}

	// 3. Synchronously drain stdin into .proc/<id>/stdin if present
	if stdinReader != nil {
		stdinData, err := io.ReadAll(stdinReader)
		if err == nil && len(stdinData) > 0 {
			stdinPath := filepath.Join(procDir, StdinFileName)
			if err := os.WriteFile(stdinPath, stdinData, 0644); err != nil {
				_ = os.RemoveAll(procDir)
				return "", fmt.Errorf("failed to write %s: %w", StdinFileName, err)
			}
		}
	}

	// 4. Write initial meta.json
	meta := Meta{
		ID:        procID,
		Tool:      toolName,
		ToolPath:  toolPath,
		Args:      args,
		Cwd:       cwd,
		StartedAt: time.Now().Unix(),
	}
	metaBytes, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		_ = os.RemoveAll(procDir)
		return "", fmt.Errorf("failed to serialize %s: %w", MetaFileName, err)
	}
	if err := os.WriteFile(filepath.Join(procDir, MetaFileName), metaBytes, 0644); err != nil {
		_ = os.RemoveAll(procDir)
		return "", fmt.Errorf("failed to write %s: %w", MetaFileName, err)
	}

	// 5. Resolve self binary path for supervisor fork
	selfBin, err := os.Executable()
	if err != nil {
		_ = os.RemoveAll(procDir)
		return "", fmt.Errorf("failed to resolve executable path: %w", err)
	}

	// 6. Launch detached supervisor with Setsid: true
	cmd := exec.Command(selfBin, "__supervise", procDir)
	cmd.Dir = cwd
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true,
	}
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil

	if err := cmd.Start(); err != nil {
		_ = os.RemoveAll(procDir)
		return "", fmt.Errorf("failed to detach supervisor: %w", err)
	}

	return procID, nil
}

// List inspects all process directories in <cwd>/.proc/ and returns their status.
func List(cwd string) ([]ProcessInfo, error) {
	procBaseDir := filepath.Join(cwd, ProcDirName)
	entries, err := os.ReadDir(procBaseDir)
	if err != nil {
		if os.IsNotExist(err) {
			return []ProcessInfo{}, nil
		}
		return nil, fmt.Errorf("failed to read %s directory: %w", ProcDirName, err)
	}

	var results []ProcessInfo
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		procID := entry.Name()
		procDir := filepath.Join(procBaseDir, procID)

		var meta Meta
		if metaData, err := os.ReadFile(filepath.Join(procDir, MetaFileName)); err == nil {
			_ = json.Unmarshal(metaData, &meta)
		}

		status, pid, pgid, exitCode, err := CheckLiveness(procDir, &meta)
		if err != nil {
			continue
		}

		toolName := meta.Tool
		if toolName == "" {
			toolName = procID
		}

		results = append(results, ProcessInfo{
			ID:        procID,
			Tool:      toolName,
			Args:      meta.Args,
			Status:    status,
			PID:       pid,
			PGID:      pgid,
			ExitCode:  exitCode,
			StartedAt: meta.StartedAt,
		})
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].StartedAt < results[j].StartedAt
	})

	if results == nil {
		results = []ProcessInfo{}
	}
	return results, nil
}

// Get dumps captured stdout and stderr for procID to the provided writers.
func Get(cwd string, procID string, stdoutWriter io.Writer, stderrWriter io.Writer) error {
	procDir := filepath.Join(cwd, ProcDirName, procID)
	if _, err := os.Stat(procDir); os.IsNotExist(err) {
		return fmt.Errorf("process %q not found", procID)
	}

	stdoutPath := filepath.Join(procDir, StdoutFileName)
	if stdoutData, err := os.ReadFile(stdoutPath); err == nil && len(stdoutData) > 0 {
		if _, err := stdoutWriter.Write(stdoutData); err != nil {
			return fmt.Errorf("failed to write stdout: %w", err)
		}
	}

	stderrPath := filepath.Join(procDir, StderrFileName)
	if stderrData, err := os.ReadFile(stderrPath); err == nil && len(stderrData) > 0 {
		if _, err := stderrWriter.Write(stderrData); err != nil {
			return fmt.Errorf("failed to write stderr: %w", err)
		}
	}

	return nil
}

// trailingLines returns the trailing n lines from data.
// If data has fewer than n lines, the entire slice is returned.
// A final line without a trailing newline is preserved as-is.
func trailingLines(data []byte, n int) []byte {
	if len(data) == 0 || n <= 0 {
		return nil
	}

	end := len(data)
	if data[len(data)-1] == '\n' {
		end = len(data) - 1
	}

	count := 0
	for i := end - 1; i >= 0; i-- {
		if data[i] == '\n' {
			count++
			if count == n {
				return data[i+1:]
			}
		}
	}

	return data
}

// Peek writes the trailing lines of captured stdout and stderr for procID to the provided writers.
// A file with fewer than lines is output in full. A final line without a trailing newline is preserved.
// Empty streams write nothing. It performs no state modification and does not mark the process record as consumed.
func Peek(cwd string, procID string, lines int, stdoutWriter io.Writer, stderrWriter io.Writer) error {
	// Package-level validation provides defense-in-depth for direct callers.
	if lines < 1 {
		return fmt.Errorf("--lines must be >= 1")
	}

	procDir := filepath.Join(cwd, ProcDirName, procID)
	if _, err := os.Stat(procDir); os.IsNotExist(err) {
		return fmt.Errorf("process %q not found", procID)
	}

	stdoutPath := filepath.Join(procDir, StdoutFileName)
	if stdoutData, err := os.ReadFile(stdoutPath); err == nil && len(stdoutData) > 0 {
		if tail := trailingLines(stdoutData, lines); len(tail) > 0 {
			if _, err := stdoutWriter.Write(tail); err != nil {
				return fmt.Errorf("failed to write stdout: %w", err)
			}
		}
	}

	stderrPath := filepath.Join(procDir, StderrFileName)
	if stderrData, err := os.ReadFile(stderrPath); err == nil && len(stderrData) > 0 {
		if tail := trailingLines(stderrData, lines); len(tail) > 0 {
			if _, err := stderrWriter.Write(tail); err != nil {
				return fmt.Errorf("failed to write stderr: %w", err)
			}
		}
	}

	return nil
}

// clampWaitSeconds bounds a requested wait duration to [1, MaxWaitSeconds].
// Letting a caller block indefinitely (or for an unreasonably long single
// call) defeats the point of a process manager meant to avoid tying up a
// turn on long-running work; clamping preserves Wait's existing "nothing
// finished in time" contract (empty string, not an error) rather than
// adding a new failure mode for a too-large request.
func clampWaitSeconds(requested int) int {
	if requested <= 0 {
		return 1
	}
	if requested > MaxWaitSeconds {
		return MaxWaitSeconds
	}
	return requested
}

func isTerminal(status string) bool {
	return status == StatusCompleted || status == StatusFailed || status == StatusCrashed
}

// Wait blocks up to timeoutSeconds for a background process to reach a terminal state.
// If targetID is provided, Wait blocks until that specific process reaches a terminal state;
// baseline exclusion does not apply to a targeted wait.
// If targetID is omitted, Wait blocks until any process that was still running when the call began
// reaches a terminal state, ignoring processes already terminal when the call started.
// Returns the process ID of the completed process, or an empty string if the timeout expires.
func Wait(cwd string, timeoutSeconds int, targetID ...string) (string, error) {
	var target string
	hasTarget := len(targetID) > 0
	if hasTarget {
		target = targetID[0]
	}

	timeoutSeconds = clampWaitSeconds(timeoutSeconds)

	if hasTarget {
		// An empty target has to be rejected here rather than falling through:
		// filepath.Join drops the empty component, so the Stat below would resolve
		// cwd/.proc, which exists once any process has run, and a bare --for would
		// block until timeout instead of failing fast.
		if target == "" {
			return "", fmt.Errorf("process %q not found", target)
		}
		procDir := filepath.Join(cwd, ProcDirName, target)
		if _, err := os.Stat(procDir); os.IsNotExist(err) {
			return "", fmt.Errorf("process %q not found", target)
		}
	}

	var baselineTerminal map[string]bool
	if !hasTarget {
		baselineTerminal = make(map[string]bool)
		initList, err := List(cwd)
		if err != nil {
			return "", err
		}
		for _, p := range initList {
			if isTerminal(p.Status) {
				baselineTerminal[p.ID] = true
			}
		}
	}

	deadline := time.Now().Add(time.Duration(timeoutSeconds) * time.Second)
	ticker := time.NewTicker(time.Duration(DefaultWaitPollIntervalMs) * time.Millisecond)
	defer ticker.Stop()

	for {
		list, err := List(cwd)
		if err != nil {
			return "", err
		}

		if hasTarget {
			var found *ProcessInfo
			for i := range list {
				if list[i].ID == target {
					found = &list[i]
					break
				}
			}
			if found == nil {
				procDir := filepath.Join(cwd, ProcDirName, target)
				if _, err := os.Stat(procDir); os.IsNotExist(err) {
					return "", fmt.Errorf("process %q not found", target)
				}
			} else if isTerminal(found.Status) {
				return found.ID, nil
			}
		} else {
			for _, p := range list {
				if isTerminal(p.Status) {
					if baselineTerminal[p.ID] {
						continue
					}
					return p.ID, nil
				}
			}
		}

		if time.Now().After(deadline) {
			return "", nil
		}

		<-ticker.C
	}
}

// Stop terminates the process group associated with procID using SIGTERM, followed by SIGKILL if needed.
func Stop(cwd string, procID string, timeoutSeconds int) error {
	procDir := filepath.Join(cwd, ProcDirName, procID)
	if _, err := os.Stat(procDir); os.IsNotExist(err) {
		return fmt.Errorf("process %q not found", procID)
	}

	var meta Meta
	if metaData, err := os.ReadFile(filepath.Join(procDir, MetaFileName)); err == nil {
		_ = json.Unmarshal(metaData, &meta)
	}

	status, pid, pgid, _, err := CheckLiveness(procDir, &meta)
	if err != nil {
		return err
	}

	if status != StatusRunning {
		return nil // Already terminated
	}

	target := -pgid
	if pgid <= 0 {
		target = -pid
		if pid <= 0 {
			return fmt.Errorf("invalid PID/PGID for process %q", procID)
		}
	}

	// Send SIGTERM to process group
	_ = syscall.Kill(target, syscall.SIGTERM)

	if timeoutSeconds <= 0 {
		timeoutSeconds = 3
	}

	deadline := time.Now().Add(time.Duration(timeoutSeconds) * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		st, _, _, _, _ := CheckLiveness(procDir, &meta)
		if st != StatusRunning {
			return nil
		}
	}

	// Force kill with SIGKILL if still running
	_ = syscall.Kill(target, syscall.SIGKILL)
	time.Sleep(50 * time.Millisecond)
	_, _, _, _, _ = CheckLiveness(procDir, &meta)
	return nil
}
