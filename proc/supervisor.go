package proc

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
)

// writeRecord persists one process record file under procDir. Every file written here is
// read back by CheckLiveness, Stop and Signal, so a lost write is not cosmetic: a missing
// exit_code makes a clean exit read as CRASHED, and a missing pid or pgid leaves Stop with
// nothing to signal.
func writeRecord(procDir, name, content string) error {
	if err := os.WriteFile(filepath.Join(procDir, name), []byte(content), 0644); err != nil {
		return fmt.Errorf("failed to record %s in %s: %w", name, procDir, err)
	}
	return nil
}

// supervisorStreams holds the open file handles for a supervised tool's I/O.
type supervisorStreams struct {
	stdout *os.File
	stderr *os.File
	stdin  *os.File
}

func (s *supervisorStreams) Close() {
	if s.stdout != nil {
		_ = s.stdout.Close()
	}
	if s.stderr != nil {
		_ = s.stderr.Close()
	}
	if s.stdin != nil {
		_ = s.stdin.Close()
	}
}

// openStreams prepares stdout, stderr, and optional stdin files under procDir.
func openStreams(procDir string) (*supervisorStreams, error) {
	stdoutPath := filepath.Join(procDir, StdoutFileName)
	stdoutFile, err := os.OpenFile(stdoutPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return nil, fmt.Errorf("failed to create %s: %w", StdoutFileName, err)
	}

	stderrPath := filepath.Join(procDir, StderrFileName)
	stderrFile, err := os.OpenFile(stderrPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		stdoutFile.Close()
		return nil, fmt.Errorf("failed to create %s: %w", StderrFileName, err)
	}

	var stdinFile *os.File
	stdinPath := filepath.Join(procDir, StdinFileName)
	if _, err := os.Stat(stdinPath); err == nil {
		if f, err := os.Open(stdinPath); err == nil {
			stdinFile = f
		}
	}

	return &supervisorStreams{stdout: stdoutFile, stderr: stderrFile, stdin: stdinFile}, nil
}

// startTool spawns the target tool isolated into its own process group with streams wired.
func startTool(procDir string, meta *Meta, streams *supervisorStreams) (*exec.Cmd, error) {
	cmd := exec.Command(meta.ToolPath, meta.Args...)
	cmd.Dir = meta.Cwd
	cmd.Env = os.Environ()
	// Attest that wackyproc supervised this process: the env var gates downstream
	// supervised-only features (e.g. wackypub agent prompt --async), and its value is
	// THIS supervisor's pid (os.Getpid() below equals the pid written to
	// SupervisorPIDFileName above, so the two agree by construction).
	cmd.Env = append(cmd.Env, SupervisedEnvVar+"="+strconv.Itoa(os.Getpid()))
	cmd.Stdout = streams.stdout
	cmd.Stderr = streams.stderr
	if streams.stdin != nil {
		cmd.Stdin = streams.stdin
	}

	// Isolate target command into its own process group
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}

	if err := cmd.Start(); err != nil {
		// Record the 127 marker so the record reads as a failed run rather than a crash, and
		// report both failures if that record cannot be written - the start error is the one
		// the operator needs, the marker error explains why the record looks wrong afterwards.
		if writeErr := writeRecord(procDir, ExitCodeFileName, "127\n"); writeErr != nil {
			return nil, fmt.Errorf("%w (and %v)", err, writeErr)
		}
		// Best-effort breadcrumb for whoever reads the stderr capture; the returned error
		// already carries the start failure. If writing to stderr fails, returning the error
		// to the caller remains the authoritative notification.
		_, _ = fmt.Fprintf(streams.stderr, "failed to start %s: %v\n", meta.ToolPath, err)
		return nil, fmt.Errorf("failed to start %s: %w", meta.ToolPath, err)
	}
	return cmd, nil
}

