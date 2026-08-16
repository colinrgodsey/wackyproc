package proc

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const charset = "0123456789abcdefghijklmnopqrstuvwxyz"

// GenerateRandomID generates a random 4-character alphanumeric string.
func GenerateRandomID() string {
	var result strings.Builder
	for i := 0; i < IDLength; i++ {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(charset))))
		if err != nil {
			result.WriteByte(charset[time.Now().UnixNano()%int64(len(charset))])
			continue
		}
		result.WriteByte(charset[n.Int64()])
	}
	return result.String()
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
