package service

import (
	"context"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/golang-jwt/jwt/v5"
	"github.com/samber/hot"
)

const vertexOAuthTokenURL = "https://oauth2.googleapis.com/token"

// VertexCredentials contains the service-account identity and signing material.
// token_uri is intentionally unsupported: credentials cannot select the OAuth endpoint.
type VertexCredentials struct {
	ProjectID    string `json:"project_id"`
	PrivateKeyID string `json:"private_key_id"`
	PrivateKey   string `json:"private_key"`
	ClientEmail  string `json:"client_email"`
	ClientID     string `json:"client_id"`
}

// ParseVertexCredentials validates service-account fields without exposing the
// JSON or signing material through decoder errors.
func ParseVertexCredentials(key string) (VertexCredentials, error) {
	var credentials VertexCredentials
	if len(key) > 64*1024 || common.UnmarshalJsonStr(key, &credentials) != nil ||
		strings.TrimSpace(credentials.ProjectID) == "" || strings.TrimSpace(credentials.PrivateKey) == "" ||
		strings.TrimSpace(credentials.ClientEmail) == "" {
		return VertexCredentials{}, errors.New("Invalid Vertex service account credentials")
	}
	return credentials, nil
}

// Tokens stay in bounded process memory; neither credentials nor access tokens
// are written to Redis, database rows, logs, or cache keys.
var vertexAccessTokens = hot.NewHotCache[string, string](hot.LRU, 4096).
	WithTTL(time.Hour).
	WithJanitor().
	Build()

// VertexOAuthHTTPClient uses an independent transport that always verifies TLS,
// regardless of inference-channel or global skip-verification settings. The
// caller owns this client and must close its idle connections after the exchange.
func VertexOAuthHTTPClient(proxy string, settings dto.ChannelSettings) (*http.Client, error) {
	proxyURL, _, err := common.ParseProxyURLRuntime(strings.TrimSpace(proxy))
	if err != nil {
		return nil, errors.New("Vertex service account authentication failed")
	}
	policy := NormalizeHTTPTransportPolicy(settings)
	policy.TLSInsecureSkipVerify = false
	// A non-nil config overrides the global TLS config in every transport shard.
	client, err := newHTTPClientFromPolicy(policy, proxyURL, &tls.Config{MinVersion: tls.VersionTLS12})
	if err != nil {
		return nil, errors.New("Vertex service account authentication failed")
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	client.Timeout = 30 * time.Second
	return client, nil
}

// VertexAccessToken exchanges a service-account JWT at Google's fixed endpoint.
// The caller supplies a client that verifies TLS certificates, with the channel's
// proxy and transport settings. Redirects and credential-controlled URLs are never used.
func VertexAccessToken(ctx context.Context, credentials VertexCredentials, client *http.Client) (string, error) {
	if ctx.Err() != nil {
		return "", errors.New("Vertex service account authentication canceled")
	}
	if strings.TrimSpace(credentials.ClientEmail) == "" || credentials.PrivateKey == "" || len(credentials.PrivateKey) > 64*1024 {
		return "", errors.New("Invalid Vertex service account credentials")
	}
	identity, err := common.Marshal(credentials)
	if err != nil {
		return "", errors.New("Invalid Vertex service account credentials")
	}
	fingerprint := sha256.Sum256(identity)
	cacheKey := hex.EncodeToString(fingerprint[:])
	if token, found, _ := vertexAccessTokens.Get(cacheKey); found {
		return token, nil
	}

	keyPEM := strings.ReplaceAll(strings.TrimSpace(credentials.PrivateKey), `\n`, "\n")
	if !strings.HasPrefix(keyPEM, "-----BEGIN ") {
		keyPEM = "-----BEGIN PRIVATE KEY-----\n" + keyPEM + "\n-----END PRIVATE KEY-----"
	}
	block, rest := pem.Decode([]byte(keyPEM))
	if block == nil || strings.TrimSpace(string(rest)) != "" {
		return "", errors.New("Invalid Vertex service account credentials")
	}
	var signingKey *rsa.PrivateKey
	switch block.Type {
	case "PRIVATE KEY":
		parsed, parseErr := x509.ParsePKCS8PrivateKey(block.Bytes)
		if parseErr == nil {
			signingKey, _ = parsed.(*rsa.PrivateKey)
		}
	case "RSA PRIVATE KEY":
		signingKey, _ = x509.ParsePKCS1PrivateKey(block.Bytes)
	}
	if signingKey == nil || signingKey.N.BitLen() < 2048 {
		return "", errors.New("Invalid Vertex service account credentials")
	}
	now := time.Now()
	assertion := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"iss": credentials.ClientEmail, "scope": "https://www.googleapis.com/auth/cloud-platform",
		"aud": vertexOAuthTokenURL, "iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
	})
	if credentials.PrivateKeyID != "" {
		assertion.Header["kid"] = credentials.PrivateKeyID
	}
	signedJWT, err := assertion.SignedString(signingKey)
	if err != nil {
		return "", errors.New("Invalid Vertex service account credentials")
	}
	form := url.Values{
		"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"},
		"assertion":  {signedJWT},
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, vertexOAuthTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", errors.New("Vertex service account authentication failed")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if client == nil {
		client = http.DefaultClient
	}
	authClient := *client
	authClient.Jar = nil
	authClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := authClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", errors.New("Vertex service account authentication canceled")
		}
		return "", errors.New("Vertex service account authentication failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", errors.New("Vertex service account authentication failed")
	}
	const responseLimit = 64 * 1024
	body, err := io.ReadAll(io.LimitReader(resp.Body, responseLimit+1))
	if err != nil || len(body) > responseLimit {
		return "", errors.New("Vertex service account authentication failed")
	}
	var result struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if common.Unmarshal(body, &result) != nil || result.AccessToken == "" || !strings.EqualFold(result.TokenType, "Bearer") || result.ExpiresIn <= 0 {
		return "", errors.New("Vertex service account authentication failed")
	}
	for _, char := range result.AccessToken {
		if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || strings.ContainsRune("-._~+/=", char)) {
			return "", errors.New("Vertex service account authentication failed")
		}
	}
	expiresAt := now.Add(time.Duration(min(result.ExpiresIn, 3600)) * time.Second)
	if !time.Now().Before(expiresAt) {
		return "", errors.New("Vertex service account authentication failed")
	}
	// Use the request start time so network latency cannot extend token lifetime.
	// Short-lived tokens are returned once, but never cached beyond their expiry.
	ttl := time.Until(expiresAt.Add(-time.Minute))
	if ttl > 0 {
		vertexAccessTokens.SetWithTTL(cacheKey, result.AccessToken, ttl)
	}
	return result.AccessToken, nil
}
