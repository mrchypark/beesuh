// Package httpapi exposes the direct user conversation endpoint.
package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/mrchypark/beesuh"
	"github.com/mrchypark/beesuh/auth"
	"github.com/mrchypark/beesuh/compos"
)

const maxBodyBytes = 256 << 10

type handler struct {
	runtime      *beesuh.Runtime
	token        [sha256.Size]byte
	authenticate func(*http.Request) (beesuh.Principal, error)
	identity     bool
}

type chatRequest struct {
	WorkspaceID           string `json:"workspace_id"`
	SessionKey            string `json:"session_key"`
	RequestID             string `json:"request_id"`
	Provider              string `json:"provider,omitempty"`
	ConnectionID          string `json:"connection_id,omitempty"`
	ConductorConnectionID string `json:"conductor_connection_id,omitempty"`
	Message               string `json:"message"`
}

// New returns the direct user conversation API. The token must be supplied as
// an HTTP Authorization header using the Bearer scheme.
func New(runtime *beesuh.Runtime, token string) (http.Handler, error) {
	if runtime == nil {
		return nil, errors.New("httpapi: runtime is required")
	}
	if runtime.RequiresIdentity() {
		return nil, errors.New("httpapi: identity runtime requires authenticated service mode")
	}
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("httpapi: bearer token is required")
	}
	return newHandler(handler{runtime: runtime, token: sha256.Sum256([]byte(token))}), nil
}

// NewAuthenticated exposes the user-facing API behind the service's existing
// authentication. The callback is the only identity boundary: request bodies
// and headers never establish a user or workspace.
func NewAuthenticated(runtime *beesuh.Runtime, authenticate func(*http.Request) (beesuh.Principal, error)) (http.Handler, error) {
	if runtime == nil {
		return nil, errors.New("httpapi: runtime is required")
	}
	if !runtime.RequiresIdentity() {
		return nil, errors.New("httpapi: authenticated service mode requires an identity runtime")
	}
	if authenticate == nil {
		return nil, errors.New("httpapi: authentication callback is required")
	}
	return newHandler(handler{runtime: runtime, authenticate: authenticate, identity: true}), nil
}

func newHandler(h handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat", h.chat)
	mux.HandleFunc("POST /v1/compos", h.compos)
	mux.HandleFunc("GET /v1/status", h.status)
	mux.HandleFunc("GET /v1/providers", h.providers)
	mux.HandleFunc("GET /v1/history", h.history)
	mux.HandleFunc("GET /v1/runs/{request_id}", h.run)
	mux.HandleFunc("GET /v1/connections", h.connections)
	mux.HandleFunc("DELETE /v1/connections/{connection_id}", h.revokeConnection)
	return h.authenticated(mux)
}

func (h handler) chat(w http.ResponseWriter, r *http.Request) {
	in, err := decode(r.Body)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errBodyTooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		h.write(w, status, beesuh.Result{State: "failed", Error: err.Error()})
		return
	}
	if err := validate(in); err != nil {
		h.write(w, http.StatusBadRequest, beesuh.Result{State: "failed", Error: err.Error()})
		return
	}
	out, err := h.runtime.Run(r.Context(), beesuh.Request{
		WorkspaceID:           in.WorkspaceID,
		SessionKey:            in.SessionKey,
		RequestID:             in.RequestID,
		Provider:              in.Provider,
		ConnectionID:          in.ConnectionID,
		ConductorConnectionID: in.ConductorConnectionID,
		Prompt:                beesuh.Prompt{Text: in.Message},
	})
	if err != nil {
		if out.State == "" {
			out.State = "failed"
		}
		if out.Error == "" {
			out.Error = err.Error()
		}
	}
	h.write(w, errorStatus(err), out)
}

