package beesuh

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestConnectionRevocationStatus(t *testing.T) {
	tests := []struct {
		name       string
		revoke     func(context.Context, Connection) error
		wantStatus string
		wantErr    error
	}{
		{name: "unsupported", wantStatus: "unavailable"},
		{name: "failed", revoke: func(context.Context, Connection) error { return errors.New("host failed") }, wantStatus: "pending", wantErr: ErrRevocationPending},
		{name: "confirmed", revoke: func(context.Context, Connection) error { return nil }, wantStatus: "confirmed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := connectionRuntime(t, t.TempDir(), func(context.Context, Principal) error { return nil })
			r.opts.RevokeCredential = tt.revoke
			ctx := principal(context.Background(), "u", "w")
			if err := r.RegisterConnection(context.Background(), Connection{ID: "c", UserID: "u", WorkspaceID: "w", Kind: "provider", TargetID: "work", CredentialRef: "ref", State: "ready"}); err != nil {
				t.Fatal(err)
			}
			err := r.SetConnectionState(ctx, "c", "revoked")
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("revoke error = %v, want %v", err, tt.wantErr)
			}
			connections, err := r.Connections(ctx)
			if err != nil || len(connections) != 1 {
				t.Fatalf("Connections() = %#v, %v", connections, err)
			}
			if got := connections[0]; got.State != "revoked" || got.Generation != 2 || got.Revocation != tt.wantStatus {
				t.Fatalf("connection = %#v", got)
			}
		})
	}
}

