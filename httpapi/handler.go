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
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/mrchypark/beesuh"
	"github.com/mrchypark/beesuh/auth"
	"github.com/mrchypark/beesuh/compos"
)

const (
	maxBodyBytes         = 256 << 10
	responseWriteTimeout = 5 * time.Second
)

// WriteBudget is the per-response write deadline the handler applies after
// long-running work returns. A server-level WriteTimeout must exceed the whole
// request budget and this value, or it cuts off a legitimate response.
const WriteBudget = responseWriteTimeout

type handler struct {
	runtime        *beesuh.Runtime
	token          [sha256.Size]byte
	authenticate   func(*http.Request) (beesuh.Principal, error)
	composProducer func(context.Context, beesuh.Principal) error
	identity       bool
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
// an HTTP Authorization header using the Bearer scheme. It does not permit
// Compos executions.
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
// and headers never establish a user or workspace. It does not permit Compos
// executions.
func NewAuthenticated(runtime *beesuh.Runtime, authenticate func(*http.Request) (beesuh.Principal, error)) (http.Handler, error) {
	return newAuthenticated(runtime, authenticate, nil)
}

// NewAuthenticatedWithComposProducer exposes the service API and permits
// /v1/compos only when authorizeCompos authorizes the authenticated producer.
// The callback must check the principal's current producer permission.
func NewAuthenticatedWithComposProducer(runtime *beesuh.Runtime, authenticate func(*http.Request) (beesuh.Principal, error), authorizeCompos func(context.Context, beesuh.Principal) error) (http.Handler, error) {
	if authorizeCompos == nil {
		return nil, errors.New("httpapi: Compos producer authorization callback is required")
	}
	return newAuthenticated(runtime, authenticate, authorizeCompos)
}

func newAuthenticated(runtime *beesuh.Runtime, authenticate func(*http.Request) (beesuh.Principal, error), composProducer func(context.Context, beesuh.Principal) error) (http.Handler, error) {
	if runtime == nil {
		return nil, errors.New("httpapi: runtime is required")
	}
	if !runtime.RequiresIdentity() {
		return nil, errors.New("httpapi: authenticated service mode requires an identity runtime")
	}
	if authenticate == nil {
		return nil, errors.New("httpapi: authentication callback is required")
	}
	return newHandler(handler{runtime: runtime, authenticate: authenticate, composProducer: composProducer, identity: true}), nil
}

func newHandler(h handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat", h.chat)
	mux.HandleFunc("POST /v1/compos", h.compos)
	mux.HandleFunc("GET /v1/status", h.status)
	mux.HandleFunc("GET /v1/providers", h.providers)
	mux.HandleFunc("GET /v1/history", h.history)
	mux.HandleFunc("GET /v1/runs/{request_id}", h.run)
	mux.HandleFunc("POST /v1/runs/{request_id}/cancel", h.cancelRun)
	mux.HandleFunc("GET /v1/connections", h.connections)
	mux.HandleFunc("DELETE /v1/connections/{connection_id}", h.revokeConnection)
	return responseWriteDeadline(h.authenticated(mux), responseWriteTimeout)
}

func (h handler) chat(w http.ResponseWriter, r *http.Request) {
	w.Header().Add("Vary", "Prefer")
	in, err := decode(r.Body)
	if err != nil {
		if !errors.Is(err, errBodyTooLarge) {
			err = errors.Join(beesuh.ErrInvalidInput, err)
		}
		h.fail(w, err, beesuh.Result{State: "failed"})
		return
	}
	if err := validate(in); err != nil {
		h.fail(w, errors.Join(beesuh.ErrInvalidInput, err), beesuh.Result{State: "failed"})
		return
	}
	req := beesuh.Request{
		WorkspaceID:           in.WorkspaceID,
		SessionKey:            in.SessionKey,
		RequestID:             in.RequestID,
		Provider:              in.Provider,
		ConnectionID:          in.ConnectionID,
		ConductorConnectionID: in.ConductorConnectionID,
		Prompt:                beesuh.Prompt{Text: in.Message},
	}
	if wantsAsync(r) {
		h.enqueue(w, r, req)
		return
	}
	out, err := h.runtime.Run(r.Context(), req)
	if err != nil {
		if out.State == "" {
			out.State = "failed"
		}
		h.fail(w, err, out)
		return
	}
	h.write(w, http.StatusOK, out)
}

func (h handler) compos(w http.ResponseWriter, r *http.Request) {
	w.Header().Add("Vary", "Prefer")
	producer, ok := beesuh.PrincipalFromContext(r.Context())
	if !h.identity || !ok || h.composProducer == nil || h.composProducer(r.Context(), producer) != nil {
		h.fail(w, beesuh.ErrForbidden, beesuh.Result{State: "failed"})
		return
	}
	query := r.URL.Query()
	session, provider := query.Get("session_key"), query.Get("provider")
	connection, conductorConnection := query.Get("connection_id"), query.Get("conductor_connection_id")
	if !validComposQuery(query) || !validID(session) || (provider != "" && !validID(provider)) || (connection != "" && !validID(connection)) || (conductorConnection != "" && !validID(conductorConnection)) {
		h.fail(w, beesuh.ErrInvalidInput, beesuh.Result{State: "failed"})
		return
	}
	in, err := compos.Decode(r.Body, session)
	if err != nil {
		h.fail(w, errors.Join(beesuh.ErrInvalidInput, err), beesuh.Result{State: "failed"})
		return
	}
	in.Provider = provider
	in.ConnectionID = connection
	in.ConductorConnectionID = conductorConnection
	if wantsAsync(r) {
		h.enqueue(w, r, in)
		return
	}
	out, err := h.runtime.Run(r.Context(), in)
	if err != nil {
		h.fail(w, err, out)
		return
	}
	h.write(w, http.StatusOK, out)
}

func (h handler) enqueue(w http.ResponseWriter, r *http.Request, req beesuh.Request) {
	out, err := h.runtime.Enqueue(r.Context(), req)
	if err != nil {
		if out.State == "" {
			out.State = "failed"
		}
		h.fail(w, err, out)
		return
	}
	w.Header().Set("Preference-Applied", "respond-async")
	w.Header().Set("Location", "/v1/runs/"+url.PathEscape(req.RequestID)+"?"+url.Values{"workspace_id": {req.WorkspaceID}}.Encode())
	status := http.StatusOK
	if out.State == "queued" || out.State == "running" {
		status = http.StatusAccepted
	}
	h.writeJSON(w, status, out)
}

func (h handler) providers(w http.ResponseWriter, r *http.Request) {
	out, err := h.runtime.Providers(r.Context())
	if err != nil {
		h.fail(w, err, beesuh.Result{State: "failed"})
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
		h.fail(w, err, beesuh.Result{State: "failed"})
		return
	}
	h.writeJSON(w, http.StatusOK, out)
}

func (h handler) history(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	workspace, session := query.Get("workspace_id"), query.Get("session_key")
	if len(query) != 2 || len(query["workspace_id"]) != 1 || len(query["session_key"]) != 1 || !validID(workspace) || !validID(session) {
		h.fail(w, beesuh.ErrInvalidInput, beesuh.Result{State: "failed"})
		return
	}
	out, err := h.runtime.History(r.Context(), workspace, session)
	if err != nil {
		h.fail(w, err, beesuh.Result{State: "failed"})
		return
	}
	h.writeJSON(w, http.StatusOK, out)
}

func (h handler) run(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	workspace, requestID := query.Get("workspace_id"), r.PathValue("request_id")
	if len(query) != 1 || len(query["workspace_id"]) != 1 || !validID(workspace) || !validID(requestID) {
		h.fail(w, beesuh.ErrInvalidInput, beesuh.Result{State: "failed"})
		return
	}
	out, err := h.runtime.GetRun(r.Context(), workspace, requestID)
	if err != nil {
		h.fail(w, err, out)
		return
	}
	if out.Error != "" {
		h.fail(w, storedFailure(out.FailureCategory), out)
		return
	}
	h.write(w, http.StatusOK, out)
}

func (h handler) connections(w http.ResponseWriter, r *http.Request) {
	out, err := h.runtime.Connections(r.Context())
	if err != nil {
		h.fail(w, err, beesuh.Result{State: "failed"})
		return
	}
	h.writeJSON(w, http.StatusOK, out)
}

func (h handler) revokeConnection(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("connection_id")
	if !validID(id) {
		h.fail(w, beesuh.ErrInvalidInput, beesuh.Result{State: "failed"})
		return
	}
	if err := h.runtime.SetConnectionState(r.Context(), id, "revoked"); err != nil {
		h.fail(w, err, beesuh.Result{State: "failed"})
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

type publicFailure struct {
	code    string
	message string
	status  int
}

func publicFailureFor(err error) publicFailure {
	if err == nil {
		return publicFailure{status: http.StatusOK}
	}
	switch {
	case errors.Is(err, errBodyTooLarge):
		return publicFailure{code: "request_too_large", message: "request body is too large", status: http.StatusRequestEntityTooLarge}
	}
	// The runtime owns the bounded category set so an initial failure and a
	// replayed one cannot drift apart.
	category := beesuh.FailureCategory(err)
	return publicFailure{code: category, message: beesuh.PublicFailureMessage(category), status: failureStatus(category)}
}

func failureStatus(category string) int {
	switch category {
	case beesuh.FailureInvalidInput:
		return http.StatusBadRequest
	case beesuh.FailureNotFound:
		return http.StatusNotFound
	case beesuh.FailureRequestConflict, beesuh.FailureConnectionUnavailable:
		return http.StatusConflict
	case beesuh.FailureUnauthorized:
		return http.StatusUnauthorized
	case beesuh.FailureForbidden:
		return http.StatusForbidden
	case beesuh.FailureRateLimited, beesuh.FailureOverloaded:
		return http.StatusTooManyRequests
	case beesuh.FailureServiceUnavailable, beesuh.FailureStorageCapacity:
		return http.StatusServiceUnavailable
	case beesuh.FailureDeadlineExceeded:
		return http.StatusGatewayTimeout
	default:
		return http.StatusBadGateway
	}
}

// storedFailure restores the runtime's bounded category after a completed run
// is read from storage. The stored error message is deliberately not parsed.
func storedFailure(category string) error {
	switch category {
	case beesuh.FailureInvalidInput:
		return beesuh.ErrInvalidInput
	case beesuh.FailureNotFound:
		return beesuh.ErrNotFound
	case beesuh.FailureRequestConflict:
		return beesuh.ErrConflict
	case beesuh.FailureUnauthorized:
		return beesuh.ErrUnauthenticated
	case beesuh.FailureForbidden:
		return beesuh.ErrForbidden
	case beesuh.FailureConnectionUnavailable:
		return beesuh.ErrConnectionUnavailable
	case beesuh.FailureRateLimited:
		return auth.ErrRateLimited
	case beesuh.FailureServiceUnavailable:
		return auth.ErrUnavailable
	case beesuh.FailureOverloaded:
		return beesuh.ErrOverloaded
	case beesuh.FailureStorageCapacity:
		return beesuh.ErrCapacity
	case beesuh.FailureDeadlineExceeded:
		return context.DeadlineExceeded
	default:
		return beesuh.ErrUpstream
	}
}

func errorStatus(err error) int { return publicFailureFor(err).status }

func publicError(err error) string {
	return publicFailureFor(err).message
}

func (h handler) write(w http.ResponseWriter, status int, out beesuh.Result) {
	h.writeJSON(w, status, out)
}

type errorResult struct {
	beesuh.Result
	ErrorCode string `json:"error_code,omitempty"`
}

func (h handler) fail(w http.ResponseWriter, err error, out beesuh.Result) {
	failure := publicFailureFor(err)
	slog.Error("httpapi request failed", "code", failure.code, "error", err)
	if out.State == "" {
		out.State = "failed"
	}
	out.Error = failure.message
	h.writeJSON(w, failure.status, errorResult{Result: out, ErrorCode: failure.code})
}

func (h handler) writeJSON(w http.ResponseWriter, status int, out any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(out)
}

type responseDeadlineWriter struct {
	http.ResponseWriter
	once    sync.Once
	timeout time.Duration
}

func responseWriteDeadline(next http.Handler, timeout time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(&responseDeadlineWriter{ResponseWriter: w, timeout: timeout}, r)
	})
}

func (w *responseDeadlineWriter) setDeadline() {
	w.once.Do(func() {
		if err := http.NewResponseController(w.ResponseWriter).SetWriteDeadline(time.Now().Add(w.timeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
			slog.Error("httpapi response write deadline failed", "error", err)
		}
	})
}

func (w *responseDeadlineWriter) WriteHeader(status int) {
	w.setDeadline()
	w.ResponseWriter.WriteHeader(status)
}

func (w *responseDeadlineWriter) Write(b []byte) (int, error) {
	w.setDeadline()
	return w.ResponseWriter.Write(b)
}

func (w *responseDeadlineWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
