// Package egress is the proxy the agent reaches the outside world through.
//
// Every request the agent makes -- to the model, to a remote MCP server, to
// anything a local MCP server decides to fetch -- goes through a proxy the
// worker runs, one per job. The proxy decides each request against the
// operator's allow and deny lists, and writes one log line for each, so that
// what a review touched is on record next to the review itself.
//
// With TLS inspection on, HTTPS is terminated with a certificate from an
// authority the worker makes at startup and only the agent's processes trust.
// That is what lets a rule name a path and a log line carry one; without it
// the proxy sees a host and a port and nothing else.
//
// The proxy is a policy and an audit trail, not a wall: it works because the
// agent's processes are told to use it, and a process that ignored
// HTTPS_PROXY would go around it. What stops that is the network the worker
// runs in (docs/security.md).
package egress

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Config configures the proxy.
type Config struct {
	// Allow lists the destinations the agent may reach. Empty allows
	// everything Deny does not name. See [Rule] for the syntax.
	Allow []string
	// Deny lists destinations the agent may never reach. It wins over Allow.
	Deny []string
	// Inspect terminates TLS so that requests inside HTTPS can be decided and
	// logged by path. Off, a rule may not name a path.
	Inspect bool
	// Passthrough names hosts whose TLS is not inspected even when Inspect is
	// on: a client that pins its certificate cannot be inspected. They are
	// decided by host alone.
	Passthrough []string
	// NoProxy names hosts the agent reaches directly, written as NO_PROXY
	// after [Loopback], which is always there. The metadata server belongs
	// here: it is how the agent gets the token it calls the model with.
	NoProxy []string
	// Observe, when set, is told the outcome of every request: "allowed",
	// "denied" or "failed".
	Observe func(outcome string)

	// upstreamRoots replaces the system roots for upstream TLS. Tests only.
	upstreamRoots *x509.CertPool
}

// Loopback is always reached without the proxy. opencode talks to its own
// server over http://127.0.0.1:<port>, and Bun sends even that to a proxy
// unless NO_PROXY names it: every call is then refused and the agent retries
// forever. Nothing the agent reaches on loopback leaves the machine.
var Loopback = []string{"localhost", "127.0.0.1", "::1"}

// Outcomes passed to [Config.Observe].
const (
	OutcomeAllowed = "allowed"
	OutcomeDenied  = "denied"
	OutcomeFailed  = "failed"
)

// Proxy holds what every job's proxy shares: the policy, the inspection
// authority, and the connections to upstream servers.
type Proxy struct {
	policy      *Policy
	ca          *authority
	passthrough hostList
	noProxy     []string
	observe     func(string)
	logger      *slog.Logger
	transport   *http.Transport

	dir string
	// nodeCAFile is what NODE_EXTRA_CA_CERTS points at: the authority, plus
	// whatever the worker's own environment already added.
	nodeCAFile string
	// bundleFile is the system's trusted roots plus the authority, for clients
	// that take one file instead of an addition. Empty when no system bundle
	// was found.
	bundleFile string
}

// New builds the proxy. Close it when the worker stops.
func New(cfg Config, logger *slog.Logger) (*Proxy, error) {
	policy, err := NewPolicy(cfg.Allow, cfg.Deny)
	if err != nil {
		return nil, err
	}
	if !cfg.Inspect && policy.HasPathRules() {
		return nil, errors.New("a rule names a path, which only TLS inspection can see")
	}

	p := &Proxy{
		policy:      policy,
		passthrough: parseHostList(cfg.Passthrough),
		noProxy:     append(append([]string(nil), Loopback...), cfg.NoProxy...),
		observe:     cfg.Observe,
		logger:      logger,
	}
	p.transport = &http.Transport{
		// Upstream is reached directly. A proxy the worker itself is told to
		// use would be a second place a request could go unrecorded.
		Proxy:                 nil,
		DialContext:           p.dial,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
		TLSClientConfig:       &tls.Config{RootCAs: cfg.upstreamRoots, MinVersion: tls.VersionTLS12},
	}

	if cfg.Inspect {
		if err := p.setUpInspection(); err != nil {
			_ = p.Close()
			return nil, err
		}
	}
	return p, nil
}

