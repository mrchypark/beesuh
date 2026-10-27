// Test-only TLS issuer for the container smoke; never delivers model credentials.
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

const issuer = "https://smoke-issuer:8443"

func main() {
	if len(os.Args) == 3 && os.Args[1] == "init" {
		if err := initialize(os.Args[2]); err != nil {
			log.Fatal(err)
		}
		return
	}
	server := &http.Server{Addr: ":8443", Handler: fixtureHandler(), ReadHeaderTimeout: 5 * time.Second}
	log.Fatal(server.ListenAndServeTLS("/config/ca.pem", "/config/key.pem"))
}

func fixtureHandler() http.Handler {
	mux := http.NewServeMux()
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
		if r.Form.Get("token") != "smoke-access-token" {
			fmt.Fprintln(w, `{"active":false}`)
			return
		}
		log.Print("validated introspection")
		_ = json.NewEncoder(w).Encode(map[string]any{"active": true, "sub": "smoke-user", "iss": issuer, "aud": "beesuh-smoke", "client_id": "beesuh-smoke", "scope": "openid execute", "token_type": "Bearer", "exp": time.Now().Add(time.Minute).Unix()})
	})
	mux.HandleFunc("GET /userinfo", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer smoke-access-token" {
			http.Error(w, "invalid token", 401)
			return
		}
		log.Print("validated userinfo")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, `{"sub":"smoke-user"}`)
	})
	return mux
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
		"providers.json": []byte(`{"default":"smoke","providers":[{"id":"smoke","kind":"openai-compatible","base_url":"https://smoke-issuer:8443/model/v1","model":"smoke"}]}`),
		"service.json":   []byte(`{"issuer":"` + issuer + `","introspection_url":"` + issuer + `/introspect","userinfo_url":"` + issuer + `/userinfo","client_id":"beesuh-smoke","client_secret_file":"/config/client-secret","audience":"beesuh-smoke","required_scopes":["openid","execute"],"goauthy":{"resource":"` + issuer + `/connections","providers":{"smoke-provider":"smoke"}}}`),
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			return err
		}
	}
	return nil
}
