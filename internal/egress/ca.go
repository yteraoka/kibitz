package egress

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"sync"
	"time"
)

// authority is the certificate authority TLS inspection signs with.
//
// It is made when the worker starts and lives only in its memory. The private
// key is never written anywhere, and the certificate is trusted only by the
// agent processes the worker starts: nothing outside one worker instance can
// be fooled by it, and a restart makes a new one.
type authority struct {
	cert *x509.Certificate
	key  crypto.Signer
	pem  []byte

	mu     sync.Mutex
	leaves map[string]*tls.Certificate
}

// Lifetimes. The authority outlives any instance of the worker, and a leaf is
// replaced a day before it would expire.
const (
	authorityLifetime = 365 * 24 * time.Hour
	leafLifetime      = 7 * 24 * time.Hour
	leafRenewBefore   = 24 * time.Hour
	// maxLeaves bounds the cache. With no allow list the agent can name any
	// host it likes, and each one would otherwise stay in memory for good.
	maxLeaves = 1024
)

func newAuthority(now time.Time) (*authority, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generating the inspection CA key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			Organization: []string{"kibitz"},
			// The serial tells one instance's authority from another's in a
			// client's error message.
			CommonName: "kibitz egress inspection CA " + serial.Text(16)[:8],
		},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(authorityLifetime),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("signing the inspection CA: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &authority{
		cert:   cert,
		key:    key,
		pem:    pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		leaves: make(map[string]*tls.Certificate),
	}, nil
}

// leaf returns a certificate for host signed by the authority, from the cache
// when there is a fresh one.
func (a *authority) leaf(host string, now time.Time) (*tls.Certificate, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if c, ok := a.leaves[host]; ok && now.Add(leafRenewBefore).Before(c.Leaf.NotAfter) {
		return c, nil
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	notAfter := now.Add(leafLifetime)
	if notAfter.After(a.cert.NotAfter) {
		notAfter = a.cert.NotAfter
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if ip := net.ParseIP(host); ip != nil {
		template.IPAddresses = []net.IP{ip}
	} else {
		template.DNSNames = []string{host}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, a.cert, &key.PublicKey, a.key)
	if err != nil {
		return nil, fmt.Errorf("signing a certificate for %s: %w", host, err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	c := &tls.Certificate{
		Certificate: [][]byte{der, a.cert.Raw},
		PrivateKey:  key,
		Leaf:        parsed,
	}
	if len(a.leaves) >= maxLeaves {
		clear(a.leaves)
	}
	a.leaves[host] = c
	return c, nil
}

func randomSerial() (*big.Int, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("generating a serial number: %w", err)
	}
	// Never zero, and never so short that the common name cannot take eight
	// hex digits from it.
	return serial.SetBit(serial, 127, 1), nil
}