// setUpInspection makes the authority and writes the files the agent's
// processes read it from. Only the certificate is written; the key stays in
// this process.
func (p *Proxy) setUpInspection() error {
	ca, err := newAuthority(time.Now())
	if err != nil {
		return err
	}
	p.ca = ca

	dir, err := os.MkdirTemp("", "kibitz-egress-")
	if err != nil {
		return fmt.Errorf("creating the directory for the inspection CA: %w", err)
	}
	p.dir = dir

	// NODE_EXTRA_CA_CERTS takes one file, so a deployment that already uses it
	// for its own authority gets that authority kept alongside this one.
	nodeCA := append([]byte(nil), ca.pem...)
	if existing := os.Getenv("NODE_EXTRA_CA_CERTS"); existing != "" {
		if data, err := os.ReadFile(existing); err == nil { //nolint:gosec // a path the operator configured
			nodeCA = append(append(data, '\n'), nodeCA...)
		}
	}
	p.nodeCAFile = filepath.Join(dir, "node-extra-ca.pem")
	if err := os.WriteFile(p.nodeCAFile, nodeCA, 0o600); err != nil { //nolint:gosec // a directory this process just made
		return fmt.Errorf("writing the inspection CA: %w", err)
	}

	if system := systemBundle(); system != nil {
		p.bundleFile = filepath.Join(dir, "ca-bundle.pem")
		bundle := append(append(system, '\n'), ca.pem...)
		if err := os.WriteFile(p.bundleFile, bundle, 0o600); err != nil {
			return fmt.Errorf("writing the CA bundle: %w", err)
		}
	}
	return nil
}

// systemBundles are where Linux distributions keep their trusted roots.
var systemBundles = []string{
	"/etc/ssl/certs/ca-certificates.crt", // Debian, Ubuntu, Alpine
	"/etc/pki/tls/certs/ca-bundle.crt",   // Fedora, RHEL
	"/etc/ssl/cert.pem",                  // Alpine, macOS
}

// systemBundle reads the trusted roots the worker's own environment uses, or
// nil when there is no file to read them from.
func systemBundle() []byte {
	candidates := systemBundles
	if configured := os.Getenv("SSL_CERT_FILE"); configured != "" {
		candidates = append([]string{configured}, candidates...)
	}
	for _, name := range candidates {
		if data, err := os.ReadFile(name); err == nil && len(data) > 0 { //nolint:gosec // fixed paths and the operator's own setting
			return data
		}
	}
	return nil
}

// Check decides a URL the way a request to it would be decided, for a
// configuration check at startup: an MCP server the operator defined but the
// rules turn away is a mistake better found before a review needs it.
func (p *Proxy) Check(u *url.URL) Decision {
	host, port := splitAuthority(u.Host, u.Scheme)
	if u.Scheme == "https" && (p.ca == nil || p.passthrough.contains(host)) {
		return p.policy.DecideTunnel(host, port)
	}
	return p.policy.Decide(host, port, path.Clean("/"+u.Path))
}

// CACertPEM is the inspection authority's certificate, nil without
// inspection.
func (p *Proxy) CACertPEM() []byte {
	if p.ca == nil {
		return nil
	}
	return p.ca.pem
}

// Close releases the upstream connections and removes the CA files.
func (p *Proxy) Close() error {
	p.transport.CloseIdleConnections()
	if p.dir != "" {
		return os.RemoveAll(p.dir)
	}
	return nil
}

