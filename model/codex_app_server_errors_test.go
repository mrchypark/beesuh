package model

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/mrchypark/beesuh/auth"
)

func TestCodexAppServerTerminalErrorCategories(t *testing.T) {
	// Wire variants checked against the installed Codex 0.152.0 experimental
	// TurnCompletedNotification schema. No authenticated provider is needed.
	for _, tc := range []struct {
		name string
		info any
		want error
	}{
		{"unauthorized", "unauthorized", auth.ErrInvalidCredentials},
		{"rate-limit", "rateLimitExceeded", auth.ErrRateLimited},
		{"usage-limit", "usageLimitExceeded", auth.ErrRateLimited},
		{"overloaded", "serverOverloaded", auth.ErrUnavailable},
		{"http-401", map[string]any{"httpConnectionFailed": map[string]any{"httpStatusCode": 401}}, auth.ErrInvalidCredentials},
		{"stream-403", map[string]any{"responseStreamConnectionFailed": map[string]any{"httpStatusCode": 403}}, auth.ErrForbidden},
		{"disconnected-429", map[string]any{"responseStreamDisconnected": map[string]any{"httpStatusCode": 429}}, auth.ErrRateLimited},
		{"attempts-503", map[string]any{"responseTooManyFailedAttempts": map[string]any{"httpStatusCode": 503}}, auth.ErrUnavailable},
		{"unknown", "other", nil},
		{"message-only", nil, nil},
		{"malformed-status", map[string]any{"httpConnectionFailed": map[string]any{"httpStatusCode": 401.5}}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wire, err := json.Marshal(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": "thread", "turn": map[string]any{"id": "turn", "status": "failed", "error": map[string]any{"codexErrorInfo": tc.info, "message": "secret unauthorized token", "additionalDetails": "private path"}}}})
			if err != nil {
				t.Fatal(err)
			}
			rpc := &appServerRPC{in: io.Discard, scan: bufio.NewScanner(bytes.NewReader(append(wire, '\n')))}
			text, err := rpc.waitTurn(context.Background(), "thread", "turn")
			if err == nil || text != "" || (tc.want != nil && !errors.Is(err, tc.want)) {
				t.Fatalf("result=%q error=%v want=%v", text, err, tc.want)
			}
			if tc.want == nil && errors.Is(err, auth.ErrInvalidCredentials) {
				t.Fatalf("unstructured error treated as invalid credentials: %v", err)
			}
			if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "private") {
				t.Fatalf("unsafe error: %v", err)
			}
		})
	}
}
