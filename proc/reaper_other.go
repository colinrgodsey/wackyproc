//go:build !linux

package proc

// becomeSubreaper is a no-op on non-Linux: PR_SET_CHILD_SUBREAPER is Linux-only.
func becomeSubreaper() error {
	return nil
}

// reapAdoptedOrphans is a no-op on non-Linux.
func reapAdoptedOrphans() {}
