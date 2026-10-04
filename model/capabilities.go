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
