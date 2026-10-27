//go:build unix

package proc

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// SessionLockBusy reports whether another process holds the owning agent's session lock.
// wackypub serializes turns with an exclusive flock on <agentDir>/session.lock, so a
// refused non-blocking acquisition means a turn is in flight. The probe takes the lock only
// to release it again, and creates the file when absent: an agent that has never written a
// session is idle by definition.
func SessionLockBusy(agentDir string) (bool, error) {
	if agentDir == "" {
		return true, errNoAgentDir
	}
	f, err := os.OpenFile(filepath.Join(agentDir, sessionLockFileName), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return true, err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return true, nil
		}
		return true, err
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false, nil
}

// detachProcess puts a hook script in its own session so it survives the supervisor, which
// exits moments after firing it.
func detachProcess(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
}
