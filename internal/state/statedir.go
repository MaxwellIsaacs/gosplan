package state

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DefaultDir returns the default state dir for a project:
// $XDG_STATE_HOME/gosplan/<basename>-<first 12 hex of sha256(abs path)>,
// falling back to $HOME/.local/state when XDG_STATE_HOME is unset.
// A relative XDG_STATE_HOME is ignored, as the XDG spec requires. It is an
// error for the result to fall inside the project tree.
func DefaultDir(project string) (string, error) {
	abs, err := filepath.Abs(project)
	if err != nil {
		return "", fmt.Errorf("state: default dir: %w", err)
	}
	root := os.Getenv("XDG_STATE_HOME")
	if root == "" || !filepath.IsAbs(root) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("state: default dir: %w", err)
		}
		root = filepath.Join(home, ".local", "state")
	}
	sum := sha256.Sum256([]byte(abs))
	dir := filepath.Join(root, "gosplan", filepath.Base(abs)+"-"+hex.EncodeToString(sum[:])[:12])
	if rel, err := filepath.Rel(abs, dir); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("state: default dir %s is inside project %s; pass --state-dir", dir, abs)
	}
	return dir, nil
}
