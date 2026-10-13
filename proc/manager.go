package proc

import (
	"context"
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
	cmd.Env = append(os.Environ(), SuperviseEntrypointEnv+"=1")
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

	// Post-spawn retirement of the oldest terminal records once the cap is exceeded.
	retireOldestTerminals(procBaseDir)

	return procID, nil
}

func isTerminal(status string) bool {
	return status == StatusCompleted || status == StatusFailed || status == StatusCrashed
}

// Describe returns the full-detail view of a single process record (args, cwd, tool path,
// output-file locations). This backs the wackyproc describe command; it never
// writes record state.
func Describe(cwd string, procID string) (*DescribeInfo, error) {
	procDir := filepath.Join(cwd, ProcDirName, procID)
	if _, err := os.Stat(procDir); os.IsNotExist(err) {
		return nil, fmt.Errorf("process %q not found", procID)
	}

	meta, err := readMeta(procDir)
	if err != nil {
		return nil, fmt.Errorf("reading meta for %q: %w", procID, err)
	}

	liveness, err := CheckLiveness(procDir, &meta)
	if err != nil {
		return nil, fmt.Errorf("checking liveness for %q: %w", procID, err)
	}

	stdinPath := filepath.Join(procDir, StdinFileName)
	stdinFile := ""
	if _, err := os.Stat(stdinPath); err == nil {
		stdinFile = stdinPath
	}

	return &DescribeInfo{
		ID:         procID,
		Tool:       meta.Tool,
		ToolPath:   meta.ToolPath,
		Args:       meta.Args,
		Cwd:        meta.Cwd,
		Status:     liveness.Status,
		PID:        liveness.PID,
		PGID:       liveness.PGID,
		ExitCode:   liveness.ExitCode,
		StartedAt:  meta.StartedAt,
		StdoutFile: filepath.Join(procDir, StdoutFileName),
		StderrFile: filepath.Join(procDir, StderrFileName),
		StdinFile:  stdinFile,
	}, nil
}

// retireOldestTerminals keeps the terminal record count at or below
// MaxTerminalEntries by retiring the oldest terminal records (ascending Gen)
// on overflow, regardless of any other state. There is no consumed
// distinction: eviction is age-based, so a record can be retired whether or
// not its output was ever read. Each retirement is logged; a failed
// retirement is logged as an error and retried on the next cap check (the
// record still counts toward the cap while it exists).
func retireOldestTerminals(procBaseDir string) {
	entries, err := os.ReadDir(procBaseDir)
	if err != nil {
		return
	}
	type retirement struct {
		id   string
		gen  uint64
		path string
	}
	var terminals []retirement
	for _, entry := range entries {
		if !entry.IsDir() || !IsProcessRecordDir(entry.Name()) {
			continue
		}
		procDir := filepath.Join(procBaseDir, entry.Name())
		meta, err := readMeta(procDir)
		if err != nil {
			continue
		}
		liveness, err := CheckLiveness(procDir, &meta)
		if err != nil || !isTerminal(liveness.Status) {
			continue
		}
		terminals = append(terminals, retirement{meta.ID, meta.Gen, procDir})
	}
	if len(terminals) <= MaxTerminalEntries {
		return
	}
	sort.SliceStable(terminals, func(i, j int) bool { return terminals[i].gen < terminals[j].gen })
	for _, rec := range terminals[:len(terminals)-MaxTerminalEntries] {
		if err := os.RemoveAll(rec.path); err != nil {
			fmt.Fprintf(os.Stderr, "error: failed to retire terminal record %s: %v\n", rec.id, err)
			continue
		}
		recordDisposedID(procBaseDir, rec.id)
		fmt.Fprintf(os.Stderr, "retired terminal record %s (cap %d)\n", rec.id, MaxTerminalEntries)
	}
}

// List inspects all process directories in <cwd>/.proc/ and returns their
// status. Terminal record retirement runs before the snapshot, so a record
// the cap is about to drop is never reported by the same call.
func List(cwd string) ([]ProcessInfo, error) {
	procBaseDir := filepath.Join(cwd, ProcDirName)
	retireOldestTerminals(procBaseDir)
	entries, err := os.ReadDir(procBaseDir)
	if err != nil {
		if os.IsNotExist(err) {
			return []ProcessInfo{}, nil
		}
		return nil, fmt.Errorf("failed to read %s directory: %w", ProcDirName, err)
	}

	var results []ProcessInfo

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

	sort.Slice(results, func(i, j int) bool {
		return results[i].StartedAt < results[j].StartedAt
	})

	if results == nil {
		results = []ProcessInfo{}
	}
	return results, nil
}

