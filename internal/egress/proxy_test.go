package egress_test

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yteraoka/kibitz/internal/egress"
)

// logBuffer collects the proxy's log. Lines are written after the response
// has gone back to the client, so reading waits for the one wanted.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// wait returns the first egress line whose fields include all of want.
func (b *logBuffer) wait(t *testing.T, want map[string]any) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		b.mu.Lock()
		lines := strings.Split(b.buf.String(), "\n")
		b.mu.Unlock()
		for _, line := range lines {
			var fields map[string]any
			if json.Unmarshal([]byte(line), &fields) != nil || fields["msg"] != "egress" {
				continue
			}
			if matchesFields(fields, want) {
				return fields
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	t.Fatalf("no egress line with %v in:\n%s", want, b.buf.String())
	return nil
}

func matchesFields(fields, want map[string]any) bool {
	for k, v := range want {
		if fields[k] != v {
			return false
		}
	}
	return true
}

type fixture struct {
	upstream *httptest.Server
	proxy    *egress.Proxy
	session  *egress.Session
	log      *logBuffer
	client   *http.Client
	// seen is the last request the upstream server received.
	seen chan *http.Request
}

func newFixture(t *testing.T, cfg egress.Config, tlsUpstream bool) *fixture {
	t.Helper()
	f := &fixture{log: &logBuffer{}, seen: make(chan *http.Request, 16)}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.seen <- r
		_, _ = io.WriteString(w, "hello from "+r.URL.Path)
	})
	if tlsUpstream {
		f.upstream = httptest.NewTLSServer(handler)
	} else {
		f.upstream = httptest.NewServer(handler)
	}
	t.Cleanup(f.upstream.Close)

	upstreamRoots := x509.NewCertPool()
	if tlsUpstream {
		upstreamRoots.AddCert(f.upstream.Certificate())
	}
	logger := slog.New(slog.NewJSONHandler(f.log, nil))
	proxy, err := egress.New(egress.WithUpstreamRoots(cfg, upstreamRoots), logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = proxy.Close() })
	f.proxy = proxy

	session, err := proxy.Start(slog.String("event_id", "ev-1"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	f.session = session

	// The client trusts what an agent would: the inspection CA when there is
	// one, and the upstream's own certificate for a tunnel.
	clientRoots := x509.NewCertPool()
	if pem := proxy.CACertPEM(); pem != nil {
		clientRoots.AppendCertsFromPEM(pem)
	}
	if tlsUpstream {
		clientRoots.AddCert(f.upstream.Certificate())
	}
	proxyURL, _ := url.Parse(session.URL())
	f.client = &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			Proxy:           http.ProxyURL(proxyURL),
			TLSClientConfig: &tls.Config{RootCAs: clientRoots, MinVersion: tls.VersionTLS12},
		},
	}
	return f
}

// response is what a GET through the proxy came back with.
type response struct {
	StatusCode int
	Header     http.Header
}

func (f *fixture) get(t *testing.T, path string) (response, string) {
	t.Helper()
	resp, err := f.client.Get(f.upstream.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return response{StatusCode: resp.StatusCode, Header: resp.Header}, string(body)
}

func TestInspectedRequestIsForwardedAndLogged(t *testing.T) {
	f := newFixture(t, egress.Config{Allow: []string{"127.0.0.1"}, Inspect: true}, true)

	resp, body := f.get(t, "/v1/items?key=s3cret&page=2")
	if resp.StatusCode != http.StatusOK || body != "hello from /v1/items" {
		t.Fatalf("got %d %q", resp.StatusCode, body)
	}
	upstream := <-f.seen
	for _, h := range []string{"X-Forwarded-For", "Forwarded", "Proxy-Authorization"} {
		if upstream.Header.Get(h) != "" {
			t.Errorf("the upstream received %s", h)
		}
	}

	line := f.log.wait(t, map[string]any{"path": "/v1/items"})
	want := map[string]any{
		"event_id": "ev-1", "outcome": "allowed", "method": "GET", "scheme": "https",
		"host": "127.0.0.1", "inspected": true, "status": float64(200),
		"reason": "allowed by 127.0.0.1",
	}
	if !matchesFields(line, want) {
		t.Errorf("log line %v, want %v", line, want)
	}
	if keys, _ := json.Marshal(line["query_keys"]); string(keys) != `["key","page"]` {
		t.Errorf("query_keys = %s", keys)
	}
	if strings.Contains(f.log.String(), "s3cret") {
		t.Error("the query value reached the log")
	}
}

func TestInspectedPathIsDenied(t *testing.T) {
	f := newFixture(t, egress.Config{
		Allow:   []string{"127.0.0.1"},
		Deny:    []string{"127.0.0.1/admin*"},
		Inspect: true,
	}, true)

	for _, path := range []string{"/admin/users", "/v1/../admin"} {
		resp, _ := f.get(t, path)
		if resp.StatusCode != http.StatusForbidden || resp.Header.Get("X-Kibitz-Egress") != "denied" {
			t.Errorf("GET %s = %d, want 403 from the proxy", path, resp.StatusCode)
		}
	}
	f.log.wait(t, map[string]any{"path": "/admin/users", "outcome": "denied", "reason": "denied by 127.0.0.1/admin*"})

	if resp, _ := f.get(t, "/v1/ok"); resp.StatusCode != http.StatusOK {
		t.Errorf("an allowed path on the same host got %d", resp.StatusCode)
	}
	select {
	case r := <-f.seen:
		if r.URL.Path != "/v1/ok" {
			t.Errorf("a denied request reached the upstream: %s", r.URL.Path)
		}
	default:
		t.Error("the allowed request did not reach the upstream")
	}
}

func TestConnectOutsideTheAllowListIsRefused(t *testing.T) {
	f := newFixture(t, egress.Config{Allow: []string{"api.example.com"}, Inspect: true}, true)

	resp, err := f.client.Get(f.upstream.URL + "/")
	if err == nil {
		_ = resp.Body.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "Forbidden") {
		t.Fatalf("got %v, want the CONNECT refused", err)
	}
	f.log.wait(t, map[string]any{"method": "CONNECT", "outcome": "denied", "reason": "not in the allow list"})
}

func TestHostHeaderMustMatchTheConnect(t *testing.T) {
	f := newFixture(t, egress.Config{Allow: []string{"127.0.0.1"}, Inspect: true}, true)

	req, _ := http.NewRequest(http.MethodGet, f.upstream.URL+"/", nil)
	req.Host = "other.example"
	resp, err := f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMisdirectedRequest {
		t.Errorf("got %d, want 421", resp.StatusCode)
	}
	if len(f.seen) != 0 {
		t.Error("the request reached the upstream")
	}
}

func TestTunnelWithoutInspection(t *testing.T) {
	f := newFixture(t, egress.Config{Allow: []string{"127.0.0.1"}}, true)

	resp, body := f.get(t, "/secret/path")
	if resp.StatusCode != http.StatusOK || body != "hello from /secret/path" {
		t.Fatalf("got %d %q", resp.StatusCode, body)
	}
	f.client.CloseIdleConnections()
	line := f.log.wait(t, map[string]any{"method": "CONNECT", "outcome": "allowed"})
	if line["inspected"] != false || line["path"] != nil {
		t.Errorf("a tunnel logged %v", line)
	}
	if line["bytes_received"].(float64) == 0 {
		t.Errorf("no bytes counted: %v", line)
	}
}

func TestPassthroughHostIsTunnelled(t *testing.T) {
	f := newFixture(t, egress.Config{
		Allow:       []string{"127.0.0.1"},
		Inspect:     true,
		Passthrough: []string{"127.0.0.1"},
	}, true)

	if resp, _ := f.get(t, "/"); resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d", resp.StatusCode)
	}
	f.client.CloseIdleConnections()
	f.log.wait(t, map[string]any{"method": "CONNECT", "outcome": "allowed", "inspected": false})
}