func (h handler) compos(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	session, provider := query.Get("session_key"), query.Get("provider")
	connection, conductorConnection := query.Get("connection_id"), query.Get("conductor_connection_id")
	if !validComposQuery(query) || !validID(session) || (provider != "" && !validID(provider)) || (connection != "" && !validID(connection)) || (conductorConnection != "" && !validID(conductorConnection)) {
		h.write(w, http.StatusBadRequest, beesuh.Result{State: "failed", Error: "httpapi: session_key query parameter is required"})
		return
	}
	in, err := compos.Decode(r.Body, session)
	if err != nil {
		h.write(w, http.StatusBadRequest, beesuh.Result{State: "failed", Error: err.Error()})
		return
	}
	in.Provider = provider
	in.ConnectionID = connection
	in.ConductorConnectionID = conductorConnection
	out, err := h.runtime.Run(r.Context(), in)
	if err != nil && out.Error == "" {
		out.State, out.Error = "failed", err.Error()
	}
	h.write(w, errorStatus(err), out)
}

func (h handler) providers(w http.ResponseWriter, r *http.Request) {
	out, err := h.runtime.Providers(r.Context())
	if err != nil {
		h.write(w, errorStatus(err), beesuh.Result{State: "failed", Error: err.Error()})
		return
	}
	h.writeJSON(w, http.StatusOK, out)
}

func (h handler) status(w http.ResponseWriter, r *http.Request) {
	if h.identity {
		h.write(w, http.StatusForbidden, beesuh.Result{State: "failed", Error: "status is unavailable"})
		return
	}
	out, err := h.runtime.Status(r.Context())
	if err != nil {
		h.write(w, errorStatus(err), beesuh.Result{State: "failed", Error: err.Error()})
		return
	}
	h.writeJSON(w, http.StatusOK, out)
}

func (h handler) history(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	workspace, session := query.Get("workspace_id"), query.Get("session_key")
	if len(query) != 2 || len(query["workspace_id"]) != 1 || len(query["session_key"]) != 1 || !validID(workspace) || !validID(session) {
		h.write(w, http.StatusBadRequest, beesuh.Result{State: "failed", Error: "httpapi: workspace_id and session_key are required"})
		return
	}
	out, err := h.runtime.History(r.Context(), workspace, session)
	if err != nil {
		h.write(w, errorStatus(err), beesuh.Result{State: "failed", Error: err.Error()})
		return
	}
	h.writeJSON(w, http.StatusOK, out)
}

func (h handler) run(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	workspace, requestID := query.Get("workspace_id"), r.PathValue("request_id")
	if len(query) != 1 || len(query["workspace_id"]) != 1 || !validID(workspace) || !validID(requestID) {
		h.write(w, http.StatusBadRequest, beesuh.Result{State: "failed", Error: "httpapi: workspace_id and request_id are required"})
		return
	}
	out, err := h.runtime.GetRun(r.Context(), workspace, requestID)
	if err != nil {
		if out.Error == "" {
			out.State, out.Error = "failed", err.Error()
		}
		h.write(w, errorStatus(err), out)
		return
	}
	h.write(w, http.StatusOK, out)
}

func (h handler) connections(w http.ResponseWriter, r *http.Request) {
	out, err := h.runtime.Connections(r.Context())
	if err != nil {
		h.write(w, errorStatus(err), beesuh.Result{State: "failed", Error: publicError(err)})
		return
	}
	h.writeJSON(w, http.StatusOK, out)
}

func (h handler) revokeConnection(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("connection_id")
	if !validID(id) {
		h.write(w, http.StatusBadRequest, beesuh.Result{State: "failed", Error: "httpapi: connection_id is required"})
		return
	}
	if err := h.runtime.SetConnectionState(r.Context(), id, "revoked"); err != nil {
		h.write(w, errorStatus(err), beesuh.Result{State: "failed", Error: publicError(err)})
		return
	}
	h.writeJSON(w, http.StatusOK, map[string]string{"connection_id": id, "state": "revoked"})
}

func (h handler) authenticated(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !h.identity {
			if !h.authorized(r.Header.Get("Authorization")) {
				h.write(w, http.StatusUnauthorized, beesuh.Result{State: "failed", Error: "authorization required"})
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		principal, err := h.authenticate(r)
		if err != nil {
			status := http.StatusUnauthorized
			if errors.Is(err, beesuh.ErrForbidden) {
				status = http.StatusForbidden
			}
			h.write(w, status, beesuh.Result{State: "failed", Error: "authorization required"})
			return
		}
		next.ServeHTTP(w, r.WithContext(beesuh.WithPrincipal(r.Context(), principal)))
	})
}

