//go:build linux

package proc

import (
	"syscall"

	"golang.org/x/sys/unix"
	"time"
)

// becomeSubreaper marks THIS process as a child subreaper (PR_SET_CHILD_SUBREAPER):
// any descendant whose parent dies becomes a direct child of THIS process instead of
// being sent to init/PID 1. The wackyproc __supervise process is orphaned itself (the
// run CLI that spawned it exits immediately), so without this flag its own tool's
// grandchildren - e.g. the `bash -c wackypub agent ... prompt` wrapper and the
// wackypub process it spawns - would reparent to PID 1 on their parents' death. On a
// container whose PID 1 is plain sh (docker-entrypoint.sh), PID 1 does not reap and
// every one of those becomes a defunct process (the accumulation in
// bugs/wackyproc/defunct-process-accumulation). Becoming the subreaper contains
// them: they reparent here, and reapAdoptedOrphans() below reaps them.
func becomeSubreaper() error {
	return unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0)
}

// reapAdoptedOrphans reaps the children adopted via subreaper status (tool
// grandchildren whose parents died). Called after cmd.Wait(): the direct tool child
// is already reaped, so wait4(-1) here only touches adopted orphans. They exit at
// their own pace - a killed bash wrapper might leave a still-running wackypub
// grandchild - so the supervisor LINGERS until every adopted child is gone (or a
// bounded grace period passes), reaping each as it finishes. Without the linger, a
// still-running orphan would be handed to init/PID 1 at supervisor exit, and a
// container whose PID 1 does not reap would accumulate it as defunct (the exact
// accumulation in bugs/wackyproc/defunct-process-accumulation). The grace bound
// means a genuinely runaway orphan is not blocked on forever: it is passed to init
// once, which a reaping init handles and a tini-less container does not - the
// supervisor's own reaping is in-repo, init reaping is a container posture.
const adoptedReapGrace = 30 * time.Second

func reapAdoptedOrphans() {
	grace := time.Now().Add(adoptedReapGrace)
	for {
		var ws syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
		if err == syscall.ECHILD {
			return
		}
		if pid <= 0 {
			// No child finished in this pass; if any adopted children are still
			// running they have up to the grace deadline, then we give up and hand
			// them to init.
			if time.Now().After(grace) {
				return
			}
			time.Sleep(50 * time.Millisecond)
			continue
		}
		// Reaped one; extend nothing - the grace is from the first check.
	}
}
