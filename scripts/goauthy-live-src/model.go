package main

import (
	"bytes"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
)

type fixtureModelConfig struct {
	Key       string   `json:"key"`
	Forbidden []string `json:"forbidden"`
}
type fixtureModel struct {
	mu                sync.Mutex
	control           string
	config            fixtureModelConfig
	calls, violations int32
}

func newFixtureModel(control string) *fixtureModel { return &fixtureModel{control: control} }
func (m *fixtureModel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	m.mu.Lock()
	defer m.mu.Unlock()
	if strings.HasPrefix(r.URL.Path, "/__fixture_control/") {
		if len(r.Header.Values("Authorization")) != 1 || !hmac.Equal([]byte(r.Header.Get("Authorization")), []byte("Bearer "+m.control)) {
			http.Error(w, "denied", 403)
			return
		}
		switch {
		case r.Method == "POST" && r.URL.Path == "/__fixture_control/model" && r.URL.RawQuery == "":
			var config fixtureModelConfig
			decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 128<<10))
			decoder.DisallowUnknownFields()
			if decoder.Decode(&config) != nil || config.Key == "" || len(config.Key) > 2048 || len(config.Forbidden) > 64 {
				http.Error(w, "invalid configuration", 400)
				return
			}
			if decoder.Decode(new(any)) != io.EOF {
				http.Error(w, "invalid configuration", 400)
				return
			}
			m.config = config
			w.WriteHeader(204)
			return
		case r.Method == "GET" && r.URL.Path == "/__fixture_control/counts":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]int32{"model": m.calls, "violations": m.violations})
			return
		default:
			http.NotFound(w, r)
			return
		}
	}
	if r.Method == "GET" && r.URL.Path == "/__fixture_model/health" && m.config.Key != "" {
		w.WriteHeader(200)
		return
	}
	if r.Method != "POST" || r.URL.Path != "/__fixture_model/v1/chat/completions" || r.URL.RawQuery != "" || m.config.Key == "" {
		http.NotFound(w, r)
		return
	}
	reject := func() { m.violations++; http.Error(w, "credential boundary violated", 400) }
	if len(r.Header.Values("Authorization")) != 1 || r.Header.Get("Authorization") != "Bearer "+m.config.Key {
		reject()
		return
	}
	body, e := io.ReadAll(http.MaxBytesReader(w, r.Body, 128<<10))
	if e != nil || bytes.Contains(body, []byte(m.config.Key)) {
		reject()
		return
	}
	for _, secret := range m.config.Forbidden {
		if secret == "" {
			continue
		}
		if bytes.Contains(body, []byte(secret)) {
			reject()
			return
		}
		for _, values := range r.Header {
			for _, v := range values {
				if strings.Contains(v, secret) {
					reject()
					return
				}
			}
		}
	}
	for name, values := range r.Header {
		if name == "Authorization" {
			continue
		}
		for _, v := range values {
			if strings.Contains(v, m.config.Key) {
				reject()
				return
			}
		}
	}
	m.calls++
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"id":"live-model","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"live verified"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
}
func (f *fixture) configureModel() error {
	forbidden := append([]string{f.clientSecret, f.password, f.controlSecret}, f.tokens...)
	_, _, e := f.request(f.public, "POST", issuer+"/__fixture_control/model", jsonBytes(fixtureModelConfig{Key: f.modelKey, Forbidden: forbidden}), map[string]string{"Content-Type": "application/json", "Authorization": "Bearer " + f.controlSecret}, 204)
	if e == nil {
		f.modelConfigured = true
	}
	return e
}
func (f *fixture) syncModelCalls() error {
	body, _, e := f.request(f.public, "GET", issuer+"/__fixture_control/counts", nil, map[string]string{"Authorization": "Bearer " + f.controlSecret}, 200)
	if e != nil {
		return e
	}
	var counters struct {
		Model      int32 `json:"model"`
		Violations int32 `json:"violations"`
	}
	if json.Unmarshal(body, &counters) != nil || counters.Violations != 0 {
		return errors.New("fixture model credential boundary failed")
	}
	f.calls.Store(counters.Model)
	return nil
}