func TestPlainHTTPIsForwarded(t *testing.T) {
	f := newFixture(t, egress.Config{Allow: []string{"127.0.0.1"}, Inspect: true}, false)

	resp, body := f.get(t, "/plain")
	if resp.StatusCode != http.StatusOK || body != "hello from /plain" {
		t.Fatalf("got %d %q", resp.StatusCode, body)
	}
	f.log.wait(t, map[string]any{"path": "/plain", "scheme": "http", "inspected": false, "outcome": "allowed"})
}

func TestInternalAddressNeedsAnAllowList(t *testing.T) {
	f := newFixture(t, egress.Config{Inspect: true}, true)

	resp, _ := f.get(t, "/")
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("got %d, want 502", resp.StatusCode)
	}
	line := f.log.wait(t, map[string]any{"outcome": "failed"})
	if !strings.Contains(line["error"].(string), "allow list") {
		t.Errorf("error = %v", line["error"])
	}
}

func TestPathRulesNeedInspection(t *testing.T) {
	_, err := egress.New(egress.Config{Allow: []string{"api.example.com/v1/*"}}, slog.New(slog.DiscardHandler))
	if err == nil {
		t.Fatal("a path rule was accepted without inspection")
	}
}

func TestSessionEnv(t *testing.T) {
	f := newFixture(t, egress.Config{Inspect: true, NoProxy: []string{"metadata.google.internal"}}, true)

	env := map[string]string{}
	for _, kv := range f.session.Env() {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	for _, k := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"} {
		if env[k] != f.session.URL() {
			t.Errorf("%s = %q, want %q", k, env[k], f.session.URL())
		}
	}
	if env["NO_PROXY"] != "localhost,127.0.0.1,::1,metadata.google.internal" {
		t.Errorf("NO_PROXY = %q", env["NO_PROXY"])
	}
	if env["NODE_USE_ENV_PROXY"] != "1" {
		t.Error("NODE_USE_ENV_PROXY is not set")
	}
	data, err := os.ReadFile(env["NODE_EXTRA_CA_CERTS"])
	if err != nil || !bytes.Contains(data, f.proxy.CACertPEM()) {
		t.Errorf("NODE_EXTRA_CA_CERTS does not hold the CA: %v", err)
	}
	if bundle := env["SSL_CERT_FILE"]; bundle != "" {
		data, err := os.ReadFile(bundle)
		if err != nil || !bytes.Contains(data, f.proxy.CACertPEM()) {
			t.Errorf("SSL_CERT_FILE does not hold the CA: %v", err)
		}
	}
}

func TestCloseCutsOpenTunnels(t *testing.T) {
	f := newFixture(t, egress.Config{Allow: []string{"127.0.0.1"}}, true)
	if resp, _ := f.get(t, "/"); resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d", resp.StatusCode)
	}
	// The client keeps the tunnel open for reuse; closing the session must
	// not wait for it.
	done := make(chan struct{})
	go func() {
		_ = f.session.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return")
	}
	f.log.wait(t, map[string]any{"method": "CONNECT", "outcome": "allowed"})
}
