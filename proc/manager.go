package proc

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ResolveToolPath searches <cwd>/tools/ recursively for an executable matching toolName (matching D14 tool discovery).
// Resolves directory and file symlinks and follows them, preventing infinite symlink cycles.
// resolveDirectToolPath attempts to resolve toolName as a relative subpath under toolsDir.
func resolveDirectToolPath(toolsDir, toolName string) (string, bool) {
	if !strings.ContainsRune(toolName, '/') {
		return "", false
	}
	cleanDirect := filepath.Clean(filepath.Join(toolsDir, toolName))
	cleanToolsDir := filepath.Clean(toolsDir)
	if strings.HasPrefix(cleanDirect, cleanToolsDir+string(filepath.Separator)) || cleanDirect == cleanToolsDir {
		if info, err := os.Stat(cleanDirect); err == nil && info.Mode().IsRegular() && info.Mode()&0111 != 0 {
			return cleanDirect, true
		}
	}
	return "", false
}

// discoverToolsMap walks toolsDir recursively with symlink cycle detection to build the tool map.
func discoverToolsMap(toolsDir string) (map[string]string, error) {
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
		return nil, fmt.Errorf("failed walking %s directory: %w", ToolsDirName, err)
	}
	return toolMap, nil
}

// ResolveToolPath resolves toolName to an executable path under <cwd>/tools/.
// Traversal and recursive discovery match wackypub's D14 tool discovery rules.
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
	// toolName is "sub/mytool") - only for names that actually specify a subpath.
	if directPath, ok := resolveDirectToolPath(toolsDir, toolName); ok {
		return directPath, nil
	}

	// 2. Recursive discovery under <cwd>/tools/ following directory symlinks with cycle detection (D14)
	toolMap, err := discoverToolsMap(toolsDir)
	if err != nil {
		return "", err
	}

	if resolved, ok := toolMap[toolName]; ok {
		return resolved, nil
	}

	return "", fmt.Errorf("tool %q not found in %s/ (no PATH fallback)", toolName, ToolsDirName)
}

// cleanupProcDir removes a process directory that failed to initialize. If removal
// fails, it warns on stderr so leftover garbage in .proc/ is operator-visible,
// but does not overwrite the primary initialization error that caused the abort.
func cleanupProcDir(procDir string) {
	if err := os.RemoveAll(procDir); err != nil {
		// Best-effort cleanup: the primary initialization error is returned to the caller;
		// log leftover directory to stderr so disk accumulation is diagnosable.
		fmt.Fprintf(os.Stderr, "warning: failed to clean up aborted process directory %s: %v\n", procDir, err)
	}
}

// prepareProcessRecord prepares the process directory, writes stdin if provided, allocates
// a sequence generation number, and persists the initial meta.json.
func prepareProcessRecord(cwd, procID, procDir, toolName, toolPath string, args []string, stdinReader io.Reader) error {
	procBaseDir := filepath.Join(cwd, ProcDirName)

	// Synchronously drain stdin into .proc/<id>/stdin if present
	if stdinReader != nil {
		stdinData, err := io.ReadAll(stdinReader)
		if err == nil && len(stdinData) > 0 {
			stdinPath := filepath.Join(procDir, StdinFileName)
			if err := os.WriteFile(stdinPath, stdinData, 0644); err != nil {
				cleanupProcDir(procDir)
				return fmt.Errorf("failed to write %s: %w", StdinFileName, err)
			}
		}
	}

	// Allocate monotonic generation number and write initial meta.json
	gen, err := nextSeq(procBaseDir)
	if err != nil {
		cleanupProcDir(procDir)
		return fmt.Errorf("failed to allocate sequence generation: %w", err)
	}

	meta := Meta{
		ID:        procID,
		Tool:      toolName,
		ToolPath:  toolPath,
		Args:      args,
		Cwd:       cwd,
		StartedAt: time.Now().Unix(),
		Gen:       gen,
	}
	metaBytes, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		cleanupProcDir(procDir)
		return fmt.Errorf("failed to serialize %s: %w", MetaFileName, err)
	}
	if err := os.WriteFile(filepath.Join(procDir, MetaFileName), metaBytes, 0644); err != nil {
		cleanupProcDir(procDir)
		return fmt.Errorf("failed to write %s: %w", MetaFileName, err)
	}
	return nil
}

