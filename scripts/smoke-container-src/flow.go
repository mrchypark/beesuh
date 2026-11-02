package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"
)

type flow struct {
	ctx    context.Context
	client *http.Client
	base   string
}
type result struct {
	RequestID             string `json:"request_id"`
	State                 string `json:"state"`
	CancellationRequested bool   `json:"cancellation_requested"`
}
type selection struct {
	ID        string `json:"id"`
	Workspace string `json:"workspace_id"`
	State     string `json:"state"`
}

const flowState = "/tmp/smoke-selection.json"

type sharedSelectors struct{ organization, workspace string }
type fixtureContentTypesKey struct{}

func (f flow) makeRequest(method, path, body, token string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(f.ctx, method, f.base+path, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if selectors, ok := f.ctx.Value(sharedSelectors{}).(sharedSelectors); ok {
		req.Header.Set("X-Organization-ID", selectors.organization)
		req.Header.Set("X-Workspace-ID", selectors.workspace)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if values, ok := f.ctx.Value(fixtureContentTypesKey{}).([]string); ok {
		req.Header.Del("Content-Type")
		for _, value := range values {
			req.Header.Add("Content-Type", value)
		}
	}
	return req, nil
}

func (f flow) request(method, path, body, token string, status int) ([]byte, error) {
	req, err := f.makeRequest(method, path, body, token)
	if err != nil {
		return nil, err
	}
	return f.sendRequest(req, status)
}

func (f flow) sendRequest(req *http.Request, status int) ([]byte, error) {
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 1<<20 {
		return nil, fmt.Errorf("%s %s: oversized response", req.Method, req.URL.Path)
	}
	if err := checkFixtureSecrets(data); err != nil {
		return nil, err
	}
	if resp.StatusCode != status {
		return nil, fmt.Errorf("%s %s: status %d, want %d", req.Method, req.URL.Path, resp.StatusCode, status)
	}
	return data, nil
}
func (f flow) auth(method, path, body string, status int) ([]byte, error) {
	return f.request(method, path, body, "smoke-access-token", status)
}
func (f flow) stats() (fixtureStats, error) {
	local := f
	local.base = issuer
	data, err := local.request("GET", "/smoke/stats", "", "", 200)
	var stats fixtureStats
	if err == nil {
		err = json.Unmarshal(data, &stats)
	}
	if err == nil && stats.Violations != 0 {
		err = fmt.Errorf("fixture observed %d credential/route violations", stats.Violations)
	}
	return stats, err
}
func (f flow) counts(models, deliveries int64) error {
	stats, err := f.stats()
	if err != nil {
		return err
	}
	if stats.Models != models || stats.Deliveries != deliveries {
		return fmt.Errorf("model calls=%d credential deliveries=%d, want %d/%d", stats.Models, stats.Deliveries, models, deliveries)
	}
	return nil
}
func chat(s selection, id, message string) string {
	data, _ := json.Marshal(map[string]string{"workspace_id": s.Workspace, "session_key": "smoke-session", "request_id": id, "connection_id": s.ID, "message": message})
	return string(data)
}
func runPath(s selection, id string) string {
	return "/v1/runs/" + id + "?workspace_id=" + url.QueryEscape(s.Workspace)
}
func cancelPath(s selection, id string) string {
	return "/v1/runs/" + id + "/cancel?workspace_id=" + url.QueryEscape(s.Workspace)
}
func checkResult(data []byte, id, state string, cancelled bool) error {
	var out result
	if err := json.Unmarshal(data, &out); err != nil {
		return err
	}
	if out.RequestID != id || out.State != state || out.CancellationRequested != cancelled {
		return fmt.Errorf("unexpected run summary: request=%s state=%s cancellation=%t", out.RequestID, out.State, out.CancellationRequested)
	}
	return nil
}
func checkCancel(data []byte, id, state string, requested bool) error {
	if err := checkResult(data, id, state, requested); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if len(fields) != 3 {
		return fmt.Errorf("cancel response must contain only request_id, state, cancellation_requested")
	}
	if string(fields["cancellation_requested"]) != fmt.Sprint(requested) {
		return fmt.Errorf("cancel response must include a boolean cancellation_requested")
	}
	return nil
}
func (f flow) exercise() error {
	options, err := f.auth("GET", "/v1/connection-options", "", 200)
	if err != nil {
		return err
	}
	if !bytes.Contains(options, []byte(`"personal"`)) || bytes.Contains(options, []byte(`"foreign"`)) {
		return fmt.Errorf("discovery did not isolate personal connection")
	}
	if _, err = f.auth("POST", "/v1/connections", `{"collection_id":"models","connection_id":"personal","grant_id":"unapproved"}`, 403); err != nil {
		return err
	}
	data, err := f.auth("POST", "/v1/connections", `{"collection_id":"models","connection_id":"personal","grant_id":"approved"}`, 201)
	if err != nil {
		return err
	}
	var s selection
	if err = json.Unmarshal(data, &s); err != nil {
		return err
	}
	identity, _ := json.Marshal([]string{issuer, "smoke-user"})
	if s.ID == "" || s.Workspace != fmt.Sprintf("oidc_%x", sha256.Sum256(identity)) || s.State != "ready" {
		return fmt.Errorf("invalid selected connection identity")
	}
	if err = os.WriteFile(flowState, data, 0600); err != nil {
		return err
	}
	if err = f.counts(0, 0); err != nil {
		return err
	}
	data, err = f.auth("POST", "/v1/chat", chat(s, "smoke-success", "hello"), 200)
	if err != nil {
		return err
	}
	if err = checkResult(data, "smoke-success", "succeeded", false); err != nil {
		return err
	}
	if !bytes.Contains(data, []byte("smoke model reply")) {
		return fmt.Errorf("missing model output")
	}
	if err = f.readReplay(s); err != nil {
		return err
	}
	if err = f.counts(1, 1); err != nil {
		return err
	}
	data, err = f.auth("POST", cancelPath(s, "smoke-success"), "", 200)
	if err != nil {
		return err
	}
	if err = checkCancel(data, "smoke-success", "succeeded", false); err != nil {
		return err
	}
	// Keep the chat connected until the cancellation response has been checked.
	done := make(chan error, 1)
	go func() {
		data, err := f.auth("POST", "/v1/chat", chat(s, "smoke-cancel", blockMarker), 200)
		if err == nil {
			err = checkResult(data, "smoke-cancel", "cancelled", true)
		}
		done <- err
	}()
	for {
		stats, err := f.stats()
		if err != nil {
			return err
		}
		if stats.Active {
			break
		}
		select {
		case err := <-done:
			return fmt.Errorf("blocking chat finished before cancel: %v", err)
		case <-f.ctx.Done():
			return f.ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	for _, token := range []string{"", "invalid-token"} {
		if _, err = f.request("POST", cancelPath(s, "smoke-cancel"), "", token, 401); err != nil {
			return err
		}
	}
	data, err = f.auth("POST", cancelPath(s, "smoke-cancel"), "", 202)
	if err != nil {
		return err
	}
	if err = checkCancel(data, "smoke-cancel", "running", true); err != nil {
		return err
	}
	select {
	case err = <-done:
		if err != nil {
			return err
		}
	case <-f.ctx.Done():
		return f.ctx.Err()
	}
	return f.cancelled(s)
}
func (f flow) readReplay(s selection) error {
	data, err := f.auth("GET", runPath(s, "smoke-success"), "", 200)
	if err != nil {
		return err
	}
	if err = checkResult(data, "smoke-success", "succeeded", false); err != nil {
		return err
	}
	data, err = f.auth("GET", "/v1/history?workspace_id="+url.QueryEscape(s.Workspace)+"&session_key=smoke-session", "", 200)
	if err != nil {
		return err
	}
	if !bytes.Contains(data, []byte("smoke model reply")) {
		return fmt.Errorf("model output missing from history")
	}
	data, err = f.auth("POST", "/v1/chat", chat(s, "smoke-success", "hello"), 200)
	if err != nil {
		return err
	}
	return checkResult(data, "smoke-success", "succeeded", false)
}
func (f flow) cancelled(s selection) error {
	data, err := f.auth("GET", runPath(s, "smoke-cancel"), "", 200)
	if err != nil {
		return err
	}
	if err = checkResult(data, "smoke-cancel", "cancelled", true); err != nil {
		return err
	}
	data, err = f.auth("POST", cancelPath(s, "smoke-cancel"), "", 200)
	if err != nil {
		return err
	}
	if err = checkCancel(data, "smoke-cancel", "cancelled", true); err != nil {
		return err
	}
	data, err = f.auth("POST", "/v1/chat", chat(s, "smoke-cancel", blockMarker), 200)
	if err != nil {
		return err
	}
	if err = checkResult(data, "smoke-cancel", "cancelled", true); err != nil {
		return err
	}
	for {
		stats, err := f.stats()
		if err != nil {
			return err
		}
		if !stats.Active {
			break
		}
		select {
		case <-f.ctx.Done():
			return f.ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	return f.counts(2, 2)
}
func (f flow) resume() error {
	data, err := os.ReadFile(flowState)
	if err != nil {
		return err
	}
	var s selection
	if err = json.Unmarshal(data, &s); err != nil {
		return err
	}
	for {
		data, err = f.auth("GET", "/v1/connections", "", 200)
		if err == nil {
			break
		}
		select {
		case <-f.ctx.Done():
			return f.ctx.Err()
		case <-time.After(time.Second):
		}
	}
	if !bytes.Contains(data, []byte(s.ID)) {
		return fmt.Errorf("connection reference did not survive restart")
	}
	if err = f.readReplay(s); err != nil {
		return err
	}
	return f.cancelled(s)
}
func runFlow(ctx context.Context, base, phase string) error {
	ca, err := os.ReadFile("/config/ca.pem")
	if err != nil {
		return err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return fmt.Errorf("invalid fixture CA")
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 70 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	f := flow{ctx, client, base}
	if strings.HasPrefix(phase, "shared-") {
		return f.sharedFlow(phase)
	}
	if phase == "exercise" {
		return f.exercise()
	}
	return f.resume()
}

func checkFixtureSecrets(data []byte) error {
	for _, secret := range fixtureSecrets() {
		if bytes.Contains(data, []byte(secret)) {
			return fmt.Errorf("fixture credential exposed")
		}
	}
	// Decode response strings too, so JSON escaping cannot hide a model key.
	var value any
	if json.Unmarshal(data, &value) == nil {
		var inspect func(any) bool
		inspect = func(value any) bool {
			switch value := value.(type) {
			case string:
				for _, secret := range fixtureSecrets() {
					if strings.Contains(value, secret) {
						return false
					}
				}
			case []any:
				for _, item := range value {
					if !inspect(item) {
						return false
					}
				}
			case map[string]any:
				for key, item := range value {
					if !inspect(key) || !inspect(item) {
						return false
					}
				}
			}
			return true
		}
		if !inspect(value) {
			return fmt.Errorf("encoded fixture credential exposed")
		}
	}
	return nil
}

func (f flow) control(path string, value any) ([]byte, error) {
	local := f
	local.base = issuer
	body, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return local.request("POST", path, string(body), fixtureControl, 200)
}

func (f flow) policyStats() (fixturePolicyStats, error) {
	local := f
	local.base = issuer
	data, err := local.request("GET", "/smoke/policy-stats", "", fixtureControl, 200)
	var stats fixturePolicyStats
	if err == nil {
		err = json.Unmarshal(data, &stats)
	}
	if err == nil && stats.Violations != 0 {
		err = fmt.Errorf("PDP fixture snapshot/binding violations=%d", stats.Violations)
	}
	return stats, err
}

func (f flow) sharedRequest(method, path, body, token, organization, workspace, mode string, status int) ([]byte, error) {
	return f.sharedRequestContentTypes(method, path, body, token, organization, workspace, mode, nil, status)
}

func (f flow) sharedRequestContentTypes(method, path, body, token, organization, workspace, mode string, contentTypes []string, status int) ([]byte, error) {
	local := f
	local.ctx = context.WithValue(f.ctx, sharedSelectors{}, sharedSelectors{organization, workspace})
	if contentTypes != nil {
		local.ctx = context.WithValue(local.ctx, fixtureContentTypesKey{}, contentTypes)
	}
	req, err := local.makeRequest(method, path, body, token)
	if err != nil {
		return nil, err
	}
	// Capture the original Bee request before transport, not the PDP JSON POST.
	expected := fixtureExpectation{Method: method, Path: path, Body: body, Token: token, Mode: mode, ContentTypes: req.Header.Values("Content-Type")}
	if _, err := f.control("/smoke/expect", expected); err != nil {
		return nil, err
	}
	return local.sendRequest(req, status)
}

type sharedSaved struct {
	Selection  selection
	Models     int64
	Deliveries int64
	Run        json.RawMessage
	History    json.RawMessage
	References json.RawMessage
}

func (f flow) sharedFlow(phase string) error {
	request := func(method, path, body string, status int) ([]byte, error) {
		return f.sharedRequest(method, path, body, "smoke-access-token", sharedOrganization, sharedWorkspace, "allow", status)
	}
	// A real authenticated request establishes readiness of this exact Bee image.
	for {
		if _, err := request("GET", "/v1/providers", "", 200); err == nil {
			break
		}
		select {
		case <-f.ctx.Done():
			return f.ctx.Err()
		case <-time.After(time.Second):
		}
	}
	if phase == "shared-partial" {
		before, err := f.policyStats()
		if err != nil {
			return err
		}
		stats, err := f.stats()
		if err != nil {
			return err
		}
		s := selection{ID: "not-selected", Workspace: sharedWorkspace}
		if _, err := request("POST", "/v1/chat", chat(s, "missing-operation", "must deny"), 403); err != nil {
			return err
		}
		after, err := f.policyStats()
		if err != nil {
			return err
		}
		if before.Checks != after.Checks {
			return fmt.Errorf("missing operation reached PDP instead of failing closed")
		}
		return f.counts(stats.Models, stats.Deliveries)
	}
	var saved sharedSaved
	if phase == "shared-exercise" {
		before, err := f.stats()
		if err != nil {
			return err
		}
		options, err := request("GET", "/v1/connection-options", "", 200)
		if err != nil {
			return err
		}
		if !bytes.Contains(options, []byte(`"personal"`)) || bytes.Contains(options, []byte(`"foreign"`)) {
			return fmt.Errorf("shared discovery leaked another user")
		}
		data, err := request("POST", "/v1/connections", `{"collection_id":"models","connection_id":"personal","grant_id":"approved"}`, 201)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(data, &saved.Selection); err != nil {
			return err
		}
		s := saved.Selection
		if s.ID == "" || s.Workspace != sharedWorkspace || s.State != "ready" {
			return fmt.Errorf("shared selection lost server workspace ownership")
		}
		data, err = f.sharedRequestContentTypes("POST", "/v1/chat", chat(s, "shared-success", "shared workspace hello"), "smoke-access-token", sharedOrganization, sharedWorkspace, "allow", []string{"application/json; charset=utf-8"}, 200)
		if err != nil {
			return err
		}
		if err := checkResult(data, "shared-success", "succeeded", false); err != nil {
			return err
		}
		if !bytes.Contains(data, []byte("smoke model reply")) {
			return fmt.Errorf("shared model output missing")
		}
		saved.Models, saved.Deliveries = before.Models+1, before.Deliveries+1
		if err := f.counts(saved.Models, saved.Deliveries); err != nil {
			return err
		}
		for _, invalid := range []struct {
			name   string
			values []string
		}{
			{"missing", []string{}},
			{"duplicate", []string{"application/json", "application/json"}},
			{"invalid", []string{"text/plain"}},
		} {
			beforePolicy, err := f.policyStats()
			if err != nil {
				return err
			}
			if _, err := f.sharedRequestContentTypes("POST", "/v1/chat", chat(s, "bad-content-type-"+invalid.name, "must not dispatch"), "smoke-access-token", sharedOrganization, sharedWorkspace, "allow", invalid.values, 403); err != nil {
				return err
			}
			afterPolicy, err := f.policyStats()
			if err != nil {
				return err
			}
			if beforePolicy.Checks != afterPolicy.Checks {
				return fmt.Errorf("%s Content-Type reached PDP instead of local refusal", invalid.name)
			}
			if err := f.counts(saved.Models, saved.Deliveries); err != nil {
				return err
			}
		}
		for _, test := range []struct {
			name, token, organization, workspace, mode string
			status                                     int
		}{
			{"wrong-org", "smoke-access-token", "foreign-org", sharedWorkspace, "allow", 403},
			{"wrong-workspace", "smoke-access-token", sharedOrganization, "foreign-workspace", "allow", 403},
			{"body-workspace", "smoke-access-token", sharedOrganization, sharedWorkspace, "allow", 403},
			{"nonmember", "smoke-nonmember-token", sharedOrganization, sharedWorkspace, "allow", 403},
			{"wrong-user", "smoke-wrong-user-token", sharedOrganization, sharedWorkspace, "allow", 401},
			{"wrong-audience", "smoke-wrong-audience-token", sharedOrganization, sharedWorkspace, "allow", 401},
			{"wrong-client", "smoke-wrong-client-token", sharedOrganization, sharedWorkspace, "allow", 403},
			{"missing-use-scope", "smoke-no-use-token", sharedOrganization, sharedWorkspace, "allow", 401},
			{"missing-connections-audience", "smoke-no-connections-audience-token", sharedOrganization, sharedWorkspace, "allow", 401},
			{"deny", "smoke-access-token", sharedOrganization, sharedWorkspace, "deny", 403},
			{"outage", "smoke-access-token", sharedOrganization, sharedWorkspace, "outage", 403},
			{"binding", "smoke-access-token", sharedOrganization, sharedWorkspace, "bad-binding", 403},
			{"digest", "smoke-access-token", sharedOrganization, sharedWorkspace, "bad-digest", 403},
			{"headers", "smoke-access-token", sharedOrganization, sharedWorkspace, "bad-headers", 403},
			{"hint", "smoke-access-token", sharedOrganization, sharedWorkspace, "bad-hint", 403},
		} {
			attempt := s
			if test.name == "body-workspace" {
				attempt.Workspace = "foreign-workspace"
			}
			if _, err := f.sharedRequest("POST", "/v1/chat", chat(attempt, "deny-"+test.name, "must not dispatch"), test.token, test.organization, test.workspace, test.mode, test.status); err != nil {
				return fmt.Errorf("%s: %w", test.name, err)
			}
			if err := f.counts(saved.Models, saved.Deliveries); err != nil {
				return fmt.Errorf("%s leaked credential/model IO: %w", test.name, err)
			}
		}
		if _, err := f.control("/smoke/consent", map[string]bool{"revoked": true}); err != nil {
			return err
		}
		if _, err := request("POST", "/v1/connections", `{"collection_id":"models","connection_id":"personal","grant_id":"approved"}`, 403); err != nil {
			return err
		}
		if _, err := request("POST", "/v1/chat", chat(s, "revoked-consent", "must not dispatch"), 403); err != nil {
			return err
		}
		if err := f.counts(saved.Models, saved.Deliveries); err != nil {
			return fmt.Errorf("revoked credential grant leaked IO: %w", err)
		}
		if _, err := f.control("/smoke/consent", map[string]bool{"revoked": false}); err != nil {
			return err
		}
	} else if phase == "shared-resume" {
		data, err := os.ReadFile("/tmp/smoke-shared.json")
		if err != nil {
			return err
		}
		if err := json.Unmarshal(data, &saved); err != nil {
			return err
		}
	} else {
		return fmt.Errorf("unknown shared phase")
	}
	s := saved.Selection
	paths := []string{runPath(s, "shared-success"), "/v1/history?workspace_id=" + url.QueryEscape(s.Workspace) + "&session_key=smoke-session", "/v1/connections"}
	baselines := []*json.RawMessage{&saved.Run, &saved.History, &saved.References}
	for i, path := range paths {
		data, err := request("GET", path, "", 200)
		if err != nil {
			return err
		}
		if phase == "shared-exercise" {
			*baselines[i] = data
		} else {
			var want, got any
			if json.Unmarshal(*baselines[i], &want) != nil || json.Unmarshal(data, &got) != nil || !reflect.DeepEqual(want, got) {
				return fmt.Errorf("shared restart changed baseline %d", i)
			}
		}
	}
	if !bytes.Contains(saved.History, []byte("smoke model reply")) || !bytes.Contains(saved.References, []byte(s.ID)) {
		return fmt.Errorf("shared history/references missing")
	}
	data, err := request("POST", "/v1/chat", chat(s, "shared-success", "shared workspace hello"), 200)
	if err != nil {
		return err
	}
	if err := checkResult(data, "shared-success", "succeeded", false); err != nil {
		return err
	}
	if err := f.counts(saved.Models, saved.Deliveries); err != nil {
		return err
	}
	policy, err := f.policyStats()
	if err != nil {
		return err
	}
	if policy.Checks == 0 || policy.Allows == 0 {
		return fmt.Errorf("shared flow never validated a PDP decision")
	}
	if phase == "shared-exercise" {
		data, err := json.Marshal(saved)
		if err != nil {
			return err
		}
		return os.WriteFile("/tmp/smoke-shared.json", data, 0600)
	}
	return nil
}

// Scan raw storage and logs, including binary database files, without printing secrets.
func scan(paths []string) error {
	for _, root := range paths {
		if err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			file, err := os.Open(path)
			if err != nil {
				return err
			}
			defer file.Close()
			buf := make([]byte, 64<<10)
			tail := []byte{}
			for {
				n, err := file.Read(buf)
				data := append(tail, buf[:n]...)
				for _, secret := range fixtureSecrets() {
					if bytes.Contains(data, []byte(secret)) {
						return fmt.Errorf("credential persisted in %s", path)
					}
				}
				if len(data) > 128 {
					tail = append([]byte(nil), data[len(data)-128:]...)
				} else {
					tail = append([]byte(nil), data...)
				}
				if err == io.EOF {
					return nil
				}
				if err != nil {
					return err
				}
			}
		}); err != nil {
			return err
		}
	}
	return nil
}