// dial connects upstream. Without an allow list, an internal address is
// refused: the agent could otherwise reach whatever the worker's network can,
// under any name it likes to resolve there. With one, every destination was
// named by the operator, internal ones included.
func (p *Proxy) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	d := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	if !p.policy.Restricted() {
		// Checked on the address actually dialled, after resolution, so a
		// name that resolves differently the second time changes nothing.
		d.Control = refuseInternal
	}
	return d.DialContext(ctx, network, addr)
}

// errInternal is returned for a connection to an internal address.
var errInternal = errors.New("internal addresses are only reachable through an allow list")

func refuseInternal(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil || internal(ip) {
		return fmt.Errorf("%s: %w", host, errInternal)
	}
	return nil
}

// cgnat is the shared address space, which a cloud network uses internally.
var cgnat = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

func internal(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() ||
		cgnat.Contains(ip)
}

// Session is one job's proxy: its own listener, so that every line it logs
// carries the job it belongs to.
type Session struct {
	proxy  *Proxy
	attrs  []slog.Attr
	ln     net.Listener
	srv    *http.Server
	ctx    context.Context
	cancel context.CancelFunc

	mu     sync.Mutex
	conns  map[net.Conn]struct{}
	closed bool
}

// Start opens a session. attrs are added to every line it logs; the job's
// event id belongs there.
func (p *Proxy) Start(attrs ...slog.Attr) (*Session, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("egress: listening: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Session{
		proxy:  p,
		attrs:  attrs,
		ln:     ln,
		ctx:    ctx,
		cancel: cancel,
		conns:  make(map[net.Conn]struct{}),
	}
	s.srv = &http.Server{
		Handler:           s,
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          slog.NewLogLogger(p.logger.Handler(), slog.LevelDebug),
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	go func() { _ = s.srv.Serve(ln) }()
	return s, nil
}

// URL is the proxy's address.
func (s *Session) URL() string { return "http://" + s.ln.Addr().String() }

// Env is what the agent's processes are given to use the proxy: the proxy
// variables in both spellings, and the files that make them trust the
// inspection authority.
func (s *Session) Env() []string {
	url := s.URL()
	noProxy := strings.Join(s.proxy.noProxy, ",")
	env := []string{
		"HTTP_PROXY=" + url, "HTTPS_PROXY=" + url,
		"http_proxy=" + url, "https_proxy=" + url,
		"NO_PROXY=" + noProxy, "no_proxy=" + noProxy,
		// Node's own fetch ignores the proxy variables unless told otherwise.
		"NODE_USE_ENV_PROXY=1",
	}
	if s.proxy.nodeCAFile != "" {
		env = append(env, "NODE_EXTRA_CA_CERTS="+s.proxy.nodeCAFile)
	}
	if s.proxy.bundleFile != "" {
		env = append(env,
			"SSL_CERT_FILE="+s.proxy.bundleFile,
			"CURL_CA_BUNDLE="+s.proxy.bundleFile,
			"REQUESTS_CA_BUNDLE="+s.proxy.bundleFile,
			"GIT_SSL_CAINFO="+s.proxy.bundleFile,
		)
	}
	return env
}

// Close stops the session and cuts every connection still open through it.
func (s *Session) Close() error {
	s.cancel()
	err := s.srv.Close()
	s.mu.Lock()
	s.closed = true
	for c := range s.conns {
		_ = c.Close()
	}
	clear(s.conns)
	s.mu.Unlock()
	return err
}

// track records a connection the server no longer owns, so that Close can
// cut it. It reports false when the session is already closed.
func (s *Session) track(c net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		_ = c.Close()
		return false
	}
	s.conns[c] = struct{}{}
	return true
}

func (s *Session) untrack(c net.Conn) {
	s.mu.Lock()
	delete(s.conns, c)
	s.mu.Unlock()
	_ = c.Close()
}