// launchSupervisor starts the detached supervisor process for procDir with Setsid: true.
func launchSupervisor(cwd, procDir string) error {
	selfBin, err := os.Executable()
	if err != nil {
		cleanupProcDir(procDir)
		return fmt.Errorf("failed to resolve executable path: %w", err)
	}

	cmd := exec.Command(selfBin, "__supervise", procDir)
	cmd.Dir = cwd
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true,
	}
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil

	if err := cmd.Start(); err != nil {
		cleanupProcDir(procDir)
		return fmt.Errorf("failed to detach supervisor: %w", err)
	}
	return nil
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

	// 3. Prepare process directory, drain stdin, and write initial meta.json
	if err := prepareProcessRecord(cwd, procID, procDir, toolName, toolPath, args, stdinReader); err != nil {
		return "", err
	}

	// 4. Launch detached supervisor with Setsid: true
	if err := launchSupervisor(cwd, procDir); err != nil {
		return "", err
	}

	// Post-spawn disposal of consumed terminal records exceeding cap (D79)
	disposeConsumedTerminals(procBaseDir)

	return procID, nil
}

func isTerminal(status string) bool {
	return status == StatusCompleted || status == StatusFailed || status == StatusCrashed
}

type consumedTerminalRecord struct {
	id   string
	gen  uint64
	path string
}

