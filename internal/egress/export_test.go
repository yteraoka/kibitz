package egress

import "crypto/x509"

// WithUpstreamRoots makes the proxy trust a test server's certificate.
func WithUpstreamRoots(cfg Config, pool *x509.CertPool) Config {
	cfg.upstreamRoots = pool
	return cfg
}
