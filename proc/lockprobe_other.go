//go:build !unix

package proc

import (
	"os/exec"
)

// SessionLockBusy cannot probe an advisory session lock portably, so every wake on this
// platform is treated as busy. That degrades to the documented skip: the job stays
// un-notified and a manual get/wait read still surfaces it. A wake that cannot prove the
// agent is idle is not worth a double turn.
func SessionLockBusy(agentDir string) (bool, error) {
	if agentDir == "" {
		return true, errNoAgentDir
	}
	return true, nil
}

// detachProcess is a no-op where setsid does not exist.
func detachProcess(cmd *exec.Cmd) {}
