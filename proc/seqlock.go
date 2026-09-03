package proc

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	SeqFileName        = ".seq"
	SeqLockDirName     = ".seq.lock"
	seqLockTimeout     = 3 * time.Second
	seqLockStaleAge    = 10 * time.Second
	seqLockPollBackoff = 10 * time.Millisecond
)

var seqLockMu sync.Mutex

// IsProcessRecordDir returns true if name is exactly IDLength (4) characters drawn from [0-9a-z].
// Non-record directories such as .seq.lock, temporary files, or dotfiles return false.
func IsProcessRecordDir(name string) bool {
	if len(name) != IDLength {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'z')) {
			return false
		}
	}
	return true
}

// acquireSeqLock acquires a directory lock on .proc/.seq.lock using os.Mkdir/EEXIST.
// Retries with backoff until seqLockTimeout. If the timeout expires, it inspects the lock
// directory's mtime and recovers by removing it if it is older than seqLockStaleAge (~10s).
//
// Honest note on the bounded stale-steal race:
// If a process holding the lock was frozen/suspended (e.g. via SIGSTOP or VM pause) for
// more than 10 seconds, another process may steal the lock by removing the directory and
// proceeding. In that rare event, two processes could theoretically increment the counter
// concurrently; the risk is bounded: a rare Gen/ConsumedSeq tie (two lock holders), not
// corruption or a crash. The critical section only reads and writes a single small counter file
// and executes in a few microseconds, so a 10s wait indicates a crashed or abandoned lock.
// This bounded recovery is preferable to permanently deadlocking all subsequent process
// spawns and gets.
func acquireSeqLock(procBaseDir string) (func(), error) {
	lockDir := filepath.Join(procBaseDir, SeqLockDirName)
	deadline := time.Now().Add(seqLockTimeout)
	staleRecovered := false

	for {
		err := os.Mkdir(lockDir, 0755)
		if err == nil {
			return func() {
				_ = os.Remove(lockDir)
			}, nil
		}
		if !os.IsExist(err) {
			return nil, fmt.Errorf("failed to create sequence lock directory %s: %w", lockDir, err)
		}

		if time.Now().After(deadline) {
			if !staleRecovered {
				fi, statErr := os.Stat(lockDir)
				if statErr == nil && time.Since(fi.ModTime()) > seqLockStaleAge {
					_ = os.Remove(lockDir)
					staleRecovered = true
					continue
				}
			}
			return nil, fmt.Errorf("timed out waiting to acquire sequence lock %s", lockDir)
		}

		time.Sleep(seqLockPollBackoff)
	}
}

// nextSeq atomically reads, increments, and persists the monotonic sequence counter under procBaseDir.
// It is used to assign Meta.Gen on process spawn and Meta.ConsumedSeq on first terminal read.
func nextSeq(procBaseDir string) (uint64, error) {
	seqLockMu.Lock()
	defer seqLockMu.Unlock()

	unlock, err := acquireSeqLock(procBaseDir)
	if err != nil {
		return 0, err
	}
	defer unlock()

	seqPath := filepath.Join(procBaseDir, SeqFileName)
	var current uint64
	data, err := os.ReadFile(seqPath)
	if err == nil {
		parsed, parseErr := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
		if parseErr == nil {
			current = parsed
		}
	} else if !os.IsNotExist(err) {
		return 0, fmt.Errorf("failed to read %s: %w", SeqFileName, err)
	}

	next := current + 1
	tmpPath := filepath.Join(procBaseDir, fmt.Sprintf(".seq.tmp.%d", time.Now().UnixNano()))
	if err := os.WriteFile(tmpPath, []byte(fmt.Sprintf("%d\n", next)), 0644); err != nil {
		return 0, fmt.Errorf("failed to write temporary sequence file: %w", err)
	}
	if err := os.Rename(tmpPath, seqPath); err != nil {
		_ = os.Remove(tmpPath)
		return 0, fmt.Errorf("failed to rename sequence file: %w", err)
	}

	return next, nil
}

// NextSeq returns the next monotonic sequence number for the process directory.
// Exported for testing and package consumers.
func NextSeq(procBaseDir string) (uint64, error) {
	return nextSeq(procBaseDir)
}