func (h handler) authorized(header string) bool {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	provided := sha256.Sum256([]byte(header[len(prefix):]))
	return subtle.ConstantTimeCompare(provided[:], h.token[:]) == 1
}

var errBodyTooLarge = errors.New("httpapi: input exceeds 256 KiB")

func decode(body io.Reader) (chatRequest, error) {
	b, err := io.ReadAll(io.LimitReader(body, maxBodyBytes+1))
	if err != nil {
		return chatRequest{}, fmt.Errorf("httpapi: read input: %w", err)
	}
	if len(b) > maxBodyBytes {
		return chatRequest{}, errBodyTooLarge
	}
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.DisallowUnknownFields()
	var in chatRequest
	if err := decoder.Decode(&in); err != nil {
		return chatRequest{}, fmt.Errorf("httpapi: invalid JSON: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return chatRequest{}, errors.New("httpapi: input must contain exactly one JSON value")
		}
		return chatRequest{}, fmt.Errorf("httpapi: invalid trailing JSON: %w", err)
	}
	return in, nil
}

func validate(in chatRequest) error {
	for _, value := range []string{in.WorkspaceID, in.SessionKey, in.RequestID} {
		if strings.TrimSpace(value) == "" || len(value) > 256 || strings.ContainsRune(value, 0) {
			return errors.New("httpapi: workspace_id, session_key and request_id must be nonempty, at most 256 bytes, without NUL")
		}
	}
	if in.Provider != "" && !validID(in.Provider) {
		return errors.New("httpapi: provider must be at most 256 bytes, without NUL")
	}
	if (in.ConnectionID != "" && !validID(in.ConnectionID)) || (in.ConductorConnectionID != "" && !validID(in.ConductorConnectionID)) {
		return errors.New("httpapi: connection IDs must be at most 256 bytes, without NUL")
	}
	if strings.TrimSpace(in.Message) == "" {
		return errors.New("httpapi: message is required")
	}
	if len(in.Message) > maxBodyBytes {
		return errors.New("httpapi: message exceeds 256 KiB")
	}
	return nil
}

func validComposQuery(query map[string][]string) bool {
	if len(query["session_key"]) != 1 {
		return false
	}
	for key, values := range query {
		if len(values) != 1 {
			return false
		}
		switch key {
		case "session_key", "provider", "connection_id", "conductor_connection_id":
		default:
			return false
		}
	}
	return true
}

func validID(value string) bool {
	return strings.TrimSpace(value) != "" && len(value) <= 256 && !strings.ContainsRune(value, 0)
}

func errorStatus(err error) int {
	if err == nil {
		return http.StatusOK
	}
	switch {
	case errors.Is(err, beesuh.ErrInvalidInput):
		return http.StatusBadRequest
	case errors.Is(err, beesuh.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, beesuh.ErrConflict), errors.Is(err, beesuh.ErrUncertain):
		return http.StatusConflict
	case errors.Is(err, beesuh.ErrUnauthenticated):
		return http.StatusUnauthorized
	case errors.Is(err, beesuh.ErrForbidden):
		return http.StatusForbidden
	case errors.Is(err, beesuh.ErrConnectionUnavailable):
		return http.StatusConflict
	case errors.Is(err, auth.ErrUnavailable):
		return http.StatusServiceUnavailable
	case errors.Is(err, auth.ErrRateLimited):
		return http.StatusTooManyRequests
	case errors.Is(err, beesuh.ErrOverloaded):
		return http.StatusTooManyRequests
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout
	default:
		return http.StatusBadGateway
	}
}

func publicError(err error) string {
	if errors.Is(err, beesuh.ErrUnauthenticated) || errors.Is(err, beesuh.ErrForbidden) {
		return "authorization required"
	}
	return err.Error()
}

func (h handler) write(w http.ResponseWriter, status int, out beesuh.Result) {
	h.writeJSON(w, status, out)
}

func (h handler) writeJSON(w http.ResponseWriter, status int, out any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(out)
}
