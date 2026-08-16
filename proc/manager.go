package proc

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"syscall"
	"time"
)

// Run spawns a background process detached from the current session.
// Strictly resolves toolName against <cwd>/tools/<toolName> (no $PATH fallback).
// Synchronously drains stdinReader to .proc/<id>/stdin before detaching the supervisor.
func Run(cwd string, toolName string, args []string, stdinReader io.Reader) (string, error) {
	if toolName == "" {
		return "", fmt.Errorf("tool name is required")
	}

	// 1. Strict tool resolution against <cwd>/tools/<toolName> - NO $PATH fallback
	toolPath := filepath.Join(cwd, ToolsDirName, toolName)
	fileInfo, err := os.Stat(toolPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("tool %q not found in %s/ (no PATH fallback)", toolName, ToolsDirName)
		}
		return "", fmt.Errorf("failed to access tool %s: %w", toolPath, err)
	}
	if fileInfo.IsDir() {
		return "", fmt.Errorf("tool path %s is a directory, not an executable", toolPath)
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

// Wait blocks up to timeoutSeconds for any tracked background process to complete.
// Returns the proc_id of whichever completes first, or empty string if timeout expires.
func Wait(cwd string, timeoutSeconds int) (string, error) {
	if timeoutSeconds <= 0 {
		timeoutSeconds = 1
	}

	deadline := time.Now().Add(time.Duration(timeoutSeconds) * time.Second)
	ticker := time.NewTicker(time.Duration(DefaultWaitPollIntervalMs) * time.Millisecond)
	defer ticker.Stop()

	for {
		list, err := List(cwd)
		if err != nil {
			return "", err
		}

		if len(list) == 0 {
			return "", fmt.Errorf("no tracked processes found")
		}

		for _, p := range list {
			if p.Status == StatusCompleted || p.Status == StatusFailed || p.Status == StatusCrashed {
				return p.ID, nil
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