// disposeConsumedTerminals disposes consumed terminal records in ascending Gen order
// if the total terminal record count exceeds MaxTerminalEntries.
// Unconsumed terminal records and RUNNING processes are NEVER auto-disposed.
func disposeConsumedTerminals(procBaseDir string) {
	entries, err := os.ReadDir(procBaseDir)
	if err != nil {
		return
	}

	var terminalCount int
	var consumedTerminals []consumedTerminalRecord

	for _, entry := range entries {
		if !entry.IsDir() || !IsProcessRecordDir(entry.Name()) {
			continue
		}
		procDir := filepath.Join(procBaseDir, entry.Name())
		// Enumeration tolerates an unreadable record: skipping it is what lets a corrupt
		// record still be pruned later instead of failing the whole scan.
		meta, err := readMeta(procDir)
		if err != nil {
			continue
		}

		liveness, err := CheckLiveness(procDir, &meta)
		if err != nil {
			continue
		}
		if isTerminal(liveness.Status) {
			terminalCount++
			if meta.ConsumedSeq > 0 {
				consumedTerminals = append(consumedTerminals, consumedTerminalRecord{
					id:   meta.ID,
					gen:  meta.Gen,
					path: procDir,
				})
			}
		}
	}

	if terminalCount <= MaxTerminalEntries || len(consumedTerminals) == 0 {
		return
	}

	// Sort consumed terminals by Gen ascending (lowest gen / oldest creation first)
	sort.Slice(consumedTerminals, func(i, j int) bool {
		return consumedTerminals[i].gen < consumedTerminals[j].gen
	})

	for _, rec := range consumedTerminals {
		if terminalCount <= MaxTerminalEntries {
			break
		}
		// Accepted race: another process may be reading this record while we remove it.
		// Get already returns 'process "id" not found' when os.Stat fails or files disappear,
		// so concurrent reads cleanly return not-found rather than crashing or returning partial output.
		if err := os.RemoveAll(rec.path); err == nil {
			recordDisposedID(procBaseDir, rec.id)
			terminalCount--
		}
	}
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
	var terminalCount int
	var consumedTerminalCount int

	for _, entry := range entries {
		if !entry.IsDir() || !IsProcessRecordDir(entry.Name()) {
			continue
		}
		procID := entry.Name()
		procDir := filepath.Join(procBaseDir, procID)

		// List tolerates an unreadable meta.json and reports the record with whatever it
		// has: hiding a directory that exists is worse than listing it with an empty tool
		// name, and Prune has to stay able to clear it.
		meta, _ := readMeta(procDir)

		liveness, err := CheckLiveness(procDir, &meta)
		if err != nil {
			continue
		}

		if isTerminal(liveness.Status) {
			terminalCount++
			if meta.ConsumedSeq > 0 {
				consumedTerminalCount++
			}
		}

		toolName := meta.Tool
		if toolName == "" {
			toolName = procID
		}

		results = append(results, ProcessInfo{
			ID:        procID,
			Tool:      toolName,
			Args:      meta.Args,
			Status:    liveness.Status,
			PID:       liveness.PID,
			PGID:      liveness.PGID,
			ExitCode:  liveness.ExitCode,
			StartedAt: meta.StartedAt,
		})
	}

	if terminalCount > MaxTerminalEntries && consumedTerminalCount == 0 {
		fmt.Fprintf(os.Stderr, "warning: %d terminal process records exceed cap of %d with 0 disposable; run 'wackyproc prune' to clear\n", terminalCount, MaxTerminalEntries)
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
// When reading a terminal process for the first time (ConsumedSeq == 0), marks it as consumed
// with a monotonic sequence number.
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

	// Output write succeeded. Now check if record should be marked as consumed (D79).
	metaPath := filepath.Join(procDir, MetaFileName)
	metaBytes, err := os.ReadFile(metaPath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("process %q not found", procID)
		}
		fmt.Fprintf(os.Stderr, "warning: failed to read %s for process %q: %v\n", MetaFileName, procID, err)
		return nil
	}

	var meta Meta
	if err := json.Unmarshal(metaBytes, &meta); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to parse %s for process %q: %v\n", MetaFileName, procID, err)
		return nil
	}

	liveness, err := CheckLiveness(procDir, &meta)
	if err != nil {
		return nil
	}
	if isTerminal(liveness.Status) && meta.ConsumedSeq == 0 {
		seq, err := nextSeq(filepath.Join(cwd, ProcDirName))
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to allocate consumed sequence for process %q: %v\n", procID, err)
			return nil
		}
		meta.ConsumedSeq = seq

		updatedBytes, err := json.MarshalIndent(meta, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to serialize %s for process %q: %v\n", MetaFileName, procID, err)
			return nil
		}

		tmpPath := filepath.Join(procDir, fmt.Sprintf("meta.json.tmp.%d", time.Now().UnixNano()))
		if err := os.WriteFile(tmpPath, updatedBytes, 0644); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to write %s for process %q: %v\n", tmpPath, procID, err)
			return nil
		}
		if err := os.Rename(tmpPath, metaPath); err != nil {
			// Best-effort cleanup: remove temporary meta file after rename failure to prevent tmp accumulation;
			// failure to remove is secondary to the logged rename error.
			_ = os.Remove(tmpPath)
			fmt.Fprintf(os.Stderr, "warning: failed to rename %s for process %q: %v\n", MetaFileName, procID, err)
			return nil
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

// ErrNothingToWaitFor is returned by any-mode Wait when there is no process eligible to
// wait on at call time (nothing running, nothing unknown). The CLI treats it as a
// successful no-op rather than a timeout.
var ErrNothingToWaitFor = errors.New("nothing to wait for")

// buildBaselineTerminal records all processes that are already in a terminal state
// when an untargeted Wait begins, so they can be excluded from satisfying the wait.
// If no running processes are eligible to wait on, it returns ErrNothingToWaitFor.
func buildBaselineTerminal(cwd string) (map[string]bool, error) {
	baselineTerminal := make(map[string]bool)
	initList, err := List(cwd)
	if err != nil {
		return nil, err
	}
	// Eligible = processes still running (or in any non-terminal state) at entry. If
	// none exist there is nothing for an any-mode wait to observe: every record is
	// already terminal, and a fresh process spawned after this call starts is not part
	// of this wait's contract. Return immediately instead of burning the timeout.
	anyEligible := false
	for _, p := range initList {
		if isTerminal(p.Status) {
			baselineTerminal[p.ID] = true
		} else {
			anyEligible = true
		}
	}
	if !anyEligible {
		return nil, ErrNothingToWaitFor
	}
	return baselineTerminal, nil
}

// findTerminalProcess checks a process listing for completion against the target or baseline.
func findTerminalProcess(cwd string, list []ProcessInfo, hasTarget bool, target string, baselineTerminal map[string]bool) (string, bool, error) {
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
				return "", false, fmt.Errorf("process %q not found", target)
			}
			return "", false, nil
		}
		if isTerminal(found.Status) {
			return found.ID, true, nil
		}
		return "", false, nil
	}

	for _, p := range list {
		if isTerminal(p.Status) {
			if baselineTerminal[p.ID] {
				continue
			}
			return p.ID, true, nil
		}
	}
	return "", false, nil
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
		var err error
		baselineTerminal, err = buildBaselineTerminal(cwd)
		if err != nil {
			return "", err
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

		if id, done, err := findTerminalProcess(cwd, list, hasTarget, target, baselineTerminal); err != nil {
			return "", err
		} else if done {
			return id, nil
		}

		if time.Now().After(deadline) {
			return "", nil
		}

		<-ticker.C
	}
}

