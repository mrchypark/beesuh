package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/mrchypark/beesuh"
	"github.com/mrchypark/beesuh/grantpolicy"
)

const sharedBodyLimit = 256 << 10

// This is trusted server configuration, not a browser-supplied policy document.
// Pins are candidate registration metadata until the owners register them.
type sharedServiceConfig struct {
	GrantURL                    string                `json:"grant_url"`
	AppKeyFile                  string                `json:"app_key_file"`
	GrantCredentialHintAudience string                `json:"grant_credential_hint_audience"`
	Contexts                    []sharedContextConfig `json:"contexts"`
}

type sharedContextConfig struct {
	OrganizationID    string                         `json:"organization_id"`
	WorkspaceID       string                         `json:"workspace_id"`
	ProviderID        string                         `json:"provider_id"`
	PEPClientID       string                         `json:"pep_client_id"`
	CatalogGeneration int64                          `json:"catalog_generation"`
	CatalogDigest     string                         `json:"catalog_digest"`
	ResourceType      string                         `json:"resource_type"`
	ResourceID        string                         `json:"resource_id"`
	ResourceEvidence  grantpolicy.ResourceEvidence   `json:"resource_evidence"`
	Operations        map[string]sharedCapabilityPin `json:"operations"`
}

type sharedCapabilityPin struct {
	CapabilityID      string `json:"capability_id"`
	CapabilityVersion int64  `json:"capability_version"`
	ContractDigest    string `json:"contract_digest"`
	ActionID          string `json:"action_id"`
}

type sharedBindingKey struct{ organization, workspace string }
type sharedPolicyChecker interface {
	Check(context.Context, string, grantpolicy.ActualSnapshot, grantpolicy.Credential) error
}
type sharedService struct {
	bindings     map[sharedBindingKey]*sharedContext
	hintAudience string
}
type sharedContext struct {
	organization, workspace string
	policy                  sharedPolicyChecker
	operations              map[string]grantpolicy.Binding
}
type sharedRequestKey struct{}

// No mutable request, body buffer, raw selectors, or cached decision is retained.
// The original Authorization header stays only in this request's memory.
type sharedRequest struct {
	owner         *serviceConfig
	binding       *sharedContext
	principal     beesuh.Principal
	operation     string
	actual        grantpolicy.ActualSnapshot
	authorization string
	policy        sharedPolicyChecker
}

type sharedRoute struct{ operation, method, path string }

func sharedRoutes() []sharedRoute {
	return []sharedRoute{
		{"run_execute", "POST", "/v1/chat"},
		{"run_read", "GET", "/v1/runs/{request_id}"},
		{"history_read", "GET", "/v1/history"},
		{"providers_list", "GET", "/v1/providers"},
		{"connection_options", "GET", "/v1/connection-options"},
		{"connection_select", "POST", "/v1/connections"},
		{"connections_list", "GET", "/v1/connections"},
		{"connection_revoke", "DELETE", "/v1/connections/{connection_id}"},
		{"run_cancel", "POST", "/v1/runs/{request_id}/cancel"},
	}
}

func (s *serviceConfig) configureShared() error {
	if len(s.Shared) == 0 {
		s.shared = nil
		return nil
	}
	bad := func() error { return errors.New("invalid shared service configuration") }
	var cfg *sharedServiceConfig
	if decodeServiceJSON(bytes.NewReader(s.Shared), 1<<20, &cfg) != nil || cfg == nil || !validServiceText(cfg.GrantCredentialHintAudience) || !utf8.ValidString(cfg.GrantCredentialHintAudience) || len(cfg.Contexts) == 0 || len(cfg.Contexts) > 128 {
		return bad()
	}
	prepared := &sharedService{bindings: make(map[sharedBindingKey]*sharedContext), hintAudience: cfg.GrantCredentialHintAudience}
	organization := ""
	for _, binding := range cfg.Contexts {
		if !validServiceText(binding.OrganizationID) || !utf8.ValidString(binding.OrganizationID) || !validServiceText(binding.WorkspaceID) || !utf8.ValidString(binding.WorkspaceID) || len(binding.Operations) == 0 {
			return bad()
		}
		if organization != "" && organization != binding.OrganizationID {
			return bad()
		}
		organization = binding.OrganizationID
		key := sharedBindingKey{binding.OrganizationID, binding.WorkspaceID}
		if _, exists := prepared.bindings[key]; exists {
			return bad()
		}
		c := grantpolicy.Config{BaseURL: cfg.GrantURL, AppKeyFile: cfg.AppKeyFile, OrganizationID: binding.OrganizationID, ProviderID: binding.ProviderID, PEPClientID: binding.PEPClientID, CatalogGeneration: binding.CatalogGeneration, CatalogDigest: binding.CatalogDigest, Operations: make(map[string]grantpolicy.Binding)}
		for _, route := range sharedRoutes() {
			pin, ok := binding.Operations[route.operation]
			if !ok {
				continue
			}
			c.Operations[route.operation] = grantpolicy.Binding{CapabilityID: pin.CapabilityID, CapabilityVersion: pin.CapabilityVersion, ContractDigest: pin.ContractDigest, ActionID: pin.ActionID, ResourceType: binding.ResourceType, ResourceID: binding.ResourceID, ResourceEvidence: binding.ResourceEvidence}
		}
		if len(c.Operations) != len(binding.Operations) {
			return bad()
		}
		policy, err := grantpolicy.New(c)
		if err != nil {
			return bad()
		}
		prepared.bindings[key] = &sharedContext{organization: binding.OrganizationID, workspace: binding.WorkspaceID, policy: policy, operations: c.Operations}
	}
	s.shared = prepared
	return nil
}

