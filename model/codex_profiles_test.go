package model

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestManagedCodexProfilesCannotShareAuthHome(t *testing.T) {
	home := t.TempDir()
	file := filepath.Join(t.TempDir(), "providers.json")
	config := map[string]any{"default": "personal", "providers": []map[string]any{
		{"id": "personal", "kind": "codex-cli", "model": "fixture", "codex_home": home},
		{"id": "work", "kind": "codex-cli", "model": "fixture", "codex_home": filepath.Join(home, ".")},
	}}
	data, _ := json.Marshal(config)
	if err := os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadProviders(file); err == nil {
		t.Fatal("two profiles accepted the same login home")
	}
	config["providers"].([]map[string]any)[1]["codex_home"] = t.TempDir()
	data, _ = json.Marshal(config)
	if err := os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
	if profiles, _, err := LoadProviders(file); err != nil || len(profiles) != 2 {
		t.Fatalf("distinct profiles rejected: %v", err)
	}
}