// Stop timings. The grace period is exported because the CLI flag advertises it as its
// default; the poll interval is the granularity of the terminal-state check.
const (
	DefaultStopTimeoutSeconds = 3
	stopPollInterval          = 50 * time.Millisecond
)

// Stop terminates the process group associated with procID using SIGTERM, followed by
// SIGKILL if needed. Every failure it cannot rule out is reported: an unreadable record, a
// signal the kernel refused, and a process still running after SIGKILL.
func Stop(cwd string, procID string, timeoutSeconds int) error {
	procDir := filepath.Join(cwd, ProcDirName, procID)
	if _, err := os.Stat(procDir); os.IsNotExist(err) {
		return fmt.Errorf("process %q not found", procID)
	}

	// An unreadable meta.json is refused rather than replaced by a zero Meta: the zero value
	// has no StartTime, which is precisely the state that skips the PID-recycling guard in
	// CheckLiveness and then signals a group that may belong to another process.
	meta, err := readMeta(procDir)
	if err != nil {
		return fmt.Errorf("cannot stop %q: %w", procID, err)
	}

	liveness, err := CheckLiveness(procDir, &meta)
	if err != nil {
		return err
	}
	if liveness.Status != StatusRunning {
		return nil // Already terminated
	}

	target, err := signalTarget(liveness.PID, liveness.PGID)
	if err != nil {
		return fmt.Errorf("invalid PID/PGID for process %q: %w", procID, err)
	}

	// Send SIGTERM to the process group. ESRCH means the target vanished between the liveness
	// check and the signal, which is the outcome the caller asked for.
	if err := syscall.Kill(target, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("failed to signal %d to stop process %q: %w", target, procID, err)
	}

	if timeoutSeconds <= 0 {
		timeoutSeconds = DefaultStopTimeoutSeconds
	}

	deadline := time.Now().Add(time.Duration(timeoutSeconds) * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(stopPollInterval)
		liveness, err := CheckLiveness(procDir, &meta)
		if err != nil {
			return err
		}
		if liveness.Status != StatusRunning {
			return nil
		}
	}

	// Force kill with SIGKILL if still running
	if err := syscall.Kill(target, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("failed to force kill %d for process %q: %w", target, procID, err)
	}

	// Verify rather than assume: a SIGKILL that was accepted but leaves the record RUNNING is
	// a real problem (an unowned group, for instance) and must not be reported as stopped.
	time.Sleep(stopPollInterval)
	liveness, err = CheckLiveness(procDir, &meta)
	if err != nil {
		return err
	}
	if liveness.Status == StatusRunning {
		return fmt.Errorf("process %q is still running after SIGKILL to %d", procID, target)
	}
	return nil
}

