package model

import (
	"strings"
	"testing"
)

func TestCapabilities(t *testing.T) {
	tests := []struct {
		name string
		kind string
		want Capabilities
	}{
		{"cli", "codex-cli", Capabilities{TokenLimit: TokenLimitRequested, Usage: UsageUnknown}},
		{"app server", "codex-app-server", Capabilities{TokenLimit: TokenLimitRequested, Usage: UsageUnknown}},
		{"api", "openai-compatible", Capabilities{TokenLimit: TokenLimitEnforced, Usage: UsageAvailable}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := (&Client{kind: tt.kind}).Capabilities(); got != tt.want {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestUsageAvailability(t *testing.T) {
	if unknownUsage().Available() {
		t.Fatal("unknown usage is available")
	}
	if !(Usage{}).Available() {
		t.Fatal("zero measured usage is unavailable")
	}
}

func TestCodexPromptKeepsAdvisoryBudgetHint(t *testing.T) {
	prompt, err := codexPrompt(Request{Messages: []Message{{Role: "user", Content: "hi"}}, MaxTokens: 12})
	if err != nil || !strings.Contains(prompt, `"max_tokens_hint":12`) {
		t.Fatalf("prompt=%q err=%v", prompt, err)
	}
}
