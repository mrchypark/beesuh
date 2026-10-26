// Exercises real Goauthy HTTP APIs and the actual Beesuh container. Only the model is synthetic.
package main

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"html"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const resource = "https://goauthy.connections.local.test"
const beeAudience = "https://beesuh.local.test"
const callback = "https://rp.example.test/use-grant"
const issuer = "https://goauthy-live.test:8443"

var source = flag.String("goauthy", "../goauthy", "Read-only Goauthy checkout")
var beesuh = flag.String("beesuh", ".", "Beesuh checkout")
var image = flag.String("image", "beesuh:goauthy-live", "Already built Beesuh image")
var authProxy = flag.Bool("auth-proxy", false, "Internal transparent real-Goauthy counter proxy")
var tlsProbe = flag.String("tls-probe", "", "Internal fixture HTTPS reachability check")

type fixture struct {
	ctx                                                         context.Context
	dir, network, authContainer, beeContainer, authAddr, beeURL string
	public, owner                                               *http.Client
	csrf, token, clientSecret, modelKey, password               string
	imageID                                                     string
	authID, beeID, networkID                                    string
	calls                                                       atomic.Int32
	tokens                                                      []string
	controlSecret                                               string
	modelConfigured                                             bool
}

func main() {
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 12*time.Minute)
	defer cancel()
	if *tlsProbe != "" {
		if err := probeTLS(ctx, *tlsProbe); err != nil {
			fmt.Fprintln(os.Stderr, "TLS probe failed:", err)
			os.Exit(1)
		}
		return
	}
	if *authProxy {
		if err := serveAuthProxy(ctx); err != nil {
			fmt.Fprintln(os.Stderr, "proxy fixture failed")
			os.Exit(1)
		}
		return
	}
	f := &fixture{ctx: ctx}
	if err := f.run(); err != nil {
		fmt.Fprintln(os.Stderr, "FAIL:", err)
		os.Exit(1)
	}
	fmt.Println("PASS full isolated actual Goauthy + Beesuh Docker integration and verified cleanup")
}
func random() string {
	b := make([]byte, 32)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
func (f *fixture) command(dir string, env []string, stdin io.Reader, args ...string) ([]byte, error) {
	c := exec.CommandContext(f.ctx, args[0], args[1:]...)
	c.Dir = dir
	c.Stdin = stdin
	if env != nil {
		c.Env = append(os.Environ(), env...)
	}
	var out bytes.Buffer
	c.Stdout = &out
	c.Stderr = &out
	if e := c.Run(); e != nil {
		diagnostic := "output withheld"
		if args[0] == "docker" {
			diagnostic = out.String()
			for _, secret := range f.secretValues() {
				if secret != "" {
					diagnostic = strings.ReplaceAll(diagnostic, secret, "[REDACTED]")
				}
			}
			if len(diagnostic) > 2048 {
				diagnostic = diagnostic[:2048]
			}
		}
		return nil, fmt.Errorf("%s failed: %w: %s", args[0], e, diagnostic)
	}
	return out.Bytes(), nil
}
func (f *fixture) docker(args ...string) ([]byte, error) {
	return f.command("", nil, nil, append([]string{"docker"}, args...)...)
}
func (f *fixture) write(name string, value []byte) error {
	return os.WriteFile(filepath.Join(f.dir, name), value, 0600)
}
func jsonBytes(v any) []byte {
	b, e := json.Marshal(v)
	if e != nil {
		panic(e)
	}
	return b
}
func (f *fixture) request(c *http.Client, method, endpoint string, data []byte, headers map[string]string, want int) ([]byte, http.Header, error) {
	r, e := http.NewRequestWithContext(f.ctx, method, endpoint, bytes.NewReader(data))
	if e != nil {
		return nil, nil, e
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	resp, e := c.Do(r)
	if e != nil {
		category := "transport"
		if errors.Is(e, syscall.ECONNREFUSED) {
			category = "connection_refused"
		} else if errors.Is(e, context.DeadlineExceeded) {
			category = "timeout"
		} else {
			var networkError net.Error
			if errors.As(e, &networkError) && networkError.Timeout() {
				category = "timeout"
			}
		}
		return nil, nil, fmt.Errorf("%s %s request failed (%s)", method, r.URL.Path, category)
	}
	defer resp.Body.Close()
	b, e := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if e != nil {
		return nil, nil, errors.New("read failed")
	}
	if resp.StatusCode != want && !(want == 302 && resp.StatusCode == 303) {
		return nil, resp.Header, fmt.Errorf("%s %s status=%d want=%d", method, r.URL.Path, resp.StatusCode, want)
	}
	if f.beeURL != "" && endpoint == f.beeURL+"/v1/chat" {
		if e := f.syncModelCalls(); e != nil {
			return nil, resp.Header, e
		}
	}
	return b, resp.Header, nil
}
func (f *fixture) api(c *http.Client, method, path string, v any, h map[string]string, want int) (map[string]any, error) {
	var b []byte
	if v != nil {
		b = jsonBytes(v)
	}
	body, _, e := f.request(c, method, issuer+path, b, h, want)
	if e != nil {
		return nil, e
	}
	var result map[string]any
	if len(body) > 0 {
		e = json.Unmarshal(body, &result)
	}
	return result, e
}
func hidden(body []byte, name string) (string, error) {
	re := regexp.MustCompile(`<input[^>]*name="` + regexp.QuoteMeta(name) + `"[^>]*value="([^"]*)"`)
	m := re.FindSubmatch(body)
	if len(m) != 2 {
		return "", fmt.Errorf("missing form field %s", name)
	}
	return html.UnescapeString(string(m[1])), nil
}
func str(v map[string]any, k string) string { s, _ := v[k].(string); return s }
func boundedDocker(args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "docker", args...).CombinedOutput()
}
func (f *fixture) run() (err error) {
	f.dir, err = os.MkdirTemp("", "beesuh-goauthy-live-")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			f.failureEvidence()
		}
		err = errors.Join(err, f.cleanup())
	}()
	suffix := random()[:10]
	f.network = "beesuh-live-" + suffix
	f.authContainer = f.network + "-auth"
	f.beeContainer = f.network + "-bee"
	fmt.Println("FIXTURE", f.network)
	f.password = random()
	f.modelKey = random()
	f.controlSecret = random()
	if err = f.write("control-secret", []byte(f.controlSecret)); err != nil {
		return err
	}
	id, e := f.docker("image", "inspect", "--format", "{{.Id}}", *image)
	if e != nil {
		return e
	}
	f.imageID = strings.TrimSpace(string(id))
	fmt.Println("IMAGE", f.imageID)
	for _, repo := range []string{*beesuh, *source} {
		if e := f.sourceEvidence(repo); e != nil {
			return e
		}
	}
	fmt.Println("STEP build actual Goauthy standalone binary")
	arch, err := f.docker("info", "--format", "{{.Architecture}}")
	if err != nil {
		return err
	}
	target := "arm64"
	if strings.Contains(string(arch), "x86") || strings.Contains(string(arch), "amd64") {
		target = "amd64"
	}
	if _, err = f.command(*source, []string{"CGO_ENABLED=0", "GOOS=linux", "GOARCH=" + target}, nil, "go", "build", "-mod=readonly", "-trimpath", "-o", filepath.Join(f.dir, "goauthy"), "./cmd/goauthy"); err != nil {
		return err
	}
	binary, err := os.Open(filepath.Join(f.dir, "goauthy"))
	if err != nil {
		return err
	}
	binaryHash := sha256.New()
	_, err = io.Copy(binaryHash, binary)
	closeErr := binary.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	fmt.Printf("GOAUTHY-BINARY sha256=%x\n", binaryHash.Sum(nil))
	if _, err = f.command(filepath.Join(*beesuh, "scripts/goauthy-live-src"), []string{"CGO_ENABLED=0", "GOOS=linux", "GOARCH=" + target}, nil, "go", "build", "-trimpath", "-o", filepath.Join(f.dir, "live-proxy"), "."); err != nil {
		return err
	}
	phc, err := f.command(*source, nil, strings.NewReader(f.password+"\n"), "go", "run", "./cmd/goauthy-password")
	if err != nil {
		return err
	}
	for n, v := range map[string][]byte{"password-phc": phc, "bootstrap-secret": []byte(random()), "oauth-hmac": []byte(base64.RawStdEncoding.EncodeToString([]byte(random()[:32]))), "master-key": []byte(base64.RawStdEncoding.EncodeToString([]byte(random()[:32])))} {
		if e := f.write(n, v); e != nil {
			return e
		}
	}
	if err = f.certificates(); err != nil {
		return err
	}
	createdNetwork, err := f.docker("network", "create", "--label", runLabel+"="+f.network, f.network)
	if err != nil {
		return err
	}
	f.networkID, err = createdID(createdNetwork)
	if err != nil {
		return err
	}
	fmt.Println("NETWORK", f.networkID)
	args := []string{"create", "--name", f.authContainer, "--label", runLabel + "=" + f.network, "--network", f.network, "--network-alias", "goauthy-live.test", "-p", "127.0.0.1::8443", "--tmpfs", "/state:rw,mode=0700", "--user", "0:0", "--entrypoint", "/fixture/live-proxy"}
	env := map[string]string{"GOAUTHY_LISTEN_ADDR": "0.0.0.0:8443", "GOAUTHY_ISSUER": issuer, "GOAUTHY_TLS_CERT_FILE": "/fixture/tls.pem", "GOAUTHY_TLS_KEY_FILE": "/fixture/tls.key", "GOAUTHY_RHIZA_PROFILE": "standalone", "GOAUTHY_CLUSTER_ID": f.network, "GOAUTHY_NODE_ID": "live-0", "GOAUTHY_DATA_DIR": "/state/data", "GOAUTHY_MASTER_KEY_DIR": "/fixture/master-keys", "GOAUTHY_ACTIVE_MASTER_KEY_ID": "dev-1", "GOAUTHY_OAUTH_HMAC_SECRET_FILE": "/fixture/oauth-hmac", "GOAUTHY_BOOTSTRAP_CLIENT_SECRET_FILE": "/fixture/bootstrap-secret", "GOAUTHY_BOOTSTRAP_USER": "admin", "GOAUTHY_BOOTSTRAP_USER_PASSWORD_PHC_FILE": "/fixture/password-phc", "GOAUTHY_BOOTSTRAP_USER_EMAIL": "admin@beesuh.test", "GOAUTHY_CONNECTIONS_RESOURCE": resource, "GOAUTHY_BOOTSTRAP_ALLOWED_RESOURCES": string(jsonBytes([]string{resource, beeAudience}))}
	for k, v := range env {
		if k == "GOAUTHY_LISTEN_ADDR" {
			v = "127.0.0.1:8444"
		}
		args = append(args, "-e", k+"="+v)
	}
	args = append(args, f.imageID, "-auth-proxy")
	if e := os.Mkdir(filepath.Join(f.dir, "master-keys"), 0700); e != nil {
		return e
	}
	master, e := os.ReadFile(filepath.Join(f.dir, "master-key"))
	if e != nil {
		return e
	}
	if e = os.WriteFile(filepath.Join(f.dir, "master-keys", "dev-1"), master, 0600); e != nil {
		return e
	}
	createdAuth, err := f.docker(args...)
	if err != nil {
		return err
	}
	f.authID, err = createdID(createdAuth)
	if err != nil {
		return err
	}
	fmt.Println("CONTAINER auth", f.authID)
	if _, err = f.docker("cp", f.dir+"/.", f.authContainer+":/fixture"); err != nil {
		return err
	}
	if _, err = f.docker("start", f.authContainer); err != nil {
		return err
	}
	port, err := f.docker("port", f.authContainer, "8443/tcp")
	if err != nil {
		return err
	}
	f.authAddr, err = publishedAddress(port)
	if err != nil {
		return err
	}
	ca, err := os.ReadFile(filepath.Join(f.dir, "ca.pem"))
	if err != nil {
		return err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return errors.New("invalid CA")
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if strings.HasPrefix(addr, "goauthy-live.test:") {
			addr = f.authAddr
		}
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, addr)
	}
	f.public = &http.Client{Transport: tr, Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	f.owner = &http.Client{Transport: tr, Timeout: 20 * time.Second, CheckRedirect: f.public.CheckRedirect}
	f.owner.Jar, _ = cookiejar.New(nil)
	for i := 0; i < 300; i++ {
		_, _, e := f.request(f.public, "GET", issuer+"/.well-known/openid-configuration", nil, nil, 200)
		if e == nil {
			break
		}
		if i == 299 {
			logs, _ := f.docker("logs", "--tail", "12", f.authContainer)
			for _, secret := range f.secretValues() {
				if secret != "" {
					logs = bytes.ReplaceAll(logs, []byte(secret), []byte("[REDACTED]"))
				}
			}
			return fmt.Errorf("Goauthy readiness timeout: %s", logs)
		}
		select {
		case <-f.ctx.Done():
			return f.ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	fmt.Println("PASS real Goauthy HTTPS startup")
	if err = f.setup(); err != nil {
		return err
	}
	return f.exercise()
}
func (f *fixture) certificates() error {
	key, e := rsa.GenerateKey(rand.Reader, 2048)
	if e != nil {
		return e
	}
	now := time.Now()
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Beesuh isolated live CA"}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	der, e := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if e != nil {
		return e
	}
	if e = f.write("ca.pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})); e != nil {
		return e
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "goauthy-live.test"}, DNSNames: []string{"goauthy-live.test"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, e = x509.CreateCertificate(rand.Reader, cert, ca, &key.PublicKey, key)
	if e != nil {
		return e
	}
	if e = f.write("tls.pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})); e != nil {
		return e
	}
	return f.write("tls.key", pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
}
func authURL(client, redirect, challenge, state, scope, res string) string {
	q := url.Values{"response_type": {"code"}, "client_id": {client}, "redirect_uri": {redirect}, "code_challenge": {challenge}, "code_challenge_method": {"S256"}, "state": {state}, "nonce": {random()}, "scope": {scope}}
	if res != "" {
		q.Set("resource", res)
	}
	return issuer + "/oidc/authorize?" + q.Encode()
}
func challenge(v string) string {
	h := sha256.Sum256([]byte(v))
	return base64.RawURLEncoding.EncodeToString(h[:])
}
func (f *fixture) setup() error {
	verifier := random()
	state := random()
	b, _, e := f.request(f.owner, "GET", authURL("goauthy-dev", "http://localhost:5555/callback", challenge(verifier), state, "openid goauthy.read", ""), nil, nil, 200)
	if e != nil {
		return e
	}
	interaction, e := hidden(b, "interaction")
	if e != nil {
		return e
	}
	form := url.Values{"interaction": {interaction}, "username": {"admin"}, "password": {f.password}}
	_, h, e := f.request(f.owner, "POST", issuer+"/auth/login", []byte(form.Encode()), map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Sec-Fetch-Site": "same-origin"}, 302)
	if e != nil {
		return e
	}
	u, e := url.Parse(h.Get("Location"))
	if e != nil || u.Query().Get("code") == "" || u.Query().Get("state") != state {
		return errors.New("login callback validation failed")
	}
	base, _ := url.Parse(issuer)
	for _, c := range f.owner.Jar.Cookies(base) {
		raw, e := base64.RawURLEncoding.DecodeString(c.Value)
		if e == nil && len(raw) == 32 {
			m := hmac.New(sha256.New, raw)
			m.Write([]byte("goauthy/browser/csrf/v1"))
			f.csrf = base64.RawURLEncoding.EncodeToString(m.Sum(nil))
		}
	}
	if f.csrf == "" {
		return errors.New("browser session missing")
	}
	fmt.Println("PASS real password login and browser session")
	headers := f.session()
	provider := map[string]any{"id": "beesuh-model", "name": "Isolated model", "kind": "api_key", "enabled": true, "callback_uri": "", "scopes": []string{}, "connector": map[string]any{"id": "beesuh-model", "header": "Authorization", "prefix": "Bearer ", "operations": []any{map[string]any{"id": "account", "url": "https://api.example.com/account", "response_fields": map[string]string{"id": "string"}}}}}
	if _, e = f.api(f.owner, "POST", "/auth/v1/saas/providers", provider, headers, 201); e != nil {
		return e
	}
	if _, e = f.api(f.owner, "POST", "/auth/v1/auth-collections", map[string]any{"id": "models", "name": "Live models", "auth_method": "api_key", "enabled": true, "fields": []any{}, "provider_ids": []string{"beesuh-model"}}, headers, 201); e != nil {
		return e
	}
	scopes := []string{"openid", "goauthy.connections.read", "goauthy.connections.write", "goauthy.connections.use"}
	if _, e = f.api(f.owner, "POST", "/auth/v1/clients", map[string]any{"id": "beesuh-live", "name": "Beesuh live", "confidential": true, "redirect_uris": []string{callback}, "audience": []string{beeAudience, resource}, "default_aud": []string{resource}, "scopes": scopes, "default_scopes": scopes, "enabled_flows": []string{"authorization_code"}}, headers, 201); e != nil {
		return e
	}
	doc, e := f.api(f.owner, "POST", "/auth/v1/clients/beesuh-live/secret", nil, headers, 200)
	if e != nil {
		return e
	}
	f.clientSecret = str(doc, "secret")
	if f.clientSecret == "" {
		return errors.New("client secret missing")
	}
	return f.issueToken(scopes)
}
func (f *fixture) issueToken(scopes []string) error {
	verifier := random()
	state := random()
	_, h, e := f.request(f.owner, "GET", authURL("beesuh-live", callback, challenge(verifier), state, strings.Join(scopes, " "), beeAudience), nil, nil, 302)
	if e != nil {
		return e
	}
	u, e := url.Parse(h.Get("Location"))
	if e != nil || u.Query().Get("state") != state || u.Query().Get("code") == "" {
		return errors.New("PKCE callback failed")
	}
	form := url.Values{"grant_type": {"authorization_code"}, "client_id": {"beesuh-live"}, "code": {u.Query().Get("code")}, "redirect_uri": {callback}, "code_verifier": {verifier}}
	basic := base64.StdEncoding.EncodeToString([]byte("beesuh-live:" + url.QueryEscape(f.clientSecret)))
	b, _, e := f.request(f.public, "POST", issuer+"/oidc/token", []byte(form.Encode()), map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Authorization": "Basic " + basic}, 200)
	if e != nil {
		return e
	}
	var doc map[string]any
	if json.Unmarshal(b, &doc) != nil {
		return errors.New("token response malformed")
	}
	f.token = str(doc, "access_token")
	if f.token == "" {
		return errors.New("access token absent")
	}
	for _, key := range []string{"access_token", "id_token", "refresh_token"} {
		if value := str(doc, key); value != "" {
			f.tokens = append(f.tokens, value)
		}
	}
	fmt.Println("PASS confidential managed consumer authorization code + S256 PKCE")
	if f.modelConfigured {
		return f.configureModel()
	}
	return nil
}
func (f *fixture) session() map[string]string {
	return map[string]string{"Content-Type": "application/json", "Sec-Fetch-Site": "same-origin", "X-CSRF-Token": f.csrf}
}
func (f *fixture) bearer() map[string]string {
	return map[string]string{"Content-Type": "application/json", "Authorization": "Bearer " + f.token}
}

func (f *fixture) exercise() error {
	connection, e := f.api(f.owner, "POST", "/auth/v1/account/connections/models", map[string]any{"definition_revision": 1, "metadata": map[string]any{}}, f.session(), 201)
	if e != nil {
		return e
	}
	connectionID := str(connection, "id")
	if connectionID == "" {
		return errors.New("connection missing")
	}
	connPath := "/auth/v1/account/connections/models/" + url.PathEscape(connectionID)
	connector, e := f.api(f.owner, "GET", connPath+"/api-key/connector", nil, f.session(), 200)
	if e != nil {
		return e
	}
	if _, e = f.api(f.owner, "PUT", connPath+"/api-key", map[string]any{"api_key": f.modelKey, "version": 0, "connector_digest": str(connector, "digest")}, f.session(), 200); e != nil {
		return e
	}
	if e = f.startBee(); e != nil {
		return e
	}
	b, _, e := f.request(f.public, "GET", f.beeURL+"/v1/connection-options", nil, f.bearer(), 200)
	if e != nil {
		return e
	}
	if !bytes.Contains(b, []byte(connectionID)) {
		return errors.New("discovery did not include owned connection")
	}
	if bytes.Contains(b, []byte(f.modelKey)) || f.calls.Load() != 0 {
		return errors.New("discovery secret isolation failed")
	}
	fmt.Println("PASS Docker Beesuh lists actual Goauthy connection without model dispatch")
	if e = f.assertCounts(0); e != nil {
		return e
	}
	_, _, e = f.request(f.public, "POST", f.beeURL+"/v1/connections", jsonBytes(map[string]string{"collection_id": "models", "connection_id": connectionID, "grant_id": random()[:24]}), f.bearer(), 403)
	if e != nil {
		return e
	}
	if e = f.assertCounts(0); e != nil {
		return e
	}
	fmt.Println("PASS unapproved binding rejected without credential retrieval")
	state := random()
	handoff, e := f.api(f.public, "POST", "/auth/v1/connection-handoffs", map[string]any{"collection_id": "models", "connection_id": connectionID, "consumer_client_id": "beesuh-live", "mode": "credential_delivery", "purpose": "Isolated Beesuh Docker acceptance", "expires_at_unix_ms": time.Now().Add(time.Hour).UnixMilli(), "return_uri": callback, "state": state}, f.bearer(), 201)
	if e != nil {
		return e
	}
	reviewURI := str(handoff, "review_uri")
	if !strings.HasPrefix(reviewURI, issuer+"/account/connection-handoffs/") {
		return errors.New("untrusted review URI")
	}
	b, _, e = f.request(f.owner, "GET", reviewURI, nil, nil, 200)
	if e != nil {
		return e
	}
	csrf, e := hidden(b, "csrf_token")
	if e != nil {
		return e
	}
	digest, e := hidden(b, "review_digest")
	if e != nil {
		return e
	}
	// Exercise the actual HTML form's explicit CSRF-protected approval.
	form := url.Values{"csrf_token": {csrf}, "decision": {"approve"}, "review_digest": {digest}, "reviewed": {"yes"}}
	b, _, e = f.request(f.owner, "POST", reviewURI, []byte(form.Encode()), map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Sec-Fetch-Site": "same-origin", "Origin": issuer}, 200)
	if e != nil {
		return e
	}
	m := regexp.MustCompile(`id="handoff-return"[^>]*href="([^"]+)"`).FindSubmatch(b)
	if len(m) != 2 {
		return errors.New("approval callback absent")
	}
	u, e := url.Parse(html.UnescapeString(string(m[1])))
	if e != nil || u.Query().Get("state") != state || u.Query().Get("grant_id") == "" {
		return errors.New("approval state validation failed")
	}
	grant := u.Query().Get("grant_id")
	fmt.Println("PASS real Goauthy handoff page and CSRF approval + callback state")
	status, e := f.api(f.public, "GET", "/auth/v1/connections/models/"+connectionID+"/grants/"+grant, nil, f.bearer(), 200)
	if e != nil {
		return e
	}
	g, ok := status["grant"].(map[string]any)
	if !ok || str(g, "consumer_client_id") != "beesuh-live" || str(g, "connection_id") != connectionID || str(g, "mode") != "credential_delivery" {
		return errors.New("approved grant binding mismatch")
	}
	b, _, e = f.request(f.public, "POST", f.beeURL+"/v1/connections", jsonBytes(map[string]string{"collection_id": "models", "connection_id": connectionID, "grant_id": grant}), f.bearer(), 201)
	if e != nil {
		return e
	}
	var selected map[string]any
	if json.Unmarshal(b, &selected) != nil || str(selected, "id") == "" || str(selected, "workspace_id") == "" {
		return errors.New("selection failed")
	}
	if f.calls.Load() != 0 {
		return errors.New("selection dispatched model")
	}
	fmt.Println("PASS Docker Beesuh verifies and selects approved binding")
	if e = f.assertCounts(0); e != nil {
		return e
	}
	chat := map[string]any{"workspace_id": str(selected, "workspace_id"), "session_key": "live-chat", "request_id": "live-1", "connection_id": str(selected, "id"), "message": "Say live verified"}
	b, _, e = f.request(f.public, "POST", f.beeURL+"/v1/chat", jsonBytes(chat), f.bearer(), 200)
	if e != nil {
		return e
	}
	if !bytes.Contains(b, []byte("live verified")) || f.calls.Load() != 1 {
		return errors.New("model dispatch did not complete exactly once")
	}
	if bytes.Contains(b, []byte(f.modelKey)) || bytes.Contains(b, []byte(f.token)) {
		return errors.New("response leaked credential")
	}
	fmt.Println("PASS Docker model execution using real Goauthy vault delivery")
	if e = f.assertCounts(1); e != nil {
		return e
	}
	if _, e = f.docker("restart", "-t", "5", f.beeID); e != nil {
		return e
	}
	if e = f.refreshBeeURL("restart"); e != nil {
		return e
	}
	oldToken := f.token
	if e = f.issueToken([]string{"openid", "goauthy.connections.read", "goauthy.connections.write", "goauthy.connections.use"}); e != nil {
		return e
	}
	if f.token == oldToken {
		return errors.New("restart did not obtain a fresh human token")
	}
	for i := 0; i < 100; i++ {
		b, _, e = f.request(f.public, "GET", f.beeURL+"/v1/connections", nil, f.bearer(), 200)
		if e == nil {
			break
		}
		if i == 99 {
			return fmt.Errorf("Beesuh restart readiness failed: %w", e)
		}
		select {
		case <-f.ctx.Done():
			return f.ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	if !bytes.Contains(b, []byte(str(selected, "id"))) {
		return errors.New("approved reference lost on Beesuh restart")
	}
	if e = f.assertCounts(1); e != nil {
		return e
	}
	chat["request_id"] = "live-restarted"
	b, _, e = f.request(f.public, "POST", f.beeURL+"/v1/chat", jsonBytes(chat), f.bearer(), 200)
	if e != nil {
		return e
	}
	if !bytes.Contains(b, []byte("live verified")) || f.calls.Load() != 2 {
		return errors.New("restart execution failed")
	}
	if e = f.assertCounts(2); e != nil {
		return e
	}
	fmt.Println("PASS Beesuh container restart preserves selected references and executes with fresh human token")
	headers := f.session()
	headers["If-Match"] = `"1"`
	if _, e = f.api(f.owner, "DELETE", connPath+"/grants/"+grant, nil, headers, 204); e != nil {
		return e
	}
	chat["request_id"] = "live-2"
	b, _, e = f.request(f.public, "POST", f.beeURL+"/v1/chat", jsonBytes(chat), f.bearer(), 403)
	if e != nil {
		return e
	}
	if f.calls.Load() != 2 {
		return errors.New("revoked grant dispatched model")
	}
	fmt.Println("PASS actual consent revocation prevents subsequent model dispatch")
	if e = f.assertCounts(2); e != nil {
		return e
	}
	return f.checkSecrets()
}
func (f *fixture) startBee() error {
	if e := f.configureModel(); e != nil {
		return e
	}
	provider := map[string]any{"default": "model", "providers": []any{map[string]any{"id": "model", "kind": "openai-compatible", "base_url": issuer + "/__fixture_model/v1", "model": "live-test"}}}
	service := map[string]any{"issuer": issuer, "introspection_url": issuer + "/oidc/introspect", "userinfo_url": issuer + "/oidc/userinfo", "client_id": "beesuh-live", "client_secret_file": "/config/client-secret", "audience": beeAudience, "required_scopes": []string{"openid"}, "goauthy": map[string]any{"resource": resource, "providers": map[string]string{"beesuh-model": "model"}}}
	ca, e := os.ReadFile(filepath.Join(f.dir, "ca.pem"))
	if e != nil {
		return e
	}
	// Preserve the image USER and give its UID sole access to the OAuth client secret.
	createdBee, e := f.docker("create", "--name", f.beeContainer, "--label", runLabel+"="+f.network, "--network", f.network, "-p", "127.0.0.1::8080", "-e", "SSL_CERT_FILE=/config/ca.pem", f.imageID, "-listen", "0.0.0.0:8080", "-data-dir", "/data", "-providers", "/config/providers.json", "-service-config", "/config/service.json")
	if e != nil {
		return e
	}
	f.beeID, e = createdID(createdBee)
	if e != nil {
		return e
	}
	fmt.Println("CONTAINER bee", f.beeID)
	if e = f.copyBeeConfig(map[string][]byte{"providers.json": jsonBytes(provider), "service.json": jsonBytes(service), "client-secret": []byte(f.clientSecret), "ca.pem": ca}); e != nil {
		return e
	}
	if _, e = f.docker("start", f.beeID); e != nil {
		return e
	}
	if _, e = f.docker("exec", f.beeID, "/config/live-proxy", "-tls-probe", issuer+"/__fixture_model/health"); e != nil {
		return fmt.Errorf("model TLS reachability preflight: %w", e)
	}
	if e = f.refreshBeeURL("start"); e != nil {
		return e
	}
	for i := 0; i < 300; i++ {
		r, e := http.NewRequestWithContext(f.ctx, "GET", f.beeURL+"/v1/connection-options", nil)
		if e != nil {
			return e
		}
		r.Header.Set("Authorization", "Bearer "+f.token)
		resp, e := f.public.Do(r)
		if e == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return nil
			}
		}
		if i == 299 {
			return errors.New("Beesuh Docker readiness failed")
		}
		select {
		case <-f.ctx.Done():
			return f.ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return nil
}
func (f *fixture) checkSecrets() error {
	env, e := f.docker("inspect", "--format", "{{json .Config.Env}}", f.beeContainer)
	if e != nil {
		return e
	}
	for _, secret := range f.secretValues() {
		if bytes.Contains(env, []byte(secret)) {
			return errors.New("Beesuh environment contains credential")
		}
	}
	config := filepath.Join(f.dir, "checked-config")
	if e = os.Mkdir(config, 0700); e != nil {
		return e
	}
	if _, e = f.docker("cp", f.beeContainer+":/config/.", config); e != nil {
		return e
	}
	if e = filepath.WalkDir(config, func(path string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.IsDir() {
			return nil
		}
		b, e := os.ReadFile(path)
		if e != nil {
			return e
		}
		for _, secret := range append([]string{f.modelKey}, f.tokens...) {
			if bytes.Contains(b, []byte(secret)) {
				return errors.New("Beesuh configuration contains model key or human token")
			}
		}
		return nil
	}); e != nil {
		return e
	}
	for _, name := range []string{f.beeContainer, f.authContainer} {
		logs, e := f.docker("logs", name)
		if e != nil {
			return e
		}
		for _, secret := range f.secretValues() {
			if bytes.Contains(logs, []byte(secret)) {
				return errors.New("container logs contain fixture secret")
			}
		}
	}
	// Copy only Beesuh data, never the Goauthy vault whose encrypted persistence is expected.
	dest := filepath.Join(f.dir, "saved")
	if e := os.Mkdir(dest, 0700); e != nil {
		return e
	}
	if _, e := f.docker("cp", f.beeContainer+":/data/.", dest); e != nil {
		return e
	}
	if e := filepath.WalkDir(dest, func(path string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.IsDir() {
			return nil
		}
		b, e := os.ReadFile(path)
		if e != nil {
			return e
		}
		for _, secret := range f.secretValues() {
			if bytes.Contains(b, []byte(secret)) {
				return errors.New("Beesuh persisted fixture secret")
			}
		}
		return nil
	}); e != nil {
		return e
	}
	fmt.Println("PASS no model key or user token in container logs or Beesuh environment/config/persisted data")
	return nil
}

func publishedAddress(output []byte) (string, error) {
	lines := strings.Fields(string(output))
	if len(lines) == 0 {
		return "", errors.New("published port absent")
	}
	_, port, e := net.SplitHostPort(lines[0])
	if e != nil {
		return "", errors.New("published port malformed")
	}
	return net.JoinHostPort("127.0.0.1", port), nil
}

// Transparent TLS proxy records counts only. Every identity/consent response comes from the actual binary.
func serveAuthProxy(ctx context.Context) error {
	child := exec.Command("/fixture/goauthy")
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	if e := child.Start(); e != nil {
		return e
	}
	done := make(chan struct{})
	go func() { _ = child.Wait(); close(done) }()
	defer func() {
		_ = child.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = child.Process.Kill()
			<-done
		}
	}()
	ca, e := os.ReadFile("/fixture/ca.pem")
	if e != nil {
		return e
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return errors.New("CA invalid")
	}
	target, _ := url.Parse("https://127.0.0.1:8444")
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "goauthy-live.test", MinVersion: tls.VersionTLS12}}
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, e error) { http.Error(w, "upstream unavailable", 503) }
	var delivered, introspected, userinfo, exchanged atomic.Int32
	control, e := os.ReadFile("/fixture/control-secret")
	if e != nil {
		return e
	}
	model := newFixtureModel(string(control))
	var counterMu sync.Mutex
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/__fixture_") {
			model.ServeHTTP(w, r)
			return
		}
		counterMu.Lock()
		switch {
		case r.Method == "POST" && strings.HasPrefix(r.URL.Path, "/auth/v1/connection-grants/") && strings.HasSuffix(r.URL.Path, "/credential"):
			delivered.Add(1)
		case r.URL.Path == "/oidc/introspect":
			introspected.Add(1)
		case r.URL.Path == "/oidc/userinfo":
			userinfo.Add(1)
		case r.URL.Path == "/oidc/token":
			exchanged.Add(1)
		}
		data := jsonBytes(map[string]int32{"credential": delivered.Load(), "introspection": introspected.Load(), "userinfo": userinfo.Load(), "token": exchanged.Load()})
		if e := os.WriteFile("/state/proxy-counts.json.tmp", data, 0600); e != nil {
			counterMu.Unlock()
			http.Error(w, "counter unavailable", 503)
			return
		}
		if e := os.Rename("/state/proxy-counts.json.tmp", "/state/proxy-counts.json"); e != nil {
			counterMu.Unlock()
			http.Error(w, "counter unavailable", 503)
			return
		}
		counterMu.Unlock()
		proxy.ServeHTTP(w, r)
	})
	server := &http.Server{Addr: "0.0.0.0:8443", Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.ListenAndServeTLS("/fixture/tls.pem", "/fixture/tls.key") }()
	defer func() {
		c, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_ = server.Shutdown(c)
		cancel()
	}()
	select {
	case <-ctx.Done():
		return nil
	case e := <-serverDone:
		return e
	case <-done:
		return errors.New("Goauthy exited")
	}
}
func (f *fixture) assertCounts(want int32) error {
	if f.modelConfigured {
		if e := f.syncModelCalls(); e != nil {
			return e
		}
	}
	b, e := f.docker("exec", f.authContainer, "cat", "/state/proxy-counts.json")
	if e != nil {
		return e
	}
	var counters map[string]int32
	if json.Unmarshal(b, &counters) != nil || counters["credential"] != want {
		return fmt.Errorf("credential delivery count differs from %d", want)
	}
	if counters["token"] < 1 || counters["introspection"] < 1 || counters["userinfo"] < 1 {
		return errors.New("standard OAuth/OIDC HTTP evidence incomplete")
	}
	fmt.Printf("COUNTERS credential=%d model=%d token=%d introspection=%d userinfo=%d\n", counters["credential"], f.calls.Load(), counters["token"], counters["introspection"], counters["userinfo"])
	return nil
}

func (f *fixture) failureEvidence() {
	var report bytes.Buffer
	if counts, e := boundedDocker("exec", f.authID, "cat", "/state/proxy-counts.json"); e == nil {
		report.WriteString("real Goauthy route counts: ")
		report.Write(counts)
		report.WriteByte('\n')
	}
	report.WriteString(fmt.Sprintf("model accepted requests: %d\n", f.calls.Load()))
	for _, name := range []string{f.authID, f.beeID} {
		if name == "" {
			continue
		}
		report.WriteString(name + "\n")
		for _, args := range [][]string{{"inspect", "--format", "{{.State.Status}} {{.State.ExitCode}}", name}, {"logs", "--tail", "15", name}} {
			out, _ := boundedDocker(args...)
			report.Write(out)
			report.WriteByte('\n')
		}
	}
	data := report.Bytes()
	for _, secret := range f.secretValues() {
		if secret != "" {
			data = bytes.ReplaceAll(data, []byte(secret), []byte("[REDACTED]"))
		}
	}
	path := f.dir + ".failure.log"
	if os.WriteFile(path, data, 0600) == nil {
		fmt.Println("Sanitized failure evidence:", path)
	}
}

func (f *fixture) secretValues() []string {
	return append([]string{f.modelKey, f.password, f.clientSecret, f.token, f.controlSecret}, f.tokens...)
}

func (f *fixture) copyBeeConfig(files map[string][]byte) error {
	helper, e := os.ReadFile(filepath.Join(f.dir, "live-proxy"))
	if e != nil {
		return e
	}
	files["live-proxy"] = helper
	var buffer bytes.Buffer
	archive := tar.NewWriter(&buffer)
	if e := archive.WriteHeader(&tar.Header{Name: "config/", Typeflag: tar.TypeDir, Mode: 0700, Uid: 10001, Gid: 10001}); e != nil {
		return e
	}
	for name, data := range files {
		mode := int64(0644)
		if name == "client-secret" {
			mode = 0600
		}
		if name == "live-proxy" {
			mode = 0700
		}
		if e := archive.WriteHeader(&tar.Header{Name: "config/" + name, Mode: mode, Size: int64(len(data)), Uid: 10001, Gid: 10001}); e != nil {
			return e
		}
		if _, e := archive.Write(data); e != nil {
			return e
		}
	}
	if e := archive.Close(); e != nil {
		return e
	}
	_, e = f.command("", nil, &buffer, "docker", "cp", "-a", "-", f.beeID+":/")
	return e
}

func (f *fixture) refreshBeeURL(phase string) error {
	output, e := f.docker("port", f.beeID, "8080/tcp")
	if e != nil {
		return e
	}
	address, e := publishedAddress(output)
	if e != nil {
		return e
	}
	old := f.beeURL
	f.beeURL = "http://" + address
	fmt.Printf("BEE-PORT phase=%s old=%s new=%s\n", phase, old, f.beeURL)
	return nil
}