// ServeHTTP handles what a client sends a proxy: CONNECT for HTTPS, an
// absolute URL for plain HTTP.
func (s *Session) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		s.connect(w, r)
		return
	}
	if r.URL.Scheme != "http" || r.URL.Host == "" {
		http.Error(w, "kibitz egress: this is a proxy; send CONNECT or an absolute http:// URL", http.StatusBadRequest)
		return
	}
	s.forward(w, r, "http", r.URL.Host, false)
}

// entry is one line of the egress log.
type entry struct {
	method    string
	scheme    string
	host      string
	port      string
	path      string
	queryKeys []string
	inspected bool
	decision  Decision
	// reason replaces the decision's own when the refusal was not the
	// policy's.
	reason   string
	outcome  string
	status   int
	sent     int64
	received int64
	err      error
	started  time.Time
}

func (s *Session) log(e entry) {
	if s.proxy.observe != nil {
		s.proxy.observe(e.outcome)
	}

	attrs := append([]slog.Attr(nil), s.attrs...)
	attrs = append(attrs,
		slog.String("outcome", e.outcome),
		slog.String("method", e.method),
		slog.String("scheme", e.scheme),
		slog.String("host", e.host),
		slog.String("port", e.port),
	)
	if e.path != "" {
		attrs = append(attrs, slog.String("path", e.path))
	}
	if len(e.queryKeys) > 0 {
		// Names only. A query string is where an API key goes when it is
		// not in a header, and this log is not the place for one.
		attrs = append(attrs, slog.Any("query_keys", e.queryKeys))
	}
	reason := e.reason
	if reason == "" {
		reason = e.decision.Reason()
	}
	attrs = append(attrs,
		slog.Bool("inspected", e.inspected),
		slog.String("reason", reason),
	)
	if e.status != 0 {
		attrs = append(attrs, slog.Int("status", e.status))
	}
	attrs = append(attrs,
		slog.Int64("bytes_sent", e.sent),
		slog.Int64("bytes_received", e.received),
		slog.Int64("duration_ms", time.Since(e.started).Milliseconds()),
	)
	level := slog.LevelInfo
	if e.err != nil {
		attrs = append(attrs, slog.String("error", e.err.Error()))
	}
	if e.outcome != OutcomeAllowed {
		level = slog.LevelWarn
	}
	s.proxy.logger.LogAttrs(s.ctx, level, "egress", attrs...)
}

// forward sends one request upstream, if the policy allows it.
func (s *Session) forward(w http.ResponseWriter, r *http.Request, scheme, authority string, inspected bool) {
	e := entry{
		method:    r.Method,
		scheme:    scheme,
		path:      r.URL.Path,
		queryKeys: queryKeys(r),
		inspected: inspected,
		started:   time.Now(),
	}
	e.host, e.port = splitAuthority(authority, scheme)

	// The path is cleaned before it is matched, so "/v1/../admin" is decided
	// as the "/admin" a server would take it for.
	e.decision = s.proxy.policy.Decide(e.host, e.port, path.Clean("/"+r.URL.Path))
	if !e.decision.Allowed {
		e.outcome, e.status = OutcomeDenied, http.StatusForbidden
		refuse(w, e.decision, e.host)
		s.log(e)
		return
	}

	rec := &recorder{ResponseWriter: w}
	body := &countingReader{ReadCloser: r.Body}
	if r.Body != nil && r.Body != http.NoBody {
		r.Body = body
	}
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = scheme
			pr.Out.URL.Host = authority
		},
		Transport: s.proxy.transport,
		// Model responses stream. Holding them back would turn every
		// response into one that arrives all at once at the end.
		FlushInterval: -1,
		ModifyResponse: func(resp *http.Response) error {
			e.status = resp.StatusCode
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			e.err = err
			e.status = http.StatusBadGateway
			http.Error(w, "kibitz egress: "+err.Error(), http.StatusBadGateway)
		},
		ErrorLog: slog.NewLogLogger(s.proxy.logger.Handler(), slog.LevelDebug),
	}
	rp.ServeHTTP(rec, r)

	e.outcome = OutcomeAllowed
	if e.err != nil {
		e.outcome = OutcomeFailed
	}
	e.sent, e.received = body.n, rec.n
	s.log(e)
}