// signalByName returns the signal for a name with SIG- prefix and case normalization
// removed ("KILL", "kill", "SIGKILL", "9" all map to SIGKILL).
var signalByName = map[string]syscall.Signal{
	"HUP":   syscall.SIGHUP,
	"INT":   syscall.SIGINT,
	"QUIT":  syscall.SIGQUIT,
	"KILL":  syscall.SIGKILL,
	"TERM":  syscall.SIGTERM,
	"USR1":  syscall.SIGUSR1,
	"USR2":  syscall.SIGUSR2,
	"CONT":  syscall.SIGCONT,
	"STOP":  syscall.SIGSTOP,
	"WINCH": syscall.SIGWINCH,
}

// ParseSignal resolves a user-supplied signal token - numeric or named, case-insensitive,
// SIG- prefix optional - into a syscall.Signal. Unknown names and unknown numbers return
// an error naming the offending token so the caller can report it without silently
// falling back to any default.
func ParseSignal(token string) (syscall.Signal, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return 0, fmt.Errorf("signal is required")
	}
	if n, err := strconv.Atoi(token); err == nil {
		// Bound numeric tokens to the portable signal range (1..64 on Linux) so nonsense
		// numbers like 999 fail loudly instead of becoming syscall.Signal(999).
		if n >= 1 && n <= 64 {
			return syscall.Signal(n), nil
		}
		return 0, fmt.Errorf("unknown signal number %q", token)
	}
	name := strings.ToUpper(token)
	name = strings.TrimPrefix(name, "SIG")
	if sig, ok := signalByName[name]; ok {
		return sig, nil
	}
	return 0, fmt.Errorf("unknown signal %q", token)
}

// Signal sends the given signal to the process group associated with procID. Only RUNNING
// processes are signaled; a terminal process is a clean no-op. The signal is dispatched to
// the whole group (-pgid) mirroring Stop's target selection.
func Signal(cwd string, procID string, sig syscall.Signal) error {
	procDir := filepath.Join(cwd, ProcDirName, procID)
	if _, err := os.Stat(procDir); os.IsNotExist(err) {
		return fmt.Errorf("process %q not found", procID)
	}

	// Same rule as Stop: without a readable meta.json the anti-recycling signature is unknown,
	// so the signal is refused instead of aimed at a possibly recycled group.
	meta, err := readMeta(procDir)
	if err != nil {
		return fmt.Errorf("cannot signal %q: %w", procID, err)
	}

	liveness, err := CheckLiveness(procDir, &meta)
	if err != nil {
		return err
	}
	if liveness.Status != StatusRunning {
		return nil // Already terminated
	}

	target, err := signalTarget(liveness.PID, liveness.PGID)
	if err != nil {
		return fmt.Errorf("invalid PID/PGID for process %q: %w", procID, err)
	}

	return syscall.Kill(target, sig)
}

// signalTarget picks the syscall.Kill target for a process record: the process group
// (-pgid) when we have a real group, else the process itself. Deliberately never -pid:
// with pid == 1 (container init or a supervisor masquerading as it), kill(-1, sig) would
// broadcast to every process the caller can reach. Shared by Signal and Stop.
func signalTarget(pid, pgid int) (int, error) {
	target := -pgid
	if pgid <= 0 {
		target = pid
		if pid <= 0 {
			return 0, fmt.Errorf("pid %d <= 0", pid)
		}
	}
	return target, nil
}

// Kill force-terminates the process group associated with procID with SIGKILL immediately.
// Unlike Stop it never waits and never sends SIGTERM first; this is the operator's
// force-kill for a wedged process that refuses graceful cleanup.
func Kill(cwd string, procID string) error {
	return Signal(cwd, procID, syscall.SIGKILL)
}

