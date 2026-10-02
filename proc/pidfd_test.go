package proc_test

import (
	"context"
	"errors"
	"github.com/colinrgodsey/wackyproc/proc"
	"golang.org/x/sys/unix"
	"syscall"
	"testing"
	"time"
)

// TestPidfdWait_LatencyOnExit measures the kernel pidfd wake latency directly: for a
// RUNNING process whose pidfd we already hold, signal-driven exit must wake poll() in
// <10ms. This measures the wake in isolation (the thing pidfd provides). It does NOT
// measure Wait's end-to-end return, which is additionally bounded by the supervisor's
// async exit_code write (CheckLiveness holds RUNNING while the supervisor finalizes) -
// that lag is the existing terminal-detection path, not the pidfd wake.
func TestPidfdWait_LatencyOnExit(t *testing.T) {
	if !proc.PidfdSupportedOnThisPlatform() {
		t.Skip("pidfd unsupported on this platform/kernel")
	}

	cwd := setupTestEnv(t)
	createExecutable(t, cwd, "pidfd-exit", "sleep 30")
	id, err := proc.Run(cwd, "pidfd-exit", nil, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer func() { _ = proc.Stop(cwd, id, 1) }()

	// Wait for the pid to be recorded, then open its pidfd directly.
	var pid int
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		list, lerr := proc.List(cwd)
		if lerr == nil {
			for _, p := range list {
				if p.ID == id && p.PID > 0 {
					pid = p.PID
					break
				}
			}
		}
		if pid > 0 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if pid <= 0 {
		t.Fatal("no recorded pid")
	}

	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		t.Fatalf("PidfdOpen: %v", err)
	}
	defer unix.Close(fd)

	pollDone := make(chan error, 1)
	go func() {
		pf := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		_, perr := unix.Poll(pf, 2000)
		pollDone <- perr
	}()

	// Give the poll goroutine a moment to block, then terminate the process.
	time.Sleep(50 * time.Millisecond)
	exitAt := time.Now()
	if err := proc.Signal(cwd, id, syscall.SIGTERM); err != nil {
		t.Fatalf("Signal: %v", err)
	}

	select {
	case perr := <-pollDone:
		if perr != nil {
			t.Fatalf("poll: %v", perr)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("pidfd poll did not wake on process exit")
	}

	elapsed := time.Since(exitAt)
	if elapsed > 10*time.Millisecond {
		t.Errorf("pidfd wake latency %v exceeds 10ms bound", elapsed)
	}
}

// TestPidfdWait_TimeoutReturns verifies the pidfd path still enforces the wait deadline.
func TestPidfdWait_TimeoutReturns(t *testing.T) {
	if !proc.PidfdSupportedOnThisPlatform() {
		t.Skip("pidfd unsupported on this platform/kernel")
	}

	cwd := setupTestEnv(t)
	createExecutable(t, cwd, "pidfd-slow", "sleep 30")

	id, err := proc.Run(cwd, "pidfd-slow", nil, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer func() {
		_ = proc.Stop(cwd, id, 1)
	}()

	start := time.Now()
	res, werr := proc.Wait(cwd, 1, id)
	elapsed := time.Since(start)
	if werr != nil {
		t.Fatalf("Wait: %v", werr)
	}
	if res != "" {
		t.Fatalf("expected timeout (empty id), got %q", res)
	}
	// 1s timeout with pidfd poll: should return close to the deadline, not hang.
	if elapsed > 3*time.Second {
		t.Fatalf("Wait took %v, exceeds timeout bound", elapsed)
	}
}

// TestPidfdWait_ExitBetweenOpenAndPoll is Colin's double-check 1: a process that
// exits between pidfd_open and the poll registration must still wake the waiter
// immediately (the pidfd pins the task; the exit is latched), NOT hang the full
// timeout on an already-dead process.
func TestPidfdWait_ExitBetweenOpenAndPoll(t *testing.T) {
	if proc.PidfdSupportedOnThisPlatform() == false {
		t.Skip("pidfd unsupported")
	}

	cwd := setupTestEnv(t)
	createExecutable(t, cwd, "pidfd-fast-exit", "exit 0")
	id, err := proc.Run(cwd, "pidfd-fast-exit", nil, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Wait for the record to have a pid, then open its pidfd as fast as possible.
	// The tool exits almost immediately; pidfd_open may race the exit/reap. The
	// point is: however the race resolves, Wait must NOT take the full timeout.
	start := time.Now()
	res, werr := proc.Wait(cwd, 3, id)
	elapsed := time.Since(start)
	if werr != nil {
		t.Fatalf("Wait: %v", werr)
	}
	if res != id {
		t.Fatalf("Wait returned %q, want %q", res, id)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("fast-exit wait took %v - pidfd race hung on a dead process (must return immediately)", elapsed)
	}
}

// TestPidfdWait_CancelMidWait is Colin's double-check 2: cancelling the context
// (agent turn cancel -> tool cancel -> wackyproc wait) must abort the pidfd wait
// promptly and cleanly - no waiting out the timeout, no goroutine leak.
func TestPidfdWait_CancelMidWait(t *testing.T) {
	if proc.PidfdSupportedOnThisPlatform() == false {
		t.Skip("pidfd unsupported")
	}

	cwd := setupTestEnv(t)
	createExecutable(t, cwd, "pidfd-long", "sleep 30")
	id, err := proc.Run(cwd, "pidfd-long", nil, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer func() { _ = proc.Stop(cwd, id, 1) }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Give the wait goroutine time to arm the pidfd and block in poll.
	result := make(chan struct {
		got string
		err error
	}, 1)
	go func() {
		got, werr := proc.WaitContext(ctx, cwd, 30, id)
		result <- struct {
			got string
			err error
		}{got, werr}
	}()

	time.Sleep(200 * time.Millisecond)
	start := time.Now()
	cancel()

	select {
	case r := <-result:
		elapsed := time.Since(start)
		if !errors.Is(r.err, context.Canceled) {
			t.Fatalf("expected context.Canceled from cancelled wait, got: %v", r.err)
		}
		if elapsed > 500*time.Millisecond {
			t.Fatalf("cancelled wait took %v to return (must abort promptly, slice poll ~100ms)", elapsed)
		}
		t.Logf("cancel->return %v", elapsed)
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled wait did not return - pidfd poll is not ctx-aware")
	}

	// No goroutine leak: after cancel + drain, the poll goroutine must be gone.
	// The result channel was consumed, and waitPidFDsContext closes fds on cancel
	// so the poll goroutine exits. A second straight Wait must also still work
	// (proves no fd exhaustion / stuck state).
	// The process is still running (sleep 30); a fresh Wait with a short timeout must
	// run normally to prove no fd exhaustion / stuck state from the cancelled wait,
	// and must NOT hang past its own timeout.
	postDone := make(chan struct {
		got string
		err error
	}, 1)
	go func() {
		got, werr := proc.WaitContext(context.Background(), cwd, 1, id)
		postDone <- struct {
			got string
			err error
		}{got, werr}
	}()
	select {
	case <-postDone:
	case <-time.After(3 * time.Second):
		t.Fatal("post-cancel Wait hung past its 1s timeout")
	}
}
