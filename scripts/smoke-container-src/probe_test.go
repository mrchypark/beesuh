package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestProbe(t *testing.T) {
	var tokens []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokens = append(tokens, r.Header.Get("Authorization"))
		if r.URL.Path != "/v1/providers" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer smoke-access-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"providers":[{"id":"smoke"}]}`))
	}))
	defer server.Close()
	if err := probe(context.Background(), server.URL+"/v1/providers"); err != nil {
		t.Fatal(err)
	}
	if strings.Join(tokens, ",") != "Bearer smoke-access-token,,Bearer invalid-token" {
		t.Fatalf("tokens = %q", tokens)
	}
}

func TestProbeResponseRejectsFailures(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		body     string
		redirect bool
	}{
		{name: "wrong status", status: 500, body: `"smoke"`},
		{name: "missing provider", status: 200, body: `{"providers":[]}`},
		{name: "oversized body", status: 200, body: strings.Repeat("x", (1<<20)+1)},
		{name: "redirect", status: 302, redirect: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.redirect {
					w.Header().Set("Location", "/other")
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			client := &http.Client{Timeout: time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			defer client.CloseIdleConnections()
			if err := probeResponse(context.Background(), client, server.URL, "smoke-access-token", 200); err == nil {
				t.Fatal("accepted invalid response")
			}
		})
	}
}

func TestProbeRejectsBrokenAuth(t *testing.T) {
	for _, brokenToken := range []string{"", "Bearer invalid-token"} {
		t.Run(brokenToken, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if token := r.Header.Get("Authorization"); token != "Bearer smoke-access-token" && token != brokenToken {
					w.WriteHeader(401)
					return
				}
				_, _ = w.Write([]byte(`"smoke"`))
			}))
			defer server.Close()
			if err := probe(context.Background(), server.URL); err == nil {
				t.Fatal("accepted broken auth")
			}
		})
	}
}

func TestProbeDeadline(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := probe(ctx, server.URL); err == nil {
		t.Fatal("accepted timed out request")
	}
}