// Headers select only an existing server binding. They never establish access.
func singleSharedHeader(h http.Header, name string) (string, bool) {
	var values []string
	for key, v := range h {
		if strings.EqualFold(key, name) {
			values = append(values, v...)
		}
	}
	limit := 256
	if strings.EqualFold(name, "Authorization") {
		limit = 16384 + len("Bearer ")
	}
	if len(values) != 1 || values[0] == "" || len(values[0]) > limit || strings.ContainsAny(values[0], "\r\n\x00,") || !utf8.ValidString(values[0]) || strings.TrimSpace(values[0]) != values[0] {
		return "", false
	}
	return values[0], true
}

func sharedOperation(method, path string) (string, bool) {
	for _, route := range sharedRoutes() {
		if method != route.method {
			continue
		}
		expected, actual := strings.Split(route.path, "/"), strings.Split(path, "/")
		if len(expected) != len(actual) {
			continue
		}
		match := true
		for i, segment := range expected {
			if strings.HasPrefix(segment, "{") {
				if !sharedPathID(actual[i]) {
					match = false
					break
				}
			} else if segment != actual[i] {
				match = false
				break
			}
		}
		if match {
			return route.operation, true
		}
	}
	return "", false
}
func sharedPathID(id string) bool {
	if id == "" || id == "." || id == ".." || len(id) > 256 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("._:-", c)) {
			return false
		}
	}
	return true
}

func sharedSnapshot(r *http.Request, organization, workspace string) (string, grantpolicy.ActualSnapshot, error) {
	denied := func() (string, grantpolicy.ActualSnapshot, error) {
		return "", grantpolicy.ActualSnapshot{}, beesuh.ErrForbidden
	}
	if r.URL == nil || r.URL.Opaque != "" || r.URL.Fragment != "" || r.URL.ForceQuery || r.URL.EscapedPath() != r.URL.Path {
		return denied()
	}
	operation, ok := sharedOperation(r.Method, r.URL.Path)
	if !ok {
		return denied()
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return denied()
	}
	required := map[string]bool{}
	switch operation {
	case "history_read":
		required["workspace_id"] = true
		required["session_key"] = true
	case "run_read", "run_cancel":
		required["workspace_id"] = true
	}
	if len(query) != len(required) {
		return denied()
	}
	for key := range required {
		if len(query[key]) != 1 || !validServiceText(query.Get(key)) {
			return denied()
		}
	}
	if required["workspace_id"] && query.Get("workspace_id") != workspace {
		return denied()
	}
	var body []byte
	if r.Body != nil {
		body, err = io.ReadAll(io.LimitReader(r.Body, sharedBodyLimit+1))
		closeErr := r.Body.Close()
		r.Body = io.NopCloser(bytes.NewReader(body))
		if err != nil || closeErr != nil || len(body) > sharedBodyLimit {
			return denied()
		}
	}
	if operation != "run_execute" && operation != "connection_select" && len(body) != 0 {
		return denied()
	}
	if operation == "run_execute" {
		// This is a selector check only. The existing HTTP decoder still owns the
		// request schema and rejects unknown identity/authority fields.
		var payload struct {
			WorkspaceID string `json:"workspace_id"`
		}
		if json.Unmarshal(body, &payload) != nil || payload.WorkspaceID != workspace {
			return denied()
		}
	}
	canonicalQuery := strings.ReplaceAll(query.Encode(), "+", "%20")
	// Bind only the validated server selectors, never Authorization or raw headers.
	headers := "x-organization-id:" + organization + "\nx-workspace-id:" + workspace + "\n"
	return operation, grantpolicy.ActualSnapshot{Method: r.Method, CanonicalPath: r.URL.Path, CanonicalQuery: canonicalQuery, ForwardedHeaderDigest: grantpolicy.Digest([]byte(headers)), BodyDigest: grantpolicy.Digest(body), BodyLength: int64(len(body))}, nil
}

