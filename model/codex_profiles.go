package model

import (
	"errors"
	"path/filepath"
)

// Configured managed profiles must not alias the same account store. External
// profiles have no home; each app-server completion gets an isolated process.
func validateCodexProfileHomes(providers map[string]Config) error {
	homes := map[string]bool{}
	for _, cfg := range providers {
		if cfg.Kind != "codex-cli" || cfg.CodexHome == "" {
			continue
		}
		home := filepath.Clean(cfg.CodexHome)
		if resolved, err := filepath.EvalSymlinks(home); err == nil {
			home = resolved
		}
		if homes[home] {
			return errors.New("model: Codex profiles must use distinct homes")
		}
		homes[home] = true
	}
	return nil
}
