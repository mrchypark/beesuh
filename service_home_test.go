package beesuh

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrchypark/beesuh/model"
)

func TestServiceCodexCLIUsesOnlyResolvedHomes(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "launches")
	command := serviceCodexCommand(t, marker)
	homeA, homeB := t.TempDir(), t.TempDir()

	opts := Options{
		DataDir: t.TempDir(), BackendID: "service-codex-home", RequireIdentity: true, MaxConcurrent: 2, MaxWaiting: 2,
		Authorize: func(context.Context, Principal) error { return nil },
		Native: &NativeOptions{TokenBudgetPolicy: TokenBudgetAdvisory, DefaultProvider: "codex", Providers: map[string]model.Config{
			// Templates are intentionally unusable. Only the resolver controls CLI
			// login storage in service mode.
			"codex": {Kind: "codex-cli", Command: command, Model: "fixture", CodexHome: "relative-template"},
		}},
		ResolveCredential: func(_ context.Context, c Connection) (Credential, error) {
			home := homeA
			if c.ID == "b" {
				home = homeB
			}
			return Credential{CodexHome: home, AccountID: "account-" + c.ID}, nil
		},
	}
	for _, template := range []string{"", "relative-template", filepath.Join(t.TempDir(), "missing")} {
		candidate := opts
		candidate.DataDir = t.TempDir()
		native := *opts.Native
		providers := map[string]model.Config{}
		for id, cfg := range opts.Native.Providers {
			cfg.CodexHome = template
			providers[id] = cfg
		}
		native.Providers = providers
		candidate.Native = &native
		opened := nativeOpen(t, candidate)
		if err := opened.Close(); err != nil {
			t.Fatal(err)
		}
	}
	r := nativeOpen(t, opts)
	defer r.Close()
	for _, id := range []string{"a", "b"} {
		if err := r.RegisterConnection(context.Background(), Connection{ID: id, UserID: "user", WorkspaceID: "workspace", Kind: "provider", TargetID: "codex", CredentialRef: id, State: "ready"}); err != nil {
			t.Fatal(err)
		}
	}
	ctx := WithPrincipal(context.Background(), Principal{UserID: "user", WorkspaceID: "workspace"})
	errs := make(chan error, 2)
	for _, id := range []string{"a", "b"} {
		go func(id string) {
			out, err := r.Run(ctx, Request{WorkspaceID: "workspace", SessionKey: id, RequestID: id, ConnectionID: id, Prompt: Prompt{Text: "hello"}})
			if err == nil && string(out.Output) != fmt.Sprintf(`{"text":%q}`, map[string]string{"a": homeA, "b": homeB}[id]) {
				err = fmt.Errorf("connection %s used wrong CodexHome: %s", id, out.Output)
			}
			errs <- err
		}(id)
	}
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	launches, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	for _, home := range []string{homeA, homeB} {
		if strings.Count(string(launches), home+"\n") != 1 {
			t.Fatalf("launches %q do not isolate resolved home %q", launches, home)
		}
	}
	if got := opts.Native.Providers["codex"].CodexHome; got != "relative-template" {
		t.Fatalf("service constructor mutated template CodexHome: %q", got)
	}
	if got := r.native.opts.Providers["codex"].CodexHome; got != "" {
		t.Fatalf("service engine retained template CodexHome: %q", got)
	}
}

func TestServiceCodexCLIRejectsInvalidResolvedCredentialBeforeLaunch(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "launches")
	command := serviceCodexCommand(t, marker)
	validTemplate := t.TempDir()
	for _, tt := range []struct {
		name        string
		credential  Credential
		resolverErr error
	}{
		{name: "resolver failure", resolverErr: errors.New("credential store unavailable")},
		{name: "missing home", credential: Credential{AccountID: "account"}},
		{name: "relative home", credential: Credential{CodexHome: "relative", AccountID: "account"}},
		{name: "missing account", credential: Credential{CodexHome: t.TempDir()}},
		{name: "invalid account", credential: Credential{CodexHome: t.TempDir(), AccountID: "\xff"}},
		{name: "invalid UTF-8 home", credential: Credential{CodexHome: "/\xff", AccountID: "account"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			valid := Credential{CodexHome: t.TempDir(), AccountID: "account"}
			opts := Options{DataDir: t.TempDir(), BackendID: "invalid-resolved-home", RequireIdentity: true, MaxConcurrent: 1,
				Authorize: func(context.Context, Principal) error { return nil },
				Native:    &NativeOptions{TokenBudgetPolicy: TokenBudgetAdvisory, Provider: model.Config{Kind: "codex-cli", Command: command, Model: "fixture", CodexHome: validTemplate}},
				ResolveCredential: func(context.Context, Connection) (Credential, error) {
					calls++
					if calls == 1 {
						return valid, nil
					}
					return tt.credential, tt.resolverErr
				},
			}
			r := nativeOpen(t, opts)
			defer r.Close()
			if err := r.RegisterConnection(context.Background(), Connection{ID: "c", UserID: "user", WorkspaceID: "workspace", Kind: "provider", TargetID: "default", CredentialRef: "ref", State: "ready"}); err != nil {
				t.Fatal(err)
			}
			ctx := WithPrincipal(context.Background(), Principal{UserID: "user", WorkspaceID: "workspace"})
			if _, err := r.Run(ctx, Request{WorkspaceID: "workspace", SessionKey: "session", RequestID: "invalid", ConnectionID: "c", Prompt: Prompt{Text: "hello"}}); err == nil {
				t.Fatal("invalid resolved credential reached dispatch")
			}
		})
	}
	if raw, err := os.ReadFile(marker); err == nil && len(raw) != 0 {
		t.Fatalf("invalid resolved credentials launched Codex: %q", raw)
	} else if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func TestServiceCodexCLIRequiresResolver(t *testing.T) {
	_, err := Open(context.Background(), Options{DataDir: t.TempDir(), BackendID: "missing-resolver", RequireIdentity: true, MaxConcurrent: 1,
		Authorize: func(context.Context, Principal) error { return nil },
		Native:    &NativeOptions{TokenBudgetPolicy: TokenBudgetAdvisory, Provider: model.Config{Kind: "codex-cli", Model: "fixture", CodexHome: "ignored"}},
	})
	if err == nil || !strings.Contains(err.Error(), "credential resolvers") {
		t.Fatalf("missing resolver error = %v", err)
	}
}

func serviceCodexCommand(t *testing.T, marker string) string {
	t.Helper()
	command := filepath.Join(t.TempDir(), "codex")
	quotedMarker := "'" + strings.ReplaceAll(marker, "'", "'\"'\"'") + "'"
	script := "#!/bin/sh\n" +
		"out=''\n" +
		"while [ $# -gt 0 ]; do\n" +
		"  if [ \"$1\" = '--output-last-message' ]; then out=$2; shift 2; continue; fi\n" +
		"  shift\n" +
		"done\n" +
		"printf '%s\\n' \"$CODEX_HOME\" >> " + quotedMarker + "\n" +
		"printf '{\"content\":\"%s\",\"tool_calls\":[]}' \"$CODEX_HOME\" > \"$out\"\n"
	if err := os.WriteFile(command, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return command
}
