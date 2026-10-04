package beesuh

import (
	"strings"
	"testing"

	"github.com/mrchypark/beesuh/model"
)

func TestNativeTokenBudgetPolicy(t *testing.T) {
	cfg := model.Config{Kind: "codex-app-server", Model: "test"}
	if _, err := newNative(NativeOptions{Provider: cfg, TokenBudgetPolicy: "invalid"}); err == nil {
		t.Fatal("invalid token budget policy was accepted")
	}
	if _, err := newNative(NativeOptions{Provider: cfg}); err == nil || !strings.Contains(err.Error(), `provider "default" cannot enforce a token limit`) || !strings.Contains(err.Error(), "TokenBudgetPolicy") {
		t.Fatalf("strict policy error = %v", err)
	}
	for _, policy := range []TokenBudgetPolicy{TokenBudgetAdvisory, TokenBudgetUnlimited} {
		n, err := newNative(NativeOptions{Provider: cfg, TokenBudgetPolicy: policy})
		if err != nil {
			t.Fatalf("%s policy: %v", policy, err)
		}
		max, advisory := n.tokenBudget("default")
		if policy == TokenBudgetAdvisory && (max != 2048 || !advisory) {
			t.Fatalf("advisory budget = %d, %t", max, advisory)
		}
		if policy == TokenBudgetUnlimited && (max != 0 || advisory) {
			t.Fatalf("unlimited budget = %d, %t", max, advisory)
		}
	}
}
