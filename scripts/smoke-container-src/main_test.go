package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTLSFixture(t *testing.T) {
	dir := t.TempDir()
	if err := initialize(dir); err != nil {
		t.Fatal(err)
	}
	cert, err := tls.LoadX509KeyPair(filepath.Join(dir, "ca.pem"), filepath.Join(dir, "key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	ca, err := os.ReadFile(filepath.Join(dir, "ca.pem"))
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		t.Fatal("invalid CA")
	}
	server := httptest.NewUnstartedServer(fixtureHandler())
	server.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	defer server.Close()
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: "smoke-issuer", MinVersion: tls.VersionTLS12}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	for _, tc := range []struct {
		path, token, secret string
		status              int
		active              bool
	}{
		{"/introspect", "smoke-access-token", "smoke-client-secret", 200, true},
		{"/introspect", "invalid", "smoke-client-secret", 200, false},
		{"/introspect", "smoke-access-token", "wrong", 401, false},
		{"/userinfo", "smoke-access-token", "", 200, false},
		{"/userinfo", "invalid", "", 401, false},
	} {
		t.Run(tc.path+tc.token+tc.secret, func(t *testing.T) {
			method := "GET"
			var body io.Reader
			if tc.path == "/introspect" {
				method = "POST"
				body = strings.NewReader("token=" + tc.token)
			}
			req, err := http.NewRequest(method, server.URL+tc.path, body)
			if err != nil {
				t.Fatal(err)
			}
			if method == "POST" {
				req.SetBasicAuth("beesuh-smoke", tc.secret)
				req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			} else {
				req.Header.Set("Authorization", "Bearer "+tc.token)
			}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.status {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.status)
			}
			if tc.status != 200 {
				return
			}
			var claims struct {
				Active  bool   `json:"active"`
				Subject string `json:"sub"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&claims); err != nil {
				t.Fatal(err)
			}
			if method == "POST" && claims.Active != tc.active {
				t.Fatalf("active = %v", claims.Active)
			}
			if (tc.active || method == "GET") && claims.Subject != "smoke-user" {
				t.Fatalf("subject = %q", claims.Subject)
			}
		})
	}
}
