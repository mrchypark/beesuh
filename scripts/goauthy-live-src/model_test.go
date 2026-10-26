package main

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestModelControlAndCredentialBoundary(t *testing.T) {
	model := newFixtureModel("control")
	call := func(path, authorization, body string) int {
		r := httptest.NewRequest("POST", path, strings.NewReader(body))
		if authorization != "" {
			r.Header.Set("Authorization", authorization)
		}
		w := httptest.NewRecorder()
		model.ServeHTTP(w, r)
		return w.Code
	}
	if got := call("/__fixture_control/model", "Bearer human", `{"key":"provider-key"}`); got != 403 {
		t.Fatal("control accepted human credential")
	}
	if got := call("/__fixture_control/model", "Bearer control", `{"key":"provider-key","forbidden":["human-token"]}`); got != 204 {
		t.Fatal("valid control configuration rejected")
	}
	if got := call("/__fixture_model/v1/chat/completions", "Bearer provider-key", `{"message":"hello"}`); got != 200 || model.calls != 1 {
		t.Fatal("valid model request failed")
	}
	if got := call("/__fixture_model/v1/chat/completions", "Bearer provider-key", `{"message":"human-token"}`); got != 400 || model.calls != 1 || model.violations != 1 {
		t.Fatal("human token body leakage was accepted")
	}
	if got := call("/__fixture_control/model", "Bearer control", `{"key":"provider-key","forbidden":["human-token","fresh-token"]}`); got != 204 {
		t.Fatal("fresh token filters rejected")
	}
	if got := call("/__fixture_model/v1/chat/completions", "Bearer provider-key", `{"message":"fresh-token"}`); got != 400 || model.calls != 1 {
		t.Fatal("fresh token body leakage was accepted")
	}
}
