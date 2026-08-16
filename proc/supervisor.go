package proc

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
)

// Supervise runs the target tool as a child in its own process group (Setpgid: true),
// piping its stdin/stdout/stderr to files in procDir, and recording PID, PGID, and exit code.
func Supervise(procDir string) error {
	metaPath := filepath.Join(procDir, MetaFileName)
	metaBytes, err := os.ReadFile(metaPath)
	if err != nil {
		return fmt.Errorf("failed to read %s: %w", MetaFileName, err)
	}

	var meta Meta
	if err := json.Unmarshal(metaBytes, &meta); err != nil {
		return fmt.Errorf("failed to parse %s: %w", MetaFileName, err)
	}

	// Record supervisor PID
	_ = os.WriteFile(filepath.Join(procDir, SupervisorPIDFileName), []byte(strconv.Itoa(os.Getpid())+"\n"), 0644)

	stdoutPath := filepath.Join(procDir, StdoutFileName)
	stdoutFile, err := os.OpenFile(stdoutPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("failed to create %s: %w", StdoutFileName, err)
	}
	defer stdoutFile.Close()

	stderrPath := filepath.Join(procDir, StderrFileName)
	stderrFile, err := os.OpenFile(stderrPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("failed to create %s: %w", StderrFileName, err)
	}
	defer stderrFile.Close()

	var stdinFile *os.File
	stdinPath := filepath.Join(procDir, StdinFileName)
	if _, err := os.Stat(stdinPath); err == nil {
		if f, err := os.Open(stdinPath); err == nil {
			stdinFile = f
			defer stdinFile.Close()
		}
	}

	cmd := exec.Command(meta.ToolPath, meta.Args...)
	cmd.Dir = meta.Cwd
	cmd.Env = os.Environ()
	cmd.Stdout = stdoutFile
	cmd.Stderr = stderrFile
	if stdinFile != nil {
		cmd.Stdin = stdinFile
	}

	// Isolate target command into its own process group
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}

	if err := cmd.Start(); err != nil {
		_ = os.WriteFile(filepath.Join(procDir, ExitCodeFileName), []byte("127\n"), 0644)
		_, _ = fmt.Fprintf(stderrFile, "failed to start %s: %v\n", meta.ToolPath, err)
		return fmt.Errorf("failed to start %s: %w", meta.ToolPath, err)
	}

	pid := cmd.Process.Pid
	pgid, err := syscall.Getpgid(pid)
	if err != nil {
		pgid = pid
	}

	// Record PID and PGID
	_ = os.WriteFile(filepath.Join(procDir, PIDFileName), []byte(strconv.Itoa(pid)+"\n"), 0644)
	_ = os.WriteFile(filepath.Join(procDir, PGIDFileName), []byte(strconv.Itoa(pgid)+"\n"), 0644)

	// Record process start time signature to detect PID recycling
	startTime := GetProcessStartTime(pid)
	if startTime != "" {
		meta.StartTime = startTime
		if updatedBytes, err := json.MarshalIndent(meta, "", "  "); err == nil {
			_ = os.WriteFile(metaPath, updatedBytes, 0644)
		}
	}

	// Wait for target command to terminate
	waitErr := cmd.Wait()
	exitCode := 0
	if waitErr != nil {
		if exitErr, ok := waitErr.(*exec.ExitError); ok {
			if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok {
				if ws.Signaled() {
					exitCode = 128 + int(ws.Signal())
				} else {
					exitCode = ws.ExitStatus()
				}
			} else {
				exitCode = exitErr.ExitCode()
			}
		} else {
			exitCode = 1
		}
	}

	// Record final exit code
	_ = os.WriteFile(filepath.Join(procDir, ExitCodeFileName), []byte(strconv.Itoa(exitCode)+"\n"), 0644)
	return nil
}
