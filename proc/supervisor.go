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
	// Attest that wackyproc supervised this process: the env var gates downstream
	// supervised-only features (e.g. wackypub agent prompt --async), and its value is
	// THIS supervisor's pid (os.Getpid() below equals the pid written to
	// SupervisorPIDFileName above, so the two agree by construction).
	cmd.Env = append(cmd.Env, SupervisedEnvVar+"="+strconv.Itoa(os.Getpid()))
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
		// Record the 127 marker so the record reads as a failed run rather than a crash, and
		// report both failures if that record cannot be written - the start error is the one
		// the operator needs, the marker error explains why the record looks wrong afterwards.
		if writeErr := writeRecord(procDir, ExitCodeFileName, "127\n"); writeErr != nil {
			return fmt.Errorf("%w (and %v)", err, writeErr)
		}
		// Best-effort breadcrumb for whoever reads the stderr capture; the returned error
		// already carries the start failure.
		_, _ = fmt.Fprintf(stderrFile, "failed to start %s: %v\n", meta.ToolPath, err)
		return fmt.Errorf("failed to start %s: %w", meta.ToolPath, err)
	}

	pid := cmd.Process.Pid
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

	// Wait for target command to terminate
	waitErr := cmd.Wait()
	exitCode := exitCodeFromWaitError(waitErr)

	// Record final exit code: this is the write whose absence turns a clean exit into a
	// reported CRASHED, so it is returned rather than discarded.
	if err := writeRecord(procDir, ExitCodeFileName, strconv.Itoa(exitCode)+"\n"); err != nil {
		return err
	}
	return nil
}

// exitCodeFromWaitError maps what cmd.Wait reported onto the shell convention: the tool's
// own status, 128+signal when it was killed by a signal, and 1 for a Wait failure that is
// not an exit status at all (for example the pipes going away).
func exitCodeFromWaitError(waitErr error) int {
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

// abortUntrackableChild stops a child we started but cannot record, preferring the process
// group so the whole subtree goes with it.
func abortUntrackableChild(pid, pgid int) {
	target, err := signalTarget(pid, pgid)
	if err != nil {
		return
	}
	_ = syscall.Kill(target, syscall.SIGKILL)
}
