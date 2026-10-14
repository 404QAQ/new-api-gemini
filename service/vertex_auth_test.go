package service

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type vertexAuthRoundTripper func(*http.Request) (*http.Response, error)

func (f vertexAuthRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func vertexAuthTestCredentials(t *testing.T) (VertexCredentials, *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	der, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	return VertexCredentials{
		ProjectID: "test-project", PrivateKeyID: "test-key", ClientID: "test-client",
		ClientEmail: "test@test-project.iam.gserviceaccount.com",
		PrivateKey:  string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
	}, key
}

func vertexAuthTestClient(t *testing.T, handler http.HandlerFunc) *http.Client {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	transport := server.Client().Transport
	return &http.Client{Transport: vertexAuthRoundTripper(func(req *http.Request) (*http.Response, error) {
		assert.Equal(t, "https://oauth2.googleapis.com/token", req.URL.String())
		local := req.Clone(req.Context())
		local.URL.Scheme = "https"
		local.URL.Host = strings.TrimPrefix(server.URL, "https://")
		return transport.RoundTrip(local)
	})}
}

func TestVertexAccessTokenCredentialBindingAndJWT(t *testing.T) {
	credentials, key := vertexAuthTestCredentials(t)
	rotated, rotatedKey := vertexAuthTestCredentials(t)
	rotated.PrivateKeyID = credentials.PrivateKeyID // A changed key must invalidate even a stale key ID.
	var calls atomic.Int32
	client := vertexAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "application/x-www-form-urlencoded", r.Header.Get("Content-Type"))
		if !assert.NoError(t, r.ParseForm()) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		assert.Equal(t, "urn:ietf:params:oauth:grant-type:jwt-bearer", r.Form.Get("grant_type"))
		n := calls.Add(1)
		publicKey := &key.PublicKey
		if n == 2 {
			publicKey = &rotatedKey.PublicKey
		}
		token, err := jwt.Parse(r.Form.Get("assertion"), func(token *jwt.Token) (any, error) {
			assert.Equal(t, "test-key", token.Header["kid"])
			return publicKey, nil
		}, jwt.WithValidMethods([]string{"RS256"}), jwt.WithAudience("https://oauth2.googleapis.com/token"), jwt.WithIssuer(credentials.ClientEmail), jwt.WithExpirationRequired())
		if assert.NoError(t, err) {
			claims := token.Claims.(jwt.MapClaims)
			assert.Equal(t, "https://www.googleapis.com/auth/cloud-platform", claims["scope"])
			assert.Equal(t, float64(3600), claims["exp"].(float64)-claims["iat"].(float64))
		}
		fmt.Fprintf(w, `{"access_token":"fake-token-%d","token_type":"Bearer","expires_in":3600}`, n)
	})

	first, err := VertexAccessToken(context.Background(), credentials, client)
	require.NoError(t, err)
	assert.Equal(t, "fake-token-1", first)
	again, err := VertexAccessToken(context.Background(), credentials, client)
	require.NoError(t, err)
	assert.Equal(t, first, again)
	assert.EqualValues(t, 1, calls.Load())

	second, err := VertexAccessToken(context.Background(), rotated, client)
	require.NoError(t, err)
	assert.Equal(t, "fake-token-2", second)
	assert.EqualValues(t, 2, calls.Load())
	// Reordering credentials must still retrieve the original account's token.
	again, err = VertexAccessToken(context.Background(), credentials, client)
	require.NoError(t, err)
	assert.Equal(t, first, again)
	assert.EqualValues(t, 2, calls.Load())

	changedIdentity := credentials
	changedIdentity.ProjectID = "another-project"
	_, err = VertexAccessToken(context.Background(), changedIdentity, client)
	require.NoError(t, err)
	assert.EqualValues(t, 3, calls.Load())
}

func TestVertexAccessTokenRejectsUnsafeResponses(t *testing.T) {
	credentials, _ := vertexAuthTestCredentials(t)
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"http error", 400, `{"error":"secret-upstream-message","access_token":"secret-token","token_type":"Bearer","expires_in":3600}`},
		{"malformed response", 200, `{"access_token":`},
		{"empty token", 200, `{"access_token":"","token_type":"Bearer","expires_in":3600}`},
		{"unsafe token", 200, `{"access_token":"fake\r\ntoken","token_type":"Bearer","expires_in":3600}`},
		{"invalid bearer token", 200, `{"access_token":"fake,token","token_type":"Bearer","expires_in":3600}`},
		{"wrong token type", 200, `{"access_token":"fake","token_type":"MAC","expires_in":3600}`},
		{"missing expiry", 200, `{"access_token":"fake","token_type":"Bearer"}`},
		{"negative expiry", 200, `{"access_token":"fake","token_type":"Bearer","expires_in":-1}`},
		{"oversized response", 200, strings.Repeat(" ", 64*1024+1)},
		{"redirect", 307, "secret-redirect-body"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			client := vertexAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Location", "https://untrusted.example/token")
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			})
			client.CheckRedirect = func(*http.Request, []*http.Request) error {
				t.Error("caller's redirect policy must not be used for OAuth credentials")
				return nil
			}
			token, err := VertexAccessToken(context.Background(), credentials, client)
			require.EqualError(t, err, "Vertex service account authentication failed")
			assert.Empty(t, token)
			assert.EqualValues(t, 1, calls.Load())
		})
	}
}