// Get dumps captured stdout and stderr for procID to the provided writers.
// It streams output only: the record is not marked in any way, so it stays
// eligible for cap retirement like any other terminal record. meta.json is
// existence-checked but never modified: a record whose meta has been disposed
// concurrently is reported as not found.
func Get(cwd string, procID string, stdoutWriter io.Writer, stderrWriter io.Writer) error {
	procDir := filepath.Join(cwd, ProcDirName, procID)
	if _, err := os.Stat(procDir); os.IsNotExist(err) {
		return fmt.Errorf("process %q not found", procID)
	}
	if _, err := os.Stat(filepath.Join(procDir, MetaFileName)); os.IsNotExist(err) {
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
// Empty streams write nothing. It performs no state modification.
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

// findCompleted maps each listed ID to its terminal state from a process
// listing. A listed ID absent from the listing is an error (fail fast, like a
// record that never existed). List already runs CheckLiveness per record, so
// statuses are live and the #13 zombie gate composes with this wait.
func findCompleted(list []ProcessInfo, ids []string) (map[string]bool, error) {
	byID := make(map[string]ProcessInfo, len(list))
	for _, p := range list {
		byID[p.ID] = p
	}
	terminal := make(map[string]bool, len(ids))
	for _, id := range ids {
		p, ok := byID[id]
		if !ok {
			return nil, fmt.Errorf("process %q not found", id)
		}
		if isTerminal(p.Status) {
			terminal[id] = true
		}
	}
	return terminal, nil
}

// runningPidsFor returns the PIDs of the listed IDs still running per the
// listing, for pidfd arming. anyPending is true when at least one listed ID is
// non-terminal with no PID recorded yet (still spawning): the caller re-lists
// immediately instead of sleeping a full polling tick.
func runningPidsFor(list []ProcessInfo, ids []string, terminal map[string]bool) (pids []int, anyPending bool) {
	for _, id := range ids {
		if terminal[id] {
			continue
		}
		for i := range list {
			if list[i].ID != id {
				continue
			}
			anyPending = true
			if list[i].PID > 0 {
				pids = append(pids, list[i].PID)
			}
			break
		}
	}
	return pids, anyPending
}

// waitPollIntervalMs is the wait-loop poll interval to sleep after pollsSoFar
// polls: the first WaitPollRampCount polls keep the start interval (short tasks
// are still detected within ~2 intervals of finishing), then the interval
// doubles per poll until it settles at WaitPollSettleIntervalMs (low-duty-cycle
// CPU for long waits).
func waitPollIntervalMs(pollsSoFar int) int {
	if pollsSoFar < WaitPollRampCount {
		return WaitPollStartIntervalMs
	}
	iv := WaitPollStartIntervalMs
	for i := 0; i < pollsSoFar-WaitPollRampCount+1; i++ {
		iv *= 2
		if iv >= WaitPollSettleIntervalMs {
			return WaitPollSettleIntervalMs
		}
	}
	return iv
}

// ForcePollingFallbackForTest is a test-only escape hatch: when true, Wait skips
// the pidfd fast path and uses the polling ramp fallback even on Linux where
// pidfd is supported. Lets the fallback be exercised deterministically on any
// platform.
var ForcePollingFallbackForTest bool

// Wait is the context-less first-completed form of WaitContext.
func Wait(cwd string, timeoutSeconds int, ids ...string) (string, error) {
	return WaitContext(context.Background(), cwd, timeoutSeconds, ids...)
}

// WaitAll is the context-less barrier form of WaitAllContext.
func WaitAll(cwd string, timeoutSeconds int, ids ...string) (string, error) {
	return WaitAllContext(context.Background(), cwd, timeoutSeconds, ids...)
}

// WaitContext blocks until any of the listed processes is in a terminal state
// (COMPLETED, FAILED, or CRASHED) and returns the ID of the first completed
// process, in caller order. A listed ID already terminal when the call starts
// is returned immediately. If the deadline passes first, WaitContext returns
// ("", nil) with no error; a listed ID with no record is an error.
//
// The loop re-runs List each tick, so terminal detection is the same liveness
// scan as wackyproc list. On Linux it arms one pidfd per listed running PID
// and falls back to the polling ramp when the kernel cannot deliver a wake;
// ForcePollingFallbackForTest exercises that fallback deterministically.
func WaitContext(ctx context.Context, cwd string, timeoutSeconds int, ids ...string) (string, error) {
	return waitSetContext(ctx, cwd, timeoutSeconds, false, ids...)
}

// WaitAllContext blocks until every listed process is in a terminal state and
// returns the ID of the last completed process (the one that satisfied the
// barrier). It returns ("", nil) if the deadline passes first.
func WaitAllContext(ctx context.Context, cwd string, timeoutSeconds int, ids ...string) (string, error) {
	return waitSetContext(ctx, cwd, timeoutSeconds, true, ids...)
}

// waitSetContext is the shared loop behind WaitContext (first completed) and
// WaitAllContext (barrier).
func waitSetContext(ctx context.Context, cwd string, timeoutSeconds int, all bool, ids ...string) (string, error) {
	if len(ids) == 0 {
		return "", fmt.Errorf("wait requires at least one process ID")
	}
	// Dedupe, preserving caller order.
	seen := make(map[string]bool, len(ids))
	set := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		set = append(set, id)
	}
	if len(set) == 0 {
		return "", fmt.Errorf("wait requires at least one process ID")
	}
	// A missing ID has to be rejected here rather than falling through:
	// filepath.Join drops empty components, so an empty ID would resolve to the
	// .proc directory itself - which exists once any process has run - and a
	// wait with nothing named would block the full timeout instead of failing
	// fast.
	procBaseDir := filepath.Join(cwd, ProcDirName)
	for _, id := range set {
		if _, err := os.Stat(filepath.Join(procBaseDir, id)); os.IsNotExist(err) {
			return "", fmt.Errorf("process %q not found", id)
		}
	}

	var pidfdTried bool
	// pendingWake is set after a pidfd wake or a reaped-pid ESRCH: a process is
	// dead but the supervisor may not have written exit_code yet (CheckLiveness
	// still reports RUNNING in that window). While pendingWake, the loop
	// re-checks at a fast 5ms cadence instead of the ramp, so marker detection
	// is bounded by the supervisor write, not by a full tick.
	var pendingWake bool
	deadline := time.Now().Add(time.Duration(clampWaitSeconds(timeoutSeconds)) * time.Second)
	intervalMs := waitPollIntervalMs(0)
	ticker := time.NewTicker(time.Duration(intervalMs) * time.Millisecond)
	defer ticker.Stop()

	polls := 0
	lastCompleted := ""
	prevTerminal := map[string]bool{}

	for {
		list, err := List(cwd)
		if err != nil {
			return "", err
		}
		terminal, err := findCompleted(list, set)
		if err != nil {
			return "", err
		}
		for _, id := range set {
			if terminal[id] && !prevTerminal[id] {
				lastCompleted = id
			}
		}
		prevTerminal = terminal
		if !all {
			if lastCompleted != "" {
				return lastCompleted, nil
			}
		} else {
			allTerminal := true
			for _, id := range set {
				if !terminal[id] {
					allTerminal = false
					break
				}
			}
			if allTerminal {
				if lastCompleted == "" {
					// Every listed ID was already terminal at call start: report the
					// last one in caller order so the contract always returns an ID.
					lastCompleted = set[len(set)-1]
				}
				return lastCompleted, nil
			}
		}

		if time.Now().After(deadline) {
			return "", nil
		}

		// pidfd fast path (Linux): arm one pidfd per listed running process and
		// block until one exits or the deadline passes. Zero polling CPU and zero
		// detection latency; the kernel wakes us exactly when a process exits.
		// Falls back to the polling ramp on unsupported kernels / sandboxes /
		// non-Linux. A wake is not a verdict - the loop re-runs List +
		// findCompleted, so liveness re-derives from the markers, then the tool-zombie
		// gate and PID-reuse detection. The zombie gate is the one added in proc/liveness.go
		// by this change; an earlier revision of this comment credited a #13 gate that
		// was never implemented, and #16 was built believing it existed.
		if !pidfdTried && !ForcePollingFallbackForTest {
			pids, anyPending := runningPidsFor(list, set, terminal)
			if len(pids) > 0 {
				remaining := time.Until(deadline)
				woke, _, perr := waitPidFDsContext(ctx, pids, remaining)
				if perr != nil {
					if errors.Is(perr, ErrPidFDProcessGone) {
						// pidfd_open raced the reap: a process is gone (ESRCH). Do not
						// re-arm pidfd (a dead pid would ESRCH-loop); fall to the fast
						// pendingWake re-check until the supervisor writes exit_code.
						pidfdTried = true
						pendingWake = true
						continue
					}
					pidfdTried = true
				} else if woke {
					// A listed process exited (pidfd fired): re-list and let
					// findCompleted + CheckLiveness confirm the terminal state
					// (zombie / reap edge cases). Disable pidfd for the rest of this
					// wait: re-arming a dead (possibly zombie) pid would just wake
					// immediately again and spin while the supervisor asynchronously
					// writes exit_code. pendingWake drops the ticker to 5ms until the
					// marker lands.
					pidfdTried = true
					pendingWake = true
					continue
				} else {
					return "", nil
				}
			} else if anyPending {
				// Non-terminal record(s) exist but no PID is recorded yet (still
				// spawning): re-list immediately so a fast-exiting process is caught
				// on this cycle, not after a full 100ms tick.
				continue
			}
		}

		polls++
		iv := waitPollIntervalMs(polls)
		if pendingWake && iv > 5 {
			iv = 5
		}
		if iv != intervalMs {
			intervalMs = iv
			ticker.Reset(time.Duration(iv) * time.Millisecond)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-ticker.C:
		}
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

// Prune disposes ALL terminal (COMPLETED, FAILED, CRASHED) process records regardless of any other state.
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

// Remove force-disposes a process record regardless of status. This is // the escape hatch for stuck RUNNING records: a process that died (or hung) outside the
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
