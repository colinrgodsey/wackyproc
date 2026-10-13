//go:build linux

package proc

import (
	"context"
	"errors"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// ErrPidFDUnsupported reports that pidfd polling is unavailable (non-Linux, kernel
// before 5.3, or a sandbox denying pidfd_open). Wait falls back to the polling ramp.
var ErrPidFDUnsupported = errors.New("pidfd polling unsupported")

// ErrPidFDProcessGone reports pidfd_open ESRCH: the process exited and was reaped
// before we armed it. The caller should re-list immediately - the answer is likely
// already terminal.
var ErrPidFDProcessGone = errors.New("pidfd process gone")

// openPidFD opens a pidfd for pid (linux-only). The kernel wakes pollers when the
// process exits, giving event-driven wait with zero polling CPU.
func openPidFD(pid int) (int, error) {
	if pid <= 0 {
		return -1, ErrPidFDUnsupported
	}
	fd, err := unix.PidfdOpen(pid, 0)
	if errors.Is(err, unix.ESRCH) {
		return -1, ErrPidFDProcessGone
	}
	if err != nil {
		return -1, err
	}
	return fd, nil
}

// pidfdPoll blocks until one of fds becomes ready (process exit, POLLIN) or timeoutMs
// elapses. Returns (n, nil) with n=0 on timeout; (n, err) on poll failure.
func pidfdPoll(fds []int, timeoutMs int) (int, error) {
	if len(fds) == 0 {
		return 0, nil
	}
	pollFds := make([]unix.PollFd, len(fds))
	for i, fd := range fds {
		pollFds[i] = unix.PollFd{Fd: int32(fd), Events: unix.POLLIN}
	}
	for {
		_, err := unix.Poll(pollFds, timeoutMs)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return 0, err
		}
		// Count ready (POLLIN or error events) fds.
		ready := 0
		for _, pf := range pollFds {
			if pf.Revents != 0 {
				ready++
			}
		}
		return ready, nil
	}
}

// waitPidFDsContext waits on pidfds for the pids while honoring ctx cancellation.
// It polls in bounded slices so a cancel is observed within one slice (100ms) without
// relying on close()-to-unblock-poll, which is unreliable on Linux while poll is
// blocked. Each slice returns immediately if any pidfd fired (process exited); between
// slices we check ctx.Done(). A process that exits between pidfd_open and the first
// poll still wakes the next slice - the pidfd pins the exited task, so the exit is
// latched (Colin double-check 1).
func waitPidFDsContext(ctx context.Context, pids []int, timeout time.Duration) (bool, []int, error) {
	var fds []int
	for _, pid := range pids {
		fd, err := openPidFD(pid)
		if err != nil {
			closePidFDs(fds)
			return false, nil, err
		}
		fds = append(fds, fd)
	}
	defer closePidFDs(fds)
	if len(fds) == 0 {
		return false, nil, ErrPidFDUnsupported
	}

	deadline := time.Now().Add(timeout)
	const sliceMs = 100
	for {
		if timeout <= 0 {
			return false, nil, nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false, nil, nil
		}
		slice := int(remaining / time.Millisecond)
		if slice > sliceMs {
			slice = sliceMs
		}
		n, err := pidfdPoll(fds, slice)
		if err != nil {
			return false, nil, err
		}
		if n > 0 {
			return true, nil, nil
		}
		select {
		case <-ctx.Done():
			return false, nil, ctx.Err()
		default:
		}
	}
}

func closePidFDs(fds []int) {
	for _, fd := range fds {
		_ = unix.Close(fd)
	}
}

// PidfdSupportedOnThisPlatform reports whether pidfd polling is available: on Linux
// it probes an actual pidfd_open (Kernel >= 5.3, not sandbox-denied); elsewhere false.
func PidfdSupportedOnThisPlatform() bool {
	fd, err := openPidFD(os.Getpid())
	if err != nil {
		return false
	}
	closePidFDs([]int{fd})
	return true
}
