package proc

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/colinrgodsey/wackypub/pkg/slug"
)

// GenerateRandomID generates an 8-character pronounceable slug ID (4 CVCV
// syllables, 24 bits of entropy via crypto/rand) using the shared scheme from
// wackypub's pkg/slug - a single source of truth for ID generation across the
// tool suite (Colin 2026-09-29).
func GenerateRandomID() string {
	return slug.New()
}

// ClaimUniqueProcessDir generates a unique 4-character ID and atomically creates its directory
// via os.Mkdir (which fails with os.ErrExist if the directory already exists).
// Retries on collision up to MaxIDGenerationRetries times.
func ClaimUniqueProcessDir(procBaseDir string) (string, string, error) {
	for i := 0; i < MaxIDGenerationRetries; i++ {
		candidate := GenerateRandomID()
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
