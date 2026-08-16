package proc

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// GetProcessStartTime retrieves a process start-time signature for PID reuse validation.
// Uses /proc/<pid>/stat if available (Linux), falling back to 'ps -p <pid> -o lstart=' (POSIX).
func GetProcessStartTime(pid int) string {
	if pid <= 0 {
		return ""
	}

	// 1. Check /proc/<pid>/stat on Linux
	statPath := fmt.Sprintf("/proc/%d/stat", pid)
	if data, err := os.ReadFile(statPath); err == nil {
		content := string(data)
		lastParen := strings.LastIndex(content, ")")
		if lastParen != -1 && lastParen+1 < len(content) {
			fields := strings.Fields(content[lastParen+1:])
			// In /proc/<pid>/stat, starttime is field 22 (1-indexed).
			// After the last ')', field index 0 is state (field 3), so starttime is index 19.
			if len(fields) > 19 {
				return fields[19]
			}
		}
	}

	// 2. Fallback to ps -p <pid> -o lstart=
	cmd := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "lstart=")
	if out, err := cmd.Output(); err == nil {
		return strings.TrimSpace(string(out))
	}

	return ""
}

// CheckLiveness determines the current status of a process in procDir.
// Returns status (RUNNING, COMPLETED, FAILED, CRASHED), PID, PGID, exitCode.
func CheckLiveness(procDir string, meta *Meta) (status string, pid int, pgid int, exitCode *int, err error) {
	// Read PID and PGID if files exist
	if pidBytes, err := os.ReadFile(filepath.Join(procDir, PIDFileName)); err == nil {
		if val, err := strconv.Atoi(strings.TrimSpace(string(pidBytes))); err == nil {
			pid = val
		}
	}
	if pgidBytes, err := os.ReadFile(filepath.Join(procDir, PGIDFileName)); err == nil {
		if val, err := strconv.Atoi(strings.TrimSpace(string(pgidBytes))); err == nil {
			pgid = val
		}
	}

	// 1. Check if exit_code file exists
	exitCodePath := filepath.Join(procDir, ExitCodeFileName)
	if exitData, err := os.ReadFile(exitCodePath); err == nil {
		code, err := strconv.Atoi(strings.TrimSpace(string(exitData)))
		if err == nil {
			exitCode = &code
			if code == 0 {
				return StatusCompleted, pid, pgid, exitCode, nil
			}
			return StatusFailed, pid, pgid, exitCode, nil
		}
	}

	// If no PID recorded yet (e.g. process just spawning)
	if pid <= 0 {
		return StatusRunning, pid, pgid, nil, nil
	}

	// 2. Zero-signal liveness check (kill -0 <pid>)
	process, err := os.FindProcess(pid)
	if err != nil || process.Signal(syscall.Signal(0)) != nil {
		// If child process just terminated, check if supervisor is still alive and finalizing exit_code
		if supData, supErr := os.ReadFile(filepath.Join(procDir, SupervisorPIDFileName)); supErr == nil {
			if supPID, supErr := strconv.Atoi(strings.TrimSpace(string(supData))); supErr == nil && supPID > 0 {
				if supProc, supErr := os.FindProcess(supPID); supErr == nil && supProc.Signal(syscall.Signal(0)) == nil {
					// Supervisor is alive and finalizing exit code
					return StatusRunning, pid, pgid, nil, nil
				}
			}
		}

		// Re-check if exit_code was written in the interim
		if exitData, err := os.ReadFile(exitCodePath); err == nil {
			code, err := strconv.Atoi(strings.TrimSpace(string(exitData)))
			if err == nil {
				exitCode = &code
				if code == 0 {
					return StatusCompleted, pid, pgid, exitCode, nil
				}
				return StatusFailed, pid, pgid, exitCode, nil
			}
		}

		// Process and supervisor are both dead without writing exit_code
		crashedCode := CrashedExitCode
		_ = os.WriteFile(exitCodePath, []byte(strconv.Itoa(crashedCode)+"\n"), 0644)
		return StatusCrashed, pid, pgid, &crashedCode, nil
	}

	// 3. Start-time verification to detect PID reuse
	if meta != nil && meta.StartTime != "" {
		currentStartTime := GetProcessStartTime(pid)
		if currentStartTime != "" && currentStartTime != meta.StartTime {
			// PID was recycled by another process!
			crashedCode := CrashedExitCode
			_ = os.WriteFile(exitCodePath, []byte(strconv.Itoa(crashedCode)+"\n"), 0644)
			return StatusCrashed, pid, pgid, &crashedCode, nil
		}
	}

	return StatusRunning, pid, pgid, nil, nil
}