func (s *serviceConfig) authenticateShared(r *http.Request, human beesuh.Principal) (beesuh.Principal, error) {
	if s.shared == nil {
		return beesuh.Principal{}, beesuh.ErrForbidden
	}
	org, ok := singleSharedHeader(r.Header, "X-Organization-ID")
	if !ok {
		return beesuh.Principal{}, beesuh.ErrForbidden
	}
	workspace, ok := singleSharedHeader(r.Header, "X-Workspace-ID")
	if !ok {
		return beesuh.Principal{}, beesuh.ErrForbidden
	}
	binding, ok := s.shared.bindings[sharedBindingKey{org, workspace}]
	if !ok {
		return beesuh.Principal{}, beesuh.ErrForbidden
	}
	authorization, ok := singleSharedHeader(r.Header, "Authorization")
	token, _ := r.Context().Value(serviceAccessKey{}).(string)
	if !ok || authorization != "Bearer "+token {
		return beesuh.Principal{}, beesuh.ErrUnauthenticated
	}
	operation, actual, err := sharedSnapshot(r, binding.organization, binding.workspace)
	if err != nil {
		return beesuh.Principal{}, err
	}
	p := beesuh.Principal{OrganizationID: binding.organization, UserID: human.UserID, WorkspaceID: binding.workspace}
	snapshot := sharedRequest{owner: s, binding: binding, principal: p, operation: operation, actual: actual, authorization: authorization, policy: binding.policy}
	*r = *r.WithContext(context.WithValue(r.Context(), sharedRequestKey{}, snapshot))
	if err := s.authorize(r.Context(), p); err != nil {
		return beesuh.Principal{}, err
	}
	return p, nil
}

func (s *serviceConfig) authorizeShared(ctx context.Context, p, human beesuh.Principal, claims serviceClaims) error {
	snapshot, ok := ctx.Value(sharedRequestKey{}).(sharedRequest)
	if !ok || s.shared == nil || snapshot.owner != s || snapshot.policy == nil || snapshot.principal != p || human.UserID != p.UserID || !validServiceText(claims.Subject) || claims.ClientID != s.ClientID {
		return beesuh.ErrForbidden
	}
	binding, ok := s.shared.bindings[sharedBindingKey{p.OrganizationID, p.WorkspaceID}]
	token, _ := ctx.Value(serviceAccessKey{}).(string)
	if !ok || binding != snapshot.binding || snapshot.authorization != "Bearer "+token {
		return beesuh.ErrForbidden
	}
	if _, enabled := binding.operations[snapshot.operation]; !enabled {
		return beesuh.ErrForbidden
	}
	credential := grantpolicy.Credential{Authorization: snapshot.authorization, Subject: claims.Subject, Audience: s.shared.hintAudience}
	if err := snapshot.policy.Check(ctx, snapshot.operation, snapshot.actual, credential); err != nil {
		return beesuh.ErrForbidden
	}
	return nil
}

// Reconstruct credential ownership from the verified request's server binding,
// rather than assigning shared credentials to the personal workspace.
func (s *serviceConfig) discoveryPrincipal(ctx context.Context, subject string) (beesuh.Principal, error) {
	id := serviceUserID(s.Issuer, subject)
	if len(s.Shared) == 0 {
		return beesuh.Principal{UserID: id, WorkspaceID: id}, nil
	}
	snapshot, ok := ctx.Value(sharedRequestKey{}).(sharedRequest)
	if !ok || s.shared == nil || snapshot.owner != s || snapshot.principal.UserID != id {
		return beesuh.Principal{}, beesuh.ErrForbidden
	}
	return snapshot.principal, nil
}
