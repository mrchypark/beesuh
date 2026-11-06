// Test-only TLS authority and fake model for the isolated container smoke.
package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const issuer = "https://smoke-issuer:8443"
const serviceAudience = "urn:bee:smoke-api"

func main() {
	if len(os.Args) >= 3 && os.Args[1] == "scan" {
		if err := scan(os.Args[2:]); err != nil {
			log.Fatal(err)
		}
		return
	}
	if len(os.Args) == 3 && (os.Args[1] == "exercise" || os.Args[1] == "resume" || strings.HasPrefix(os.Args[1], "shared-")) {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		if err := runFlow(ctx, os.Args[2], os.Args[1]); err != nil {
			log.Fatal(err)
		}
		return
	}
	if len(os.Args) == 3 && os.Args[1] == "probe" {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		if err := probe(ctx, os.Args[2]); err != nil {
			log.Fatal(err)
		}
		return
	}
	if len(os.Args) == 3 && os.Args[1] == "init" {
		if err := initialize(os.Args[2]); err != nil {
			log.Fatal(err)
		}
		return
	}
	server := &http.Server{Addr: ":8443", Handler: fixtureHandler(), ReadHeaderTimeout: 5 * time.Second}
	log.Fatal(server.ListenAndServeTLS("/config/ca.pem", "/config/key.pem"))
}

func probe(ctx context.Context, url string) error {
	client := &http.Client{
		Timeout:       2 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	defer client.CloseIdleConnections()
	var err error
	for attempt := 0; attempt < 30; attempt++ {
		err = probeResponse(ctx, client, url, "smoke-access-token", http.StatusOK)
		if err == nil {
			break
		}
		if attempt == 29 {
			return fmt.Errorf("providers readiness: %w", err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("providers readiness: %w", ctx.Err())
		case <-time.After(time.Second):
		}
	}
	for _, token := range []string{"", "invalid-token"} {
		if err := probeResponse(ctx, client, url, token, http.StatusUnauthorized); err != nil {
			return fmt.Errorf("providers rejection (token present: %t): %w", token != "", err)
		}
	}
	return nil
}

func probeResponse(ctx context.Context, client *http.Client, url, token string, status int) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	const maxBody = 1 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return err
	}
	if len(body) > maxBody {
		return fmt.Errorf("response exceeds %d bytes", maxBody)
	}
	if resp.StatusCode != status {
		return fmt.Errorf("status = %d, want %d", resp.StatusCode, status)
	}
	if status == http.StatusOK {
		// No connection is selected: service mode must not expose provider templates.
		var providers []json.RawMessage
		if err := json.Unmarshal(body, &providers); err != nil {
			return fmt.Errorf("providers response must be a JSON array: %w", err)
		}
		if providers == nil || len(providers) != 0 {
			return fmt.Errorf("providers response must be an empty array before connection selection")
		}
	}
	return nil
}

func fixtureHandler() http.Handler {
	mux := http.NewServeMux()
	policy := newFixturePolicy()
	mux.Handle("/api/v2/http-capabilities/decide", policy)
	mux.HandleFunc("/api/v2/http-capabilities/catalog/bee-smoke-provider", policy.catalog)
	mux.HandleFunc("/smoke/expect", policy.expect)
	mux.HandleFunc("/smoke/policy-stats", policy.stats)
	mux.HandleFunc("POST /introspect", func(w http.ResponseWriter, r *http.Request) {
		client, secret, ok := r.BasicAuth()
		if !ok || client != "beesuh-smoke" || secret != "smoke-client-secret" {
			http.Error(w, "invalid client", 401)
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, "invalid form", 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		token := r.Form.Get("token")
		if token != "smoke-access-token" && token != "smoke-nonmember-token" && token != "smoke-wrong-user-token" && token != "smoke-wrong-audience-token" && token != "smoke-wrong-client-token" && token != "smoke-no-use-token" && token != "smoke-no-connections-audience-token" {
			fmt.Fprintln(w, `{"active":false}`)
			return
		}
		log.Print("validated introspection")
		subject, client := "smoke-user", "beesuh-smoke"
		audiences := []string{serviceAudience, resource}
		if token == "smoke-nonmember-token" {
			subject = "nonmember-user"
		}
		if token == "smoke-wrong-audience-token" {
			audiences = []string{"urn:foreign:api"}
		}
		if token == "smoke-wrong-client-token" {
			client = "foreign-client"
		}
		scopes := "openid execute goauthy.connections.read goauthy.connections.use"
		if token == "smoke-no-use-token" {
			scopes = "openid execute goauthy.connections.read goauthy.read"
		}
		if token == "smoke-no-connections-audience-token" {
			audiences = []string{serviceAudience}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"active": true, "sub": subject, "iss": issuer, "aud": audiences, "client_id": client, "scope": scopes, "token_type": "Bearer", "exp": time.Now().Add(time.Minute).Unix()})
	})
	mux.HandleFunc("GET /userinfo", func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if token != "smoke-access-token" && token != "smoke-nonmember-token" && token != "smoke-wrong-user-token" && token != "smoke-wrong-audience-token" && token != "smoke-wrong-client-token" && token != "smoke-no-use-token" && token != "smoke-no-connections-audience-token" {
			http.Error(w, "invalid token", 401)
			return
		}
		log.Print("validated userinfo")
		w.Header().Set("Content-Type", "application/json")
		subject := "smoke-user"
		if token == "smoke-nonmember-token" || token == "smoke-wrong-user-token" {
			subject = "nonmember-user"
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"sub": subject})
	})
	return personalFixture(mux)
}

func initialize(dir string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return err
	}
	cert := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "smoke-issuer"}, DNSNames: []string{"smoke-issuer"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		return err
	}
	rawKey, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	files := map[string][]byte{
		"ca.pem":         pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		"key.pem":        pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: rawKey}),
		"client-secret":  []byte("smoke-client-secret"),
		"providers.json": []byte(`{"default":"opencode","providers":[{"id":"opencode","kind":"openai-compatible","base_url":"https://smoke-issuer:8443/inference/go/openai/v1","model":"` + fakeModel + `"}]}`),
		"service.json":   []byte(`{"issuer":"` + issuer + `","introspection_url":"` + issuer + `/introspect","userinfo_url":"` + issuer + `/userinfo","client_id":"beesuh-smoke","client_secret_file":"/config/client-secret","audience":"` + serviceAudience + `","required_scopes":["openid","execute"],"goauthy":{"resource":"` + resource + `","providers":{"smoke-provider":"opencode"}}}`),
		"grant-app-key":  []byte(fixtureAppKey),
	}
	for _, partial := range []bool{false, true} {
		name := "shared-service.json"
		if partial {
			name = "partial-service.json"
		}
		data, err := sharedFixtureConfig(files["service.json"], partial)
		if err != nil {
			return err
		}
		files[name] = data
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			return err
		}
	}
	return nil
}