// recordStart records PID, PGID, and start-time signature under procDir.
func recordStart(procDir string, meta *Meta, pid int) error {
	pgid, err := syscall.Getpgid(pid)
	if err != nil {
		pgid = pid
	}

	// Record PID and PGID. Without them Stop and Signal have no target, and the child would
	// keep running as an untrackable orphan, so a failed record is fatal here: kill the
	// group we just created before returning.
	if err := writeRecord(procDir, PIDFileName, strconv.Itoa(pid)+"\n"); err != nil {
		abortUntrackableChild(pid, pgid)
		return err
	}
	if err := writeRecord(procDir, PGIDFileName, strconv.Itoa(pgid)+"\n"); err != nil {
		abortUntrackableChild(pid, pgid)
		return err
	}

	// Record process start time signature to detect PID recycling. Deliberately best-effort:
	// the child is already running and recorded, and killing a live, tracked process because
	// its anti-recycling signature could not be persisted is worse than the degraded guard.
	// Stop still refuses to act on a record whose meta.json cannot be read at all.
	startTime := GetProcessStartTime(pid)
	if startTime != "" && startTime != meta.StartTime {
		meta.StartTime = startTime
		if updatedBytes, err := json.MarshalIndent(meta, "", "  "); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to encode start time for pid %d: %v\n", pid, err)
		} else if err := os.WriteFile(filepath.Join(procDir, MetaFileName), updatedBytes, 0644); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to persist start time for pid %d, PID-recycling detection is disabled for this record: %v\n", pid, err)
		}
	}
	return nil
}

// awaitExit waits for cmd to terminate and writes the final exit code record.
func awaitExit(procDir string, cmd *exec.Cmd) error {
	waitErr := cmd.Wait()
	exitCode := exitCodeFromProcessState(waitErr)

	// Record final exit code: this is the write whose absence turns a clean exit into a
	// reported CRASHED, so it is returned rather than discarded.
	if err := writeRecord(procDir, ExitCodeFileName, strconv.Itoa(exitCode)+"\n"); err != nil {
		return err
	}
	return nil
}

// Supervise runs the target tool as a child in its own process group (Setpgid: true),
// piping its stdin/stdout/stderr to files in procDir, and recording PID, PGID, and exit code.
func Supervise(procDir string) error {
	meta, err := readMeta(procDir)
	if err != nil {
		return err
	}

	// Record supervisor PID before starting the child: CheckLiveness consults it to tell
	// "child between exit and exit_code written" from "supervisor gone, so CRASHED".
	if err := writeRecord(procDir, SupervisorPIDFileName, strconv.Itoa(os.Getpid())+"\n"); err != nil {
		return err
	}

	streams, err := openStreams(procDir)
	if err != nil {
		return err
	}
	defer streams.Close()

	cmd, err := startTool(procDir, &meta, streams)
	if err != nil {
		return err
	}

	if err := recordStart(procDir, &meta, cmd.Process.Pid); err != nil {
		return err
	}

	return awaitExit(procDir, cmd)
}

// exitCodeFromProcessState maps what cmd.Wait reported onto the shell convention: the tool's
// own status, 128+signal when it was killed by a signal, and 1 for a Wait failure that is
// not an exit status at all (for example the pipes going away).
func exitCodeFromProcessState(waitErr error) int {
	if waitErr == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok {
			if ws.Signaled() {
				return 128 + int(ws.Signal())
			}
			return ws.ExitStatus()
		}
		return exitErr.ExitCode()
	}
	return 1
}

// exitCodeFromWaitError is an alias for exitCodeFromProcessState.
func exitCodeFromWaitError(waitErr error) int {
	return exitCodeFromProcessState(waitErr)
}

// abortUntrackableChild stops a child we started but cannot record, preferring the process
// group so the whole subtree goes with it.
func abortUntrackableChild(pid, pgid int) {
	target, err := signalTarget(pid, pgid)
	if err != nil {
		return
	}
	// Best-effort kill of untrackable child process group: error is ignored because the caller
	// is already reporting the fatal record-write failure that triggered the abort.
	_ = syscall.Kill(target, syscall.SIGKILL)
}
