package proc

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/colinrgodsey/wackypub/pkg/slug"
)

// GenerateRandomID generates an 8-character pronounceable slug ID (4 CVCV
// syllables, 24 bits of entropy via crypto/rand) using the shared scheme from
// wackypub's pkg/slug - a single source of truth for ID generation across the
// tool suite (Colin 2026-09-29).
func GenerateRandomID() string {
	return slug.New()
}

// DisposedIDsFileName is a per-workspace list of process IDs that have been disposed
// (pruned, evicted, or force-removed). Claiming skips them so a fresh dispatch never
// inherits an ID that operator or agent logs still reference from the previous record.
const DisposedIDsFileName = "disposed_ids"

// MaxDisposedIDEntries caps the disposed-ID list; the oldest entries fall off first when it
// grows past the cap. 8-byte IDs + 1 byte of newline keep even the full cap well under 32KB.
const MaxDisposedIDEntries = 2048

// loadDisposedIDs reads the disposed-ID list. A missing or unreadable file yields an
// empty set: the list is a best-effort reuse guard, never a hard dependency.
func loadDisposedIDs(procBaseDir string) map[string]bool {
	ids := make(map[string]bool)
	data, err := os.ReadFile(filepath.Join(procBaseDir, DisposedIDsFileName))
	if err != nil {
		return ids
	}
	for _, line := range strings.Split(string(data), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			ids[line] = true
		}
	}
	return ids
}

// recordDisposedID appends an ID to the disposed list (deduped, capped, written
// tmp+rename). Best-effort: a failure here never fails the disposal that triggered it.
func recordDisposedID(procBaseDir, id string) {
	path := filepath.Join(procBaseDir, DisposedIDsFileName)
	var lines []string
	if data, err := os.ReadFile(path); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if line = strings.TrimSpace(line); line != "" && line != id {
				lines = append(lines, line)
			}
		}
	}
	lines = append(lines, id)
	if len(lines) > MaxDisposedIDEntries {
		lines = lines[len(lines)-MaxDisposedIDEntries:]
	}
	tmpPath := path + fmt.Sprintf(".tmp.%d", time.Now().UnixNano())
	if err := os.WriteFile(tmpPath, []byte(strings.Join(lines, "\n")+"\n"), 0644); err != nil {
		return
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
	}
}

// ClaimUniqueProcessDir generates a unique ID and atomically creates its directory
// via os.Mkdir (which fails with os.ErrExist if the directory already exists).
// Candidates on the disposed list are never re-claimed. Retries on collision up to
// MaxIDGenerationRetries times.
func ClaimUniqueProcessDir(procBaseDir string) (string, string, error) {
	return claimUniqueProcessDir(procBaseDir, GenerateRandomID)
}

func claimUniqueProcessDir(procBaseDir string, nextID func() string) (string, string, error) {
	disposed := loadDisposedIDs(procBaseDir)
	for i := 0; i < MaxIDGenerationRetries; i++ {
		candidate := nextID()
		if disposed[candidate] {
			continue
		}
		dirPath := filepath.Join(procBaseDir, candidate)
		err := os.Mkdir(dirPath, 0755)
		if err == nil {
			return candidate, dirPath, nil
		}
		if !os.IsExist(err) {
			return "", "", fmt.Errorf("failed to create process directory: %w", err)
		}
	}
	return "", "", fmt.Errorf("failed to generate unique process ID after %d retries", MaxIDGenerationRetries)
}
