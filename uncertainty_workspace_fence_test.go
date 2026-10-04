package beesuh

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/mrchypark/beesuh/conductor"
)

// A stale admission read must not let a second connection dispatch after a
// first connection loses its acknowledgement. The second reservation is the
// authoritative fence because it runs as one serialized Rhiza write.
func TestWorkspaceFenceReservationRejectsStaleAdmission(t *testing.T) {
	var posts atomic.Int32
	conduit := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/events" {
			t.Fatalf("unexpected conductor path %q", request.URL.Path)
		}
		posts.Add(1) // The Conductor has applied the effect.
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("test server does not support connection hijacking")
		}
		connection, _, err := hijacker.Hijack()
		if err != nil {
			t.Fatal(err)
		}
		_ = connection.Close() // Lose the acknowledgement after application.
	}))
	defer conduit.Close()

	r := nativeOpen(t, nativeOptions(t.TempDir(), t.TempDir(), "http://127.0.0.1:1", nil))
	defer r.Close()
	client, err := conductor.New(conductor.Config{BaseURL: conduit.URL, AllowedEvents: []string{"deploy"}, Bindings: map[string]conductor.Binding{"deploy": {HandlerKey: "payload.echo", HandlerVersion: "1", OperationKey: "payload.echo"}}})
	if err != nil {
		t.Fatal(err)
	}
	requestA := Request{WorkspaceID: "workspace", SessionKey: "session-a", RequestID: "request-a", ConductorConnectionID: "connection-a"}
	requestB := Request{WorkspaceID: "workspace", SessionKey: "session-b", RequestID: "request-b", ConductorConnectionID: "connection-b"}

	// B passes the old, fast admission check and then pauses before its durable
	// reservation. A creates an effect with a different key and loses its ACK.
	if err := r.assertWorkspaceRecoverable(context.Background(), requestB.WorkspaceID); err != nil {
		t.Fatalf("B admission check: %v", err)
	}
	if err := r.recordUncertainIntent(context.Background(), requestA, "effect-a", "deploy"); err != nil {
		t.Fatalf("A reservation: %v", err)
	}
	if _, err := client.Emit(context.Background(), requestA.WorkspaceID, "deploy", []byte(`{}`), "effect-a"); err == nil {
		t.Fatal("A dispatch unexpectedly received an acknowledgement")
	}

	// B resumes after A's effect is pending. It cannot obtain a reservation and
	// therefore must not make a second external POST.
	if err := r.recordUncertainIntent(context.Background(), requestB, "effect-b", "deploy"); !errors.Is(err, ErrConflict) || !errors.Is(err, ErrUncertain) {
		t.Fatalf("B reservation=%v, want uncertain conflict", err)
	}
	if posts.Load() != 1 {
		t.Fatalf("external posts=%d, want only A's post", posts.Load())
	}
}

// With no existing pending row, concurrent reservations for different effect
// keys race at the conditional INSERT. Rhiza serializes the writes, so exactly
// one may commit and keep the workspace fenced.
func TestConcurrentWorkspaceReservationsAdmitOnlyOne(t *testing.T) {
	r := nativeOpen(t, nativeOptions(t.TempDir(), t.TempDir(), "http://127.0.0.1:1", nil))
	defer r.Close()

	start := make(chan struct{})
	results := make(chan error, 2)
	var group sync.WaitGroup
	for _, item := range []struct{ session, request, effect string }{
		{session: "session-a", request: "request-a", effect: "effect-a"},
		{session: "session-b", request: "request-b", effect: "effect-b"},
	} {
		group.Add(1)
		go func(item struct{ session, request, effect string }) {
			defer group.Done()
			<-start
			results <- r.recordUncertainIntent(context.Background(), Request{WorkspaceID: "workspace", SessionKey: item.session, RequestID: item.request}, item.effect, "deploy")
		}(item)
	}
	close(start)
	group.Wait()
	close(results)

	successes := 0
	for err := range results {
		if err == nil {
			successes++
			continue
		}
		if !errors.Is(err, ErrConflict) || !errors.Is(err, ErrUncertain) {
			t.Fatalf("reservation=%v, want uncertain conflict", err)
		}
	}
	if successes != 1 {
		t.Fatalf("successful reservations=%d, want 1", successes)
	}
	page, err := r.UnresolvedEffects(context.Background(), "workspace", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Effects) != 1 {
		t.Fatalf("pending effects=%d, want 1", len(page.Effects))
	}
}