// Prune disposes ALL terminal (COMPLETED, FAILED, CRASHED) process records regardless of consumed state.
// Reports each removed process ID and tool to report. RUNNING processes are untouched.
// If .proc/ does not exist or has no terminal records, Prune is a clean no-op.
func Prune(cwd string, report io.Writer) error {
	procBaseDir := filepath.Join(cwd, ProcDirName)
	entries, err := os.ReadDir(procBaseDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("failed to read %s directory: %w", ProcDirName, err)
	}

	for _, entry := range entries {
		if !entry.IsDir() || !IsProcessRecordDir(entry.Name()) {
			continue
		}
		procID := entry.Name()
		procDir := filepath.Join(procBaseDir, procID)

		// Prune tolerates an unreadable meta.json on purpose: meta only supplies the reported
		// tool name, and refusing here would leave a corrupt record permanently undeletable.
		meta, _ := readMeta(procDir)

		toolName := meta.Tool
		if toolName == "" {
			toolName = procID
		}

		liveness, _ := CheckLiveness(procDir, &meta)
		if isTerminal(liveness.Status) {
			if err := os.RemoveAll(procDir); err != nil {
				return fmt.Errorf("failed to remove process record %s: %w", procID, err)
			}
			recordDisposedID(procBaseDir, procID)
			if report != nil {
				fmt.Fprintf(report, "pruned %s (%s)\n", procID, toolName)
			}
		}
	}

	return nil
}

// Unconsume clears the ConsumedSeq of a process record.
// If procID does not exist, returns the standard 'process %q not found' error.
// Works on running records too (clears preemptively).
func Unconsume(cwd string, procID string) error {
	procDir := filepath.Join(cwd, ProcDirName, procID)
	if _, err := os.Stat(procDir); os.IsNotExist(err) {
		return fmt.Errorf("process %q not found", procID)
	}

	metaPath := filepath.Join(procDir, MetaFileName)
	metaBytes, err := os.ReadFile(metaPath)
	if err != nil {
		return fmt.Errorf("failed to read %s for process %q: %w", MetaFileName, procID, err)
	}

	var meta Meta
	if err := json.Unmarshal(metaBytes, &meta); err != nil {
		return fmt.Errorf("failed to parse %s for process %q: %w", MetaFileName, procID, err)
	}

	meta.ConsumedSeq = 0

	updatedBytes, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to serialize %s: %w", MetaFileName, err)
	}

	tmpPath := filepath.Join(procDir, fmt.Sprintf("meta.json.tmp.%d", time.Now().UnixNano()))
	if err := os.WriteFile(tmpPath, updatedBytes, 0644); err != nil {
		return fmt.Errorf("failed to write temporary %s: %w", MetaFileName, err)
	}
	if err := os.Rename(tmpPath, metaPath); err != nil {
		// Best-effort cleanup: remove temporary meta file after rename failure to prevent tmp accumulation;
		// failure to remove does not supersede returning the rename error.
		_ = os.Remove(tmpPath)
		return fmt.Errorf("failed to update %s: %w", MetaFileName, err)
	}

	return nil
}

// Remove force-disposes a process record regardless of status or consumed state. This is
// the escape hatch for stuck RUNNING records: a process that died (or hung) outside the
// supervisor's capture window leaves no exit, so prune - which only touches terminal
// records - can never clear it. The process itself is NOT signaled: if it is still alive
// it keeps running unmanaged, so stop or kill it first if you want it gone. The record's
// ID is added to the disposed list and never re-claimed. Returns the tool name for reporting.
func Remove(cwd string, procID string) (string, error) {
	procBaseDir := filepath.Join(cwd, ProcDirName)
	procDir := filepath.Join(procBaseDir, procID)
	if _, err := os.Stat(procDir); os.IsNotExist(err) {
		return "", fmt.Errorf("process %q not found", procID)
	}

	// meta only supplies the reported tool name; an unreadable record is still removable.
	meta, _ := readMeta(procDir)
	toolName := meta.Tool
	if toolName == "" {
		toolName = procID
	}

	if err := os.RemoveAll(procDir); err != nil {
		return "", fmt.Errorf("failed to remove process record %s: %w", procID, err)
	}
	recordDisposedID(procBaseDir, procID)
	return toolName, nil
}
