package beesuh

import (
	"context"
	"errors"
	"testing"

	"github.com/mrchypark/beesuh/model"
)

func TestConnectionCapacityIncludesRevokedRowsBeforeCredentialResolution(t *testing.T) {
	ctx := context.Background()
	resolutions := 0
	opts := Options{DataDir: t.TempDir(), BackendID: "connection-capacity", MaxConcurrent: 1,
		RequireIdentity: true, Authorize: func(context.Context, Principal) error { return nil },
		ResolveCredential: func(context.Context, Connection) (Credential, error) {
			resolutions++
			return Credential{AccountID: "account", APIKey: "fixture"}, nil
		},
		Native:   &NativeOptions{Provider: model.Config{BaseURL: "http://provider.invalid", Model: "fixture"}},
		Capacity: CapacityLimits{ConnectionsPerWorkspace: 1, ConnectionsGlobal: 1},
	}
	r := nativeOpen(t, opts)
	c := Connection{ID: "revoked", UserID: "user", WorkspaceID: "workspace", Kind: "provider", TargetID: "default", CredentialRef: "ref", State: "revoked"}
	if err := r.RegisterConnection(ctx, c); err != nil {
		t.Fatal(err)
	}
	if err := r.RegisterConnection(ctx, c); err != nil {
		t.Fatalf("idempotent registration at capacity: %v", err)
	}
	newConnection := c
	newConnection.ID, newConnection.WorkspaceID, newConnection.State = "new", "other-workspace", "ready"
	if err := r.RegisterConnection(ctx, newConnection); !errors.Is(err, ErrCapacity) {
		t.Fatalf("global capacity = %v", err)
	}
	if resolutions != 0 {
		t.Fatalf("denied connection resolved credentials %d times", resolutions)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r = nativeOpen(t, opts)
	t.Cleanup(func() { _ = r.Close() })
	if err := r.RegisterConnection(ctx, newConnection); !errors.Is(err, ErrCapacity) {
		t.Fatalf("reopened capacity = %v", err)
	}
	principal := WithPrincipal(ctx, Principal{UserID: c.UserID, WorkspaceID: c.WorkspaceID})
	connections, err := r.Connections(principal)
	if err != nil || len(connections) != 1 || connections[0].State != "revoked" || connections[0].Revocation != "confirmed" {
		t.Fatalf("retained connection=%+v error=%v", connections, err)
	}
}
