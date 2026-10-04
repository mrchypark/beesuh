package model

import (
	"context"
	"errors"
)

// TokenLimit reports how an adapter handles a requested token limit.
type TokenLimit string

const (
	TokenLimitEnforced    TokenLimit = "enforced"
	TokenLimitRequested   TokenLimit = "requested"
	TokenLimitUnavailable TokenLimit = "unavailable"
)

// UsageAvailability reports whether an adapter returns measured token usage.
type UsageAvailability string

const (
	UsageAvailable UsageAvailability = "available"
	UsageUnknown   UsageAvailability = "unknown"
)

type Capabilities struct {
	TokenLimit TokenLimit        `json:"token_limit"`
	Usage      UsageAvailability `json:"usage"`
}

// ConfigCapabilities reports capabilities without starting a provider process.
func ConfigCapabilities(cfg Config) Capabilities {
	if cfg.Kind == "codex-cli" || cfg.Kind == "codex-app-server" {
		return Capabilities{TokenLimit: TokenLimitRequested, Usage: UsageUnknown}
	}
	return Capabilities{TokenLimit: TokenLimitEnforced, Usage: UsageAvailable}
}

// ErrTokenLimitNotEnforced rejects a request that needs a hard token ceiling.
var ErrTokenLimitNotEnforced = errors.New("model: token limit is not enforced")

type advisoryTokenBudgetKey struct{}

// AllowAdvisoryTokenBudget permits an adapter to pass MaxTokens as a hint.
func AllowAdvisoryTokenBudget(ctx context.Context) context.Context {
	return context.WithValue(ctx, advisoryTokenBudgetKey{}, struct{}{})
}

func advisoryTokenBudgetAllowed(ctx context.Context) bool {
	_, ok := ctx.Value(advisoryTokenBudgetKey{}).(struct{})
	return ok
}

func (c *Client) Capabilities() Capabilities {
	return ConfigCapabilities(Config{Kind: c.kind})
}

func (c *Client) validateTokenLimit(ctx context.Context, in Request) error {
	if in.MaxTokens == 0 || c.Capabilities().TokenLimit == TokenLimitEnforced || advisoryTokenBudgetAllowed(ctx) {
		return nil
	}
	return ErrTokenLimitNotEnforced
}

// Available reports whether token counts are measured rather than unknown.
func (u Usage) Available() bool {
	return u.PromptTokens >= 0 && u.CompletionTokens >= 0 && u.TotalTokens >= 0
}

func unknownUsage() Usage {
	return Usage{PromptTokens: -1, CompletionTokens: -1, TotalTokens: -1}
}

// measuredUsage converts a complete, non-negative provider usage record. A
// missing, partial, or invalid record is unknown rather than a false zero.
func measuredUsage(prompt, completion, total *int) Usage {
	if prompt == nil || completion == nil || total == nil || *prompt < 0 || *completion < 0 || *total < 0 {
		return unknownUsage()
	}
	return Usage{PromptTokens: *prompt, CompletionTokens: *completion, TotalTokens: *total}
}

func sumUsage(values ...int) (int, bool) {
	max := int(^uint(0) >> 1)
	total := 0
	for _, value := range values {
		if value < 0 || value > max-total {
			return 0, false
		}
		total += value
	}
	return total, true
}
