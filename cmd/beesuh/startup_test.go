package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStandaloneRejectsOperatorFallbacksBeforeOpeningData(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"missing config", nil, "-listen requires"},
		{"missing providers", []string{"-service-config", "missing.json"}, "-listen requires"},
		{"external backend", []string{"-backend", "opencode", "-service-config", "missing.json", "-providers", "missing.json"}, "-listen requires"},
		{"operator flag", []string{"-operator-http"}, "flag provided but not defined"},
		{"conductor credentials", []string{"-service-config", "missing.json", "-providers", "missing.json", "-conductor-tools"}, "-listen forbids operator Conductor"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			data := filepath.Join(dir, "data")
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			args := append([]string{"-test.run=^TestOAuthBinaryProcess$", "--", "-listen", "127.0.0.1:0", "-data-dir", data}, tc.args...)
			cmd := exec.CommandContext(ctx, os.Args[0], args...)
			cmd.Env = []string{"BEESUH_TEST_OAUTH_BINARY_PROCESS=1", "HOME=" + dir, "PATH=" + os.Getenv("PATH"), "BEESUH_API_TOKEN=operator", "BEESUH_MODEL_API_KEY=shared-key", "BEESUH_MODEL_URL=https://model.example/v1", "BEESUH_MODEL=test", "CONDUCTOR_TOKEN=operator", "CONDUCTOR_EVENT_BINDINGS=invalid-json"}
			out, err := cmd.CombinedOutput()
			if ctx.Err() != nil || err == nil || !strings.Contains(string(out), tc.want) {
				t.Fatalf("startup: %v, context: %v, output: %s", err, ctx.Err(), out)
			}
			if _, err = os.Stat(data); !os.IsNotExist(err) {
				t.Fatalf("opened data before rejecting config: %v", err)
			}
		})
	}
}
