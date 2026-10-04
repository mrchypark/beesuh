package beesuh

import (
	"strings"
	"testing"

	"github.com/mrchypark/beesuh/conductor"
	"github.com/mrchypark/beesuh/model"
	"github.com/mrchypark/beesuh/opencode"
)

// Operator mode has no connection IDs and no principal scoping, so a named
// Conductor target can never be resolved at dispatch. Runtime.Open must reject
// that configuration instead of advertising permanently unusable tools.
func TestOperatorModeRejectsNamedOnlyConductorTargets(t *testing.T) {
	_, err := Open(t.Context(), Options{
		DataDir:       t.TempDir(),
		BackendID:     "test-backend",
		MaxConcurrent: 1,
		OpenCode:      opencode.Config{BaseURL: "http://127.0.0.1:1", Directory: "/workspace"},
		Native: &NativeOptions{
			Provider: model.Config{BaseURL: "http://127.0.0.1:1/v1", Model: "test"},
			Conductors: map[string]conductor.Config{
				"only": {BaseURL: "http://127.0.0.1:1"},
			},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "named Conductor targets require service mode") {
		t.Fatalf("named-only operator configuration error = %v", err)
	}
}

// The singleton Conductor configuration is the supported operator path.
func TestOperatorModeAcceptsSingletonConductorTarget(t *testing.T) {
	r, err := Open(t.Context(), Options{
		DataDir:       t.TempDir(),
		BackendID:     "test-backend",
		MaxConcurrent: 1,
		OpenCode:      opencode.Config{BaseURL: "http://127.0.0.1:1", Directory: "/workspace"},
		Native: &NativeOptions{
			Provider: model.Config{BaseURL: "http://127.0.0.1:1/v1", Model: "test"},
			Conductor: &conductor.Config{
				BaseURL:       "http://127.0.0.1:1",
				AllowedEvents: []string{"note"},
				Bindings:      map[string]conductor.Binding{"note": {HandlerKey: "note-handler", HandlerVersion: "v1", OperationKey: "create"}},
			},
		},
	})
	if err != nil {
		t.Fatalf("singleton operator Conductor: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}