// connect handles CONNECT: the destination is decided, and then either
// inspected or tunnelled.
func (s *Session) connect(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	authority := r.Host
	host, port, err := net.SplitHostPort(authority)
	if err != nil {
		http.Error(w, "kibitz egress: CONNECT needs host:port", http.StatusBadRequest)
		return
	}
	host = normalizeHost(host)
	inspect := s.proxy.ca != nil && !s.proxy.passthrough.contains(host)

	var decision Decision
	if inspect {
		decision = s.proxy.policy.DecideConnect(host, port)
	} else {
		decision = s.proxy.policy.DecideTunnel(host, port)
	}
	if !decision.Allowed {
		refuse(w, decision, host)
		s.log(entry{
			method: http.MethodConnect, scheme: "https", host: host, port: port,
			inspected: inspect, decision: decision, outcome: OutcomeDenied,
			status: http.StatusForbidden, started: started,
		})
		return
	}

	conn, rw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		http.Error(w, "kibitz egress: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if !s.track(conn) {
		return
	}
	defer s.untrack(conn)
	client := &bufferedConn{Conn: conn, r: rw.Reader}

	if inspect {
		s.inspect(client, host, authority, decision)
		return
	}
	s.tunnel(client, host, port, authority, decision, started)
}

const connectEstablished = "HTTP/1.1 200 Connection Established\r\n\r\n"

// tunnel copies bytes both ways without looking at them, and logs one line
// when the connection ends.
func (s *Session) tunnel(client net.Conn, host, port, authority string, decision Decision, started time.Time) {
	e := entry{
		method: http.MethodConnect, scheme: "https", host: host, port: port,
		decision: decision, started: started,
	}
	upstream, err := s.proxy.dial(s.ctx, "tcp", authority)
	if err != nil {
		_, _ = io.WriteString(client, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
		e.outcome, e.status, e.err = OutcomeFailed, http.StatusBadGateway, err
		s.log(e)
		return
	}
	if !s.track(upstream) {
		return
	}
	defer s.untrack(upstream)

	if _, err := io.WriteString(client, connectEstablished); err != nil {
		return
	}
	e.sent, e.received = splice(client, upstream)
	e.outcome, e.status = OutcomeAllowed, http.StatusOK
	s.log(e)
}

// inspect terminates the client's TLS with a certificate for host and serves
// the requests inside, deciding each one.
func (s *Session) inspect(client net.Conn, host, authority string, decision Decision) {
	if _, err := io.WriteString(client, connectEstablished); err != nil {
		return
	}
	conn := tls.Server(client, &tls.Config{
		// The certificate is for the host the client asked to CONNECT to,
		// whatever the handshake names: that is the host the policy decided.
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			return s.proxy.ca.leaf(host, time.Now())
		},
		NextProtos: []string{"http/1.1"},
		MinVersion: tls.VersionTLS12,
	})
	ctx, cancel := context.WithTimeout(s.ctx, 10*time.Second)
	err := conn.HandshakeContext(ctx)
	cancel()
	if err != nil {
		// Almost always a client that does not trust the authority, either
		// because it was not given the file or because it pins. Either way
		// the host belongs in the passthrough list or the client needs the CA.
		s.log(entry{
			method: http.MethodConnect, scheme: "https", host: host, port: portOf(authority),
			inspected: true, decision: decision, outcome: OutcomeFailed,
			err:     fmt.Errorf("TLS handshake with the client failed (does it trust the inspection CA, or pin its certificate?): %w", err),
			started: time.Now(),
		})
		return
	}

	ln := &oneConnListener{conn: conn, done: make(chan struct{})}
	var hijacked atomic.Bool
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// The destination is the CONNECT's, never the Host header's: a
			// connection opened to an allowed host must not carry requests
			// for another one that shares its front end.
			if h, _ := splitAuthority(r.Host, "https"); normalizeHost(h) != host {
				s.log(entry{
					method: r.Method, scheme: "https", host: host, port: portOf(authority),
					path: r.URL.Path, inspected: true, outcome: OutcomeDenied,
					status: http.StatusMisdirectedRequest, started: time.Now(),
					reason: fmt.Sprintf("the Host header names %q, not the host of the CONNECT", r.Host),
				})
				http.Error(w, "kibitz egress: the Host header does not match the CONNECT", http.StatusMisdirectedRequest)
				return
			}
			s.forward(w, r, "https", authority, true)
			// An upgraded connection was taken over by the handler, which is
			// done with it now; the server will never report it closed.
			if hijacked.Load() {
				_ = ln.Close()
			}
		}),
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          slog.NewLogLogger(s.proxy.logger.Handler(), slog.LevelDebug),
		BaseContext:       func(net.Listener) context.Context { return s.ctx },
		ConnState: func(_ net.Conn, state http.ConnState) {
			switch state {
			case http.StateClosed:
				_ = ln.Close()
			case http.StateHijacked:
				// Still in use by the handler, whose return ends it.
				hijacked.Store(true)
			}
		},
	}
	_ = srv.Serve(ln)
}