func TestVertexAccessTokenDoesNotCacheNearExpiry(t *testing.T) {
	credentials, _ := vertexAuthTestCredentials(t)
	var calls atomic.Int32
	client := vertexAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"access_token":"fake-token-%d","token_type":"Bearer","expires_in":30}`, calls.Add(1))
	})
	first, err := VertexAccessToken(context.Background(), credentials, client)
	require.NoError(t, err)
	second, err := VertexAccessToken(context.Background(), credentials, client)
	require.NoError(t, err)
	assert.NotEqual(t, first, second)
	assert.EqualValues(t, 2, calls.Load())
}

func TestVertexAccessTokenCancellationAndSanitizedTransportErrors(t *testing.T) {
	credentials, _ := vertexAuthTestCredentials(t)
	client := &http.Client{Transport: vertexAuthRoundTripper(func(req *http.Request) (*http.Response, error) {
		deadline, ok := req.Context().Deadline()
		assert.True(t, ok)
		assert.LessOrEqual(t, time.Until(deadline), 30*time.Second)
		return nil, errors.New("secret assertion and proxy password")
	})}
	_, err := VertexAccessToken(context.Background(), credentials, client)
	require.EqualError(t, err, "Vertex service account authentication failed")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = VertexAccessToken(ctx, credentials, client)
	require.EqualError(t, err, "Vertex service account authentication canceled")
	_, err = VertexAccessToken(context.Background(), VertexCredentials{PrivateKey: "secret-invalid-key"}, client)
	require.EqualError(t, err, "Invalid Vertex service account credentials")
}

func TestVertexAccessTokenIgnoresCredentialsTokenURI(t *testing.T) {
	credentials, _ := vertexAuthTestCredentials(t)
	raw, err := common.Marshal(map[string]any{
		"project_id": credentials.ProjectID, "client_email": credentials.ClientEmail,
		"private_key": credentials.PrivateKey, "token_uri": "https://untrusted.example/token",
	})
	require.NoError(t, err)
	parsed, err := ParseVertexCredentials(string(raw))
	require.NoError(t, err)
	client := vertexAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"access_token":"fake","token_type":"Bearer","expires_in":3600}`)
	})
	_, err = VertexAccessToken(context.Background(), parsed, client)
	require.NoError(t, err)
}

func TestParseVertexCredentialsRejectsIncompleteOrMalformedInput(t *testing.T) {
	for _, input := range []string{
		`{"project_id":"project","private_key":"private-key-secret"}`,
		`{"client_email":"test@example.com","private_key":"private-key-secret"}`,
		`{"project_id":"project","client_email":"test@example.com","private_key":" "}`,
		`{"private_key":"private-key-secret"`,
		"null",
	} {
		credentials, err := ParseVertexCredentials(input)
		require.EqualError(t, err, "Invalid Vertex service account credentials")
		assert.Empty(t, credentials.PrivateKey)
	}
}

func TestVertexOAuthHTTPClientVerifiesTLSDespiteGlobalAndChannelOverrides(t *testing.T) {
	previousSkip := common.TLSInsecureSkipVerify
	common.TLSInsecureSkipVerify = true
	t.Cleanup(func() { common.TLSInsecureSkipVerify = previousSkip })
	var requests atomic.Int32
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)
	var proxyRequests atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		proxyRequests.Add(1)
		target, err := net.Dial("tcp", r.Host)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		defer target.Close()
		conn, _, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n")
		go func() { _, _ = io.Copy(target, conn) }()
		_, _ = io.Copy(conn, target)
	}))
	t.Cleanup(proxy.Close)

	// Establish that ordinary inference clients still honor the global override.
	relayClient, err := newHTTPClientFromPolicy(defaultHTTPTransportPolicy(), nil, nil)
	require.NoError(t, err)
	t.Cleanup(relayClient.CloseIdleConnections)
	resp, err := relayClient.Get(upstream.URL)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	for _, tc := range []struct {
		name     string
		proxy    string
		settings dto.ChannelSettings
	}{
		{"direct", "", dto.ChannelSettings{TLSInsecureSkipVerify: true}},
		{"http1", "", dto.ChannelSettings{TLSInsecureSkipVerify: true, HTTPProtocol: dto.HTTPProtocolHTTP1}},
		{"sharded", "", dto.ChannelSettings{TLSInsecureSkipVerify: true, HTTP2ConnectionShards: 3}},
		{"proxy", proxy.URL, dto.ChannelSettings{TLSInsecureSkipVerify: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, err := VertexOAuthHTTPClient(tc.proxy, tc.settings)
			require.NoError(t, err)
			t.Cleanup(client.CloseIdleConnections)
			_, err = client.Get(upstream.URL)
			var verifyErr *tls.CertificateVerificationError
			require.ErrorAs(t, err, &verifyErr)
		})
	}
	assert.EqualValues(t, 1, requests.Load(), "OAuth requests must never reach an untrusted TLS server")
	assert.EqualValues(t, 1, proxyRequests.Load(), "OAuth must still use the configured proxy")
	// Dedicated OAuth clients must not change any existing inference client.
	resp, err = relayClient.Get(upstream.URL)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.EqualValues(t, 2, requests.Load())
}