func TestConnectionRevocationRetriesAfterRestart(t *testing.T) {
	dir := t.TempDir()
	r := connectionRuntime(t, dir, func(context.Context, Principal) error { return nil })
	ctx := principal(context.Background(), "u", "w")
	if err := r.RegisterConnection(context.Background(), Connection{ID: "c", UserID: "u", WorkspaceID: "w", Kind: "provider", TargetID: "work", CredentialRef: "ref", State: "ready"}); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	r.opts.RevokeCredential = func(context.Context, Connection) error {
		if calls.Add(1) == 1 {
			return errors.New("temporary failure")
		}
		return nil
	}
	if err := r.SetConnectionState(ctx, "c", "revoked"); !errors.Is(err, ErrRevocationPending) {
		t.Fatalf("first revoke = %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r = connectionRuntime(t, dir, func(context.Context, Principal) error { return nil })
	r.opts.RevokeCredential = func(context.Context, Connection) error {
		calls.Add(1)
		return nil
	}
	if err := r.SetConnectionState(ctx, "c", "revoked"); err != nil {
		t.Fatalf("retry revoke = %v", err)
	}
	connections, err := r.Connections(ctx)
	if err != nil || len(connections) != 1 || connections[0].Revocation != "confirmed" {
		t.Fatalf("Connections() = %#v, %v", connections, err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("callback calls = %d, want 2", got)
	}
}

func TestConnectionRevocationReleasesConnectionLock(t *testing.T) {
	r := connectionRuntime(t, t.TempDir(), func(context.Context, Principal) error { return nil })
	ctx := principal(context.Background(), "u", "w")
	if err := r.RegisterConnection(context.Background(), Connection{ID: "c", UserID: "u", WorkspaceID: "w", Kind: "provider", TargetID: "work", CredentialRef: "ref", State: "ready"}); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	r.opts.RevokeCredential = func(_ context.Context, c Connection) error {
		if c.State != "revoked" || c.Generation != 2 {
			return errors.New("local revocation was not committed")
		}
		close(entered)
		<-release
		return nil
	}
	done := make(chan error, 1)
	go func() { done <- r.SetConnectionState(ctx, "c", "revoked") }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("revocation callback did not begin")
	}
	locked := make(chan struct{})
	go func() {
		unlock, err := r.lockSession(context.Background(), "connection\x00c")
		if err == nil {
			unlock()
			close(locked)
		}
	}()
	select {
	case <-locked:
	case <-time.After(time.Second):
		t.Fatal("external callback held the connection lock")
	}
	connections, err := r.Connections(ctx)
	if err != nil || len(connections) != 1 || connections[0].Revocation != "pending" {
		t.Fatalf("pending connection = %#v, %v", connections, err)
	}
	if _, err := r.Connection(ctx, "c"); !errors.Is(err, ErrConnectionUnavailable) {
		t.Fatalf("locally denied connection = %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("revoke = %v", err)
	}
}

func TestConnectionRevocationTimeoutLeavesPending(t *testing.T) {
	r := connectionRuntime(t, t.TempDir(), func(context.Context, Principal) error { return nil })
	r.opts.RevocationTimeout = 20 * time.Millisecond
	ctx := principal(context.Background(), "u", "w")
	if err := r.RegisterConnection(context.Background(), Connection{ID: "c", UserID: "u", WorkspaceID: "w", Kind: "provider", TargetID: "work", CredentialRef: "ref", State: "ready"}); err != nil {
		t.Fatal(err)
	}
	canceled := make(chan error, 1)
	r.opts.RevokeCredential = func(ctx context.Context, _ Connection) error {
		<-ctx.Done()
		canceled <- ctx.Err()
		return ctx.Err()
	}
	if err := r.SetConnectionState(ctx, "c", "revoked"); !errors.Is(err, ErrRevocationPending) {
		t.Fatalf("revoke = %v, want pending", err)
	}
	select {
	case err := <-canceled:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("callback context = %v, want deadline exceeded", err)
		}
	case <-time.After(time.Second):
		t.Fatal("callback context was not canceled")
	}
	connections, err := r.Connections(ctx)
	if err != nil || len(connections) != 1 || connections[0].Revocation != "pending" {
		t.Fatalf("Connections() = %#v, %v", connections, err)
	}
}

func TestConnectionRevocationTimeoutOverridesLongCallback(t *testing.T) {
	r := connectionRuntime(t, t.TempDir(), func(context.Context, Principal) error { return nil })
	r.opts.RevocationTimeout = 20 * time.Millisecond
	ctx := principal(context.Background(), "u", "w")
	if err := r.RegisterConnection(context.Background(), Connection{ID: "c", UserID: "u", WorkspaceID: "w", Kind: "provider", TargetID: "work", CredentialRef: "ref", State: "ready"}); err != nil {
		t.Fatal(err)
	}
	r.opts.RevokeCredential = func(context.Context, Connection) error {
		// Deliberately ignore the cancellation signal and report success only
		// after the revocation deadline has elapsed.
		<-time.After(40 * time.Millisecond)
		return nil
	}
	if err := r.SetConnectionState(ctx, "c", "revoked"); !errors.Is(err, ErrRevocationPending) {
		t.Fatalf("revoke = %v, want pending", err)
	}
	connections, err := r.Connections(ctx)
	if err != nil || len(connections) != 1 || connections[0].Revocation != "pending" {
		t.Fatalf("Connections() = %#v, %v", connections, err)
	}
	if _, err := r.lookupConnection(ctx, "c"); !errors.Is(err, ErrConnectionUnavailable) {
		t.Fatalf("lookupConnection() = %v, want unavailable", err)
	}
}

func TestConnectionRevocationSerializesCallbacks(t *testing.T) {
	r := connectionRuntime(t, t.TempDir(), func(context.Context, Principal) error { return nil })
	ctx := principal(context.Background(), "u", "w")
	if err := r.RegisterConnection(context.Background(), Connection{ID: "c", UserID: "u", WorkspaceID: "w", Kind: "provider", TargetID: "work", CredentialRef: "ref", State: "ready"}); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var calls, active, maxActive atomic.Int32
	r.opts.RevokeCredential = func(context.Context, Connection) error {
		calls.Add(1)
		current := active.Add(1)
		for {
			maximum := maxActive.Load()
			if current <= maximum || maxActive.CompareAndSwap(maximum, current) {
				break
			}
		}
		close(entered)
		<-release
		active.Add(-1)
		return nil
	}
	done := make(chan error, 2)
	go func() { done <- r.SetConnectionState(ctx, "c", "revoked") }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("first callback did not begin")
	}
	go func() { done <- r.SetConnectionState(ctx, "c", "revoked") }()
	time.Sleep(20 * time.Millisecond)
	if got := calls.Load(); got != 1 || maxActive.Load() != 1 {
		t.Fatalf("callbacks=%d max active=%d, want one", got, maxActive.Load())
	}
	close(release)
	for range 2 {
		if err := <-done; err != nil {
			t.Fatalf("revoke = %v", err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("callbacks = %d, want one", got)
	}
}

func TestConnectionRevocationFailureKeepsLocalStateRevoked(t *testing.T) {
	r := connectionRuntime(t, t.TempDir(), func(context.Context, Principal) error { return nil })
	ctx := principal(context.Background(), "u", "w")
	if err := r.RegisterConnection(context.Background(), Connection{ID: "c", UserID: "u", WorkspaceID: "w", Kind: "provider", TargetID: "work", CredentialRef: "ref", State: "ready"}); err != nil {
		t.Fatal(err)
	}
	r.opts.RevokeCredential = func(context.Context, Connection) error { return errors.New("host failed") }
	if err := r.SetConnectionState(ctx, "c", "revoked"); !errors.Is(err, ErrRevocationPending) {
		t.Fatalf("revoke = %v, want pending", err)
	}
	if err := r.SetConnectionState(ctx, "c", "ready"); !errors.Is(err, ErrConnectionUnavailable) {
		t.Fatalf("reactivate = %v, want unavailable", err)
	}
	connections, err := r.Connections(ctx)
	if err != nil || len(connections) != 1 || connections[0].State != "revoked" {
		t.Fatalf("Connections() = %#v, %v", connections, err)
	}
}