// refuse answers a denied request.
func refuse(w http.ResponseWriter, d Decision, host string) {
	w.Header().Set("X-Kibitz-Egress", "denied")
	http.Error(w, fmt.Sprintf("kibitz egress: %s is %s", host, d.Reason()), http.StatusForbidden)
}

// splitAuthority splits host[:port], supplying the scheme's port when there is
// none.
func splitAuthority(authority, scheme string) (host, port string) {
	if h, p, err := net.SplitHostPort(authority); err == nil {
		return normalizeHost(h), p
	}
	if scheme == "https" {
		return normalizeHost(authority), "443"
	}
	return normalizeHost(authority), "80"
}

func portOf(authority string) string {
	_, port := splitAuthority(authority, "https")
	return port
}

func queryKeys(r *http.Request) []string {
	if r.URL.RawQuery == "" {
		return nil
	}
	values := r.URL.Query()
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// splice copies both ways until both directions are done, and returns what
// went each way.
func splice(client, upstream net.Conn) (sent, received int64) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		sent, _ = io.Copy(upstream, client)
		closeWrite(upstream)
	}()
	received, _ = io.Copy(client, upstream)
	closeWrite(client)
	<-done
	return sent, received
}

// closeWrite ends one direction, or the whole connection when half-closing is
// not possible.
func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = c.Close()
}

// bufferedConn is a hijacked connection with whatever the server had already
// read from it put back in front.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

func (c *bufferedConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return c.Close()
}

// oneConnListener hands one connection to an http.Server, then waits until it
// is closed so that Serve does not return while the connection is in use.
type oneConnListener struct {
	mu   sync.Mutex
	conn net.Conn
	once sync.Once
	done chan struct{}
}

func (l *oneConnListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	c := l.conn
	l.conn = nil
	l.mu.Unlock()
	if c != nil {
		return c, nil
	}
	<-l.done
	return nil, net.ErrClosed
}

func (l *oneConnListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *oneConnListener) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)} }

// recorder counts what was written to the client.
type recorder struct {
	http.ResponseWriter
	n int64
}

func (r *recorder) Write(b []byte) (int, error) {
	n, err := r.ResponseWriter.Write(b)
	r.n += int64(n)
	return n, err
}

// Unwrap lets http.ResponseController reach Flush and Hijack underneath.
func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// countingReader counts what the client sent.
type countingReader struct {
	io.ReadCloser
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	c.n += int64(n)
	return n, err
}
