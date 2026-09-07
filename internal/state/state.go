// Package state persists the last-built upstream release so the watcher
// ("waits") can decide whether a new build is needed.
package state

import (
	"os"
	"path/filepath"
	"strings"
)

// State is the last release we converted.
type State struct {
	Version string
	BuildID string
}

// Load reads VERSION and BUILD_ID files from dir. Missing files yield an
// empty State and no error.
func Load(dir string) (State, error) {
	var s State
	if b, err := os.ReadFile(filepath.Join(dir, "VERSION")); err == nil {
		s.Version = strings.TrimSpace(string(b))
	} else if !os.IsNotExist(err) {
		return s, err
	}
	if b, err := os.ReadFile(filepath.Join(dir, "BUILD_ID")); err == nil {
		s.BuildID = strings.TrimSpace(string(b))
	} else if !os.IsNotExist(err) {
		return s, err
	}
	return s, nil
}

// Save writes VERSION and BUILD_ID files to dir.
func Save(dir string, s State) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "VERSION"), []byte(s.Version+"\n"), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "BUILD_ID"), []byte(s.BuildID+"\n"), 0o644)
}

// IsNew reports whether rel differs from the recorded state.
func (s State) IsNew(version, buildID string) bool {
	if version == "" {
		return false
	}
	if s.Version == "" {
		return true
	}
	return version != s.Version || (buildID != "" && s.BuildID != "" && buildID != s.BuildID)
}
