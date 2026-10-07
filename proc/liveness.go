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

// statusFromMarkers checks if the crashed marker file exists, returning StatusCrashed
// and any recorded exit code.
func statusFromMarkers(procDir string) (string, *int, bool) {
	if _, err := os.Stat(filepath.Join(procDir, CrashedFileName)); err == nil {
		return StatusCrashed, readRecordExitCode(procDir), true
	}
	return "", nil, false
}

// statusFromExitCode derives COMPLETED or FAILED from a recorded exit code, and reports
// whether a usable code was present at all.
func statusFromExitCode(procDir string) (string, *int, bool) {
	code := readRecordExitCode(procDir)
	if code == nil {
		return "", nil, false
	}
	if *code == 0 {
		return StatusCompleted, code, true
	}
	return StatusFailed, code, true
}

// isSupervisorFinalizing checks if supervisor is still alive and finalizing exit_code.
// A zombie supervisor (exited or killed but not yet reaped by its parent) passes the
// zero-signal check yet can never finalize, so it does not count as finalizing.
func isSupervisorFinalizing(procDir string) bool {
	if supPID := readRecordInt(procDir, SupervisorPIDFileName); supPID > 0 {
		if supProc, supErr := os.FindProcess(supPID); supErr == nil && supProc.Signal(syscall.Signal(0)) == nil {
			if !isZombie(supPID) {
				return true
			}
		}
	}
	return false
}

// isZombie reports whether pid is a zombie: exited but not yet reaped. The state field
// is the first field after the closing paren of /proc/<pid>/stat. Non-Linux or an
// unreadable stat yields false, keeping the conservative (assume finalizing) behavior.
func isZombie(pid int) bool {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	content := string(data)
	lastParen := strings.LastIndex(content, ")")
	if lastParen != -1 && lastParen+1 < len(content) {
		fields := strings.Fields(content[lastParen+1:])
		if len(fields) > 0 {
			return fields[0] == "Z"
		}
	}
	return false
}

// pidRecycled checks whether a running process PID has a start time that differs
// from the recorded start time, indicating the PID was recycled by the OS.
func pidRecycled(pid int, meta *Meta) bool {
	if meta != nil && meta.StartTime != "" {
		currentStartTime := GetProcessStartTime(pid)
		if currentStartTime != "" && currentStartTime != meta.StartTime {
			return true
		}
	}
	return false
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

	// 1. Check if crashed marker file exists
	if status, code, ok := statusFromMarkers(procDir); ok {
		lv.Status = status
		lv.ExitCode = code
		return lv, nil
	}

	// 2. Check if exit_code file exists
	if status, code, ok := statusFromExitCode(procDir); ok {
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
		if isSupervisorFinalizing(procDir) {
			lv.Status = StatusRunning
			return lv, nil
		}

		// Re-check if crashed marker or exit_code was written in the interim
		if status, code, ok := statusFromMarkers(procDir); ok {
			lv.Status = status
			lv.ExitCode = code
			return lv, nil
		}
		if status, code, ok := statusFromExitCode(procDir); ok {
			lv.Status = status
			lv.ExitCode = code
			return lv, nil
		}

		// Process and supervisor are both dead without writing exit_code
		markCrashed(procDir)
		lv.Status = StatusCrashed
		return lv, nil
	}

	// 3b. A zombie answers the zero-signal check, so the branch above cannot see
	// that the tool already exited. Only the supervisor writes exit_code, and only
	// after reaping, so a live non-zombie supervisor means the record is still
	// finalizing and the exit code has not been lost. Marking every zombie terminal
	// would be wrong: a tool that exited microseconds ago is a zombie for exactly as
	// long as it takes the supervisor to reap it and write exit_code, and the crashed
	// marker outranks exit_code in CheckLiveness, so the marker would permanently
	// shadow a status that was about to be correct.
	if isZombie(lv.PID) {
		if isSupervisorFinalizing(procDir) {
			lv.Status = StatusRunning
			return lv, nil
		}
		markCrashed(procDir)
		lv.Status = StatusCrashed
		return lv, nil
	}

	// 4. Start-time verification to detect PID reuse
	if pidRecycled(lv.PID, meta) {
		markCrashed(procDir)
		lv.Status = StatusCrashed
		return lv, nil
	}

	lv.Status = StatusRunning
	return lv, nil
}
