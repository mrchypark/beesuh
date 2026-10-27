package model

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
)

var providerID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)
var envName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

func ValidProviderID(id string) bool { return providerID.MatchString(id) }

// LoadProviders reads named connections. Secrets are environment references,
// never literal config fields. Repeated kinds are allowed; IDs must be unique.
func LoadProviders(path string) (map[string]Config, string, error) { return loadProviders(path, false) }

// LoadServiceProviders rejects shared credentials before reading environment values.
func LoadServiceProviders(path string) (map[string]Config, string, error) {
	return loadProviders(path, true)
}

func loadProviders(path string, service bool) (map[string]Config, string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, "", err
	}
	defer file.Close()
	b, err := io.ReadAll(io.LimitReader(file, (64<<10)+1))
	if err != nil {
		return nil, "", err
	}
	if len(b) > 64<<10 {
		return nil, "", errors.New("model: provider file exceeds 64 KiB")
	}
	var config struct {
		Default   string `json:"default"`
		Providers []struct {
			ID string `json:"id"`
			Config
		} `json:"providers"`
	}
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return nil, "", errors.New("model: invalid provider config JSON")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, "", errors.New("model: provider config must contain one JSON value")
	}
	if len(config.Providers) == 0 || len(config.Providers) > 64 {
		return nil, "", errors.New("model: configure 1–64 providers")
	}
	providers := make(map[string]Config, len(config.Providers))
	for _, entry := range config.Providers {
		if !ValidProviderID(entry.ID) {
			return nil, "", errors.New("model: invalid provider ID")
		}
		if _, ok := providers[entry.ID]; ok {
			return nil, "", fmt.Errorf("model: duplicate provider ID %q", entry.ID)
		}
		cfg := entry.Config
		if service {
			if cfg.APIKeyEnv != "" || cfg.APIKey != "" || cfg.Command != "" || cfg.CodexHome != "" || cfg.CodexAuth != nil || cfg.CodexAccountID != "" {
				return nil, "", errors.New("model: service providers forbid shared credentials and login stores")
			}
			if (cfg.Kind != "" && cfg.Kind != "openai-compatible" && cfg.Kind != "opencode-go" && cfg.Kind != "opencode-zen") || (cfg.API != "" && cfg.API != "chat-completions" && cfg.API != "responses") {
				return nil, "", errors.New("model: service providers require Bearer-compatible HTTP providers")
			}
		}
		if cfg.APIKeyEnv != "" {
			if !envName.MatchString(cfg.APIKeyEnv) {
				return nil, "", errors.New("model: invalid credential environment reference")
			}
			cfg.APIKey = os.Getenv(cfg.APIKeyEnv)
			if cfg.APIKey == "" {
				return nil, "", fmt.Errorf("model: credential environment variable is empty for provider %q", entry.ID)
			}
		}
		if _, err := New(cfg); err != nil {
			return nil, "", fmt.Errorf("model: provider %q: %w", entry.ID, err)
		}
		providers[entry.ID] = cfg
	}
	if err := ValidateCodexProfileHomes(providers); err != nil {
		return nil, "", err
	}
	if config.Default == "" && len(providers) == 1 {
		for id := range providers {
			config.Default = id
		}
	}
	if _, ok := providers[config.Default]; !ok {
		return nil, "", errors.New("model: default must name a configured provider")
	}
	return providers, config.Default, nil
}
