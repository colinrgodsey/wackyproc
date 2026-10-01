//go:build !linux

package proc

import (
	"context"
	"errors"
	"time"
)

// ErrPidFDUnsupported reports that pidfd polling is unavailable (non-Linux, kernel
// before 5.3, or a sandbox denying pidfd_open). Wait falls back to the polling ramp.
var ErrPidFDUnsupported = errors.New("pidfd polling unsupported")

var ErrPidFDProcessGone = errors.New("pidfd process gone")

// openPidFD is a no-op on non-Linux.
func openPidFD(pid int) (int, error) {
	return -1, ErrPidFDUnsupported
}

// waitPidFDs is a no-op on non-Linux.
func waitPidFDs(pids []int, timeout time.Duration) (bool, []int, error) {
	return false, nil, ErrPidFDUnsupported
}

func closePidFDs(fds []int) {}

// PidfdSupportedOnThisPlatform is false on non-Linux.
func PidfdSupportedOnThisPlatform() bool {
	return false
}

// waitPidFDsContext is the non-Linux stub: always unsupported.
func waitPidFDsContext(ctx context.Context, pids []int, timeout time.Duration) (bool, []int, error) {
	return false, nil, ErrPidFDUnsupported
}
