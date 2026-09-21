package github

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// tokenRefreshMargin renews an installation token before it actually expires,
// so a long job cannot start with a token that dies mid-flight.
const tokenRefreshMargin = 5 * time.Minute

// appAuth authenticates as a GitHub App: a short-lived JWT signed with the
// app's private key is exchanged for an installation token, which is what the
// REST API accepts. kibitz never uses a personal access token
// (see docs/security.md).
type appAuth struct {
	appID          int64
	installationID int64
	key            *rsa.PrivateKey
	baseURL        string
	httpClient     *http.Client
	now            func() time.Time

	mu      sync.Mutex
	token   string
	expires time.Time
}

// parsePrivateKey accepts the PEM GitHub hands out (PKCS#1) as well as PKCS#8,
// since key material often makes a detour through a secret manager that
// re-encodes it.
func parsePrivateKey(pemData string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemData))
	if block == nil {
		return nil, fmt.Errorf("private key is not PEM encoded")
	}

	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parsing private key: %w", err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("private key is %T, want an RSA key", parsed)
	}
	return key, nil
}

// jwt mints the app-level token. GitHub rejects a lifetime over ten minutes
// and clocks that run fast, hence the backdated issue time.
func (a *appAuth) jwt() (string, error) {
	now := a.now()
	header := map[string]string{"alg": "RS256", "typ": "JWT"}
	claims := map[string]any{
		"iat": now.Add(-30 * time.Second).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": a.appID,
	}

	headerJSON, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}

	enc := base64.RawURLEncoding
	signingInput := enc.EncodeToString(headerJSON) + "." + enc.EncodeToString(claimsJSON)

	digest := sha256.Sum256([]byte(signingInput))
	signature, err := rsa.SignPKCS1v15(rand.Reader, a.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("signing app token: %w", err)
	}
	return signingInput + "." + enc.EncodeToString(signature), nil
}

// installationToken returns a cached token, refreshing it when it is close to
// expiry.
func (a *appAuth) installationToken(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.token != "" && a.now().Before(a.expires.Add(-tokenRefreshMargin)) {
		return a.token, nil
	}

	appToken, err := a.jwt()
	if err != nil {
		return "", err
	}

	url := fmt.Sprintf("%s/app/installations/%d/access_tokens", a.baseURL, a.installationID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+appToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", apiVersion)

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("requesting installation token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("requesting installation token: %w", errorFromResponse(resp))
	}

	var body struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("decoding installation token: %w", err)
	}
	if body.Token == "" {
		return "", fmt.Errorf("installation token response carried no token")
	}

	a.token, a.expires = body.Token, body.ExpiresAt
	return a.token, nil
}
