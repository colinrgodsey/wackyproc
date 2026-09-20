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

// Liveness is the outcome of inspecting one process record. It replaced a five-value
// return of two adjacent ints plus a nullable code, which call sites could only consume
// by discarding positions they did not want.
type Liveness struct {
	Status string
	PID    int
	PGID   int
	// ExitCode is non-nil only when a terminal status is backed by a recorded exit code.
	ExitCode *int
}

// readRecordInt reads a single-integer process record file (pid, pgid).
// An absent or malformed file yields 0, which callers treat as "not recorded yet".
func readRecordInt(procDir, name string) int {
	data, err := os.ReadFile(filepath.Join(procDir, name))
	if err != nil {
		return 0
	}
	val, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0
	}
	return val
}

// readRecordExitCode returns the recorded exit code when exit_code holds a parseable integer.
func readRecordExitCode(procDir string) *int {
	data, err := os.ReadFile(filepath.Join(procDir, ExitCodeFileName))
	if err != nil {
		return nil
	}
	code, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return nil
	}
	return &code
}

// statusFromTerminal derives COMPLETED or FAILED from a recorded exit code, and reports
// whether a usable code was present at all.
func statusFromTerminal(procDir string) (string, *int, bool) {
	code := readRecordExitCode(procDir)
	if code == nil {
		return "", nil, false
	}
	if *code == 0 {
		return StatusCompleted, code, true
	}
	return StatusFailed, code, true
}

// markCrashed writes the crashed marker. The returned status does not depend on this
// write: the marker only spares later readers the re-derivation, so a failure here is
// reported by the caller's own status and never changes the answer.
func markCrashed(procDir string) {
	_ = os.WriteFile(filepath.Join(procDir, CrashedFileName), []byte(""), 0644)
}

// CheckLiveness determines the current status of a process in procDir.
func CheckLiveness(procDir string, meta *Meta) (Liveness, error) {
	lv := Liveness{
		PID:  readRecordInt(procDir, PIDFileName),
		PGID: readRecordInt(procDir, PGIDFileName),
	}

	crashedPath := filepath.Join(procDir, CrashedFileName)

	// 1. Check if crashed marker file exists
	if _, err := os.Stat(crashedPath); err == nil {
		lv.Status = StatusCrashed
		lv.ExitCode = readRecordExitCode(procDir)
		return lv, nil
	}

	// 2. Check if exit_code file exists
	if status, code, ok := statusFromTerminal(procDir); ok {
		lv.Status = status
		lv.ExitCode = code
		return lv, nil
	}

	// If no PID recorded yet (e.g. process just spawning)
	if lv.PID <= 0 {
		lv.Status = StatusRunning
		return lv, nil
	}

	// 3. Zero-signal liveness check (kill -0 <pid>)
	process, err := os.FindProcess(lv.PID)
	if err != nil || process.Signal(syscall.Signal(0)) != nil {
		// If child process just terminated, check if supervisor is still alive and finalizing exit_code
		if supPID := readRecordInt(procDir, SupervisorPIDFileName); supPID > 0 {
			if supProc, supErr := os.FindProcess(supPID); supErr == nil && supProc.Signal(syscall.Signal(0)) == nil {
				// Supervisor is alive and finalizing exit code
				lv.Status = StatusRunning
				return lv, nil
			}
		}

		// Re-check if crashed marker or exit_code was written in the interim
		if _, err := os.Stat(crashedPath); err == nil {
			lv.Status = StatusCrashed
			lv.ExitCode = readRecordExitCode(procDir)
			return lv, nil
		}
		if status, code, ok := statusFromTerminal(procDir); ok {
			lv.Status = status
			lv.ExitCode = code
			return lv, nil
		}

		// Process and supervisor are both dead without writing exit_code
		markCrashed(procDir)
		lv.Status = StatusCrashed
		return lv, nil
	}

	// 4. Start-time verification to detect PID reuse
	if meta != nil && meta.StartTime != "" {
		currentStartTime := GetProcessStartTime(lv.PID)
		if currentStartTime != "" && currentStartTime != meta.StartTime {
			// PID was recycled by another process!
			markCrashed(procDir)
			lv.Status = StatusCrashed
			return lv, nil
		}
	}

	lv.Status = StatusRunning
	return lv, nil
}
