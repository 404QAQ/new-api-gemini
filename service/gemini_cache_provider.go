package service

import (
	"context"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/dto"
)

var (
	geminiCacheVertexProjectPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	geminiCacheVertexNamePattern    = regexp.MustCompile(`^projects/([A-Za-z0-9_-]{1,128})/locations/([a-z][a-z0-9-]{0,62})/cachedContents/[A-Za-z0-9_-]{1,256}$`)
	geminiCacheVertexModelPattern   = regexp.MustCompile(`^projects/([A-Za-z0-9_-]{1,128})/locations/([a-z][a-z0-9-]{0,62})/publishers/google/models/([A-Za-z0-9][A-Za-z0-9._-]{0,199})$`)
)

// GeminiCacheProvider resolves the account and endpoint used for every cache
// operation. BindingURL is persisted as an identity, never used as a request URL.
type GeminiCacheProvider struct {
	BindingURL   string
	apiRoot      string
	resourceRoot string
	modelRoot    string
	key          string
	authHeader   string
	vertex       bool
	region       string
	credentials  VertexCredentials
	settings     dto.ChannelSettings
}

func GeminiCacheProviderForChannel(channel *model.Channel, key, localModel string) (*GeminiCacheProvider, error) {
	config, supported := model.GeminiCacheChannelConfigForModel(channel, localModel)
	if !supported {
		return nil, geminiCacheUnavailable()
	}
	base := config.BaseURL
	provider := &GeminiCacheProvider{BindingURL: base, apiRoot: base + "/v1beta", key: key, authHeader: config.AuthHeader, settings: channel.GetSetting()}
	if channel.Type == constant.ChannelTypeVertexAi {
		credentials, err := ParseVertexCredentials(key)
		if err != nil || !geminiCacheVertexProjectPattern.MatchString(credentials.ProjectID) {
			return nil, geminiCacheUnavailable()
		}
		region := config.Region
		if base == "" {
			base = "https://" + region + "-aiplatform.googleapis.com"
		}
		if !strings.HasSuffix(base, "/v1") {
			base += "/v1"
		}
		provider.apiRoot = base
		provider.resourceRoot = "projects/" + credentials.ProjectID + "/locations/" + region + "/"
		provider.modelRoot = provider.resourceRoot + "publishers/google/models/"
		provider.BindingURL = base + "/" + strings.TrimSuffix(provider.resourceRoot, "/") + "#gemini-cache:vertex-oauth"
		provider.vertex, provider.region, provider.credentials = true, region, credentials
	} else {
		if key == "" || strings.ContainsAny(key, "\r\n") {
			return nil, geminiCacheUnavailable()
		}
		provider.modelRoot = "models/"
		if channel.Type != constant.ChannelTypeGemini {
			// Channel type and auth changes must invalidate a gateway binding,
			// even when its URL and key happen to remain identical.
			provider.BindingURL = base + "#gemini-cache:" + strconv.Itoa(channel.Type) + ":" + strings.ToLower(provider.authHeader)
		}
	}
	if len(provider.BindingURL) > 512 {
		return nil, geminiCacheUnavailable()
	}
	return provider, nil
}

func (p *GeminiCacheProvider) GenerationURL(upstreamModel string, stream bool) string {
	if p == nil || !geminiCacheModelPattern.MatchString(upstreamModel) {
		return ""
	}
	action := ":generateContent"
	if stream {
		action = ":streamGenerateContent?alt=sse"
	}
	return p.apiRoot + "/" + p.modelRoot + upstreamModel + action
}

func (p *GeminiCacheProvider) SetupHeader(ctx context.Context, header *http.Header) error {
	if p == nil || header == nil {
		return geminiCacheUnavailable()
	}
	if *header == nil {
		*header = make(http.Header)
	}
	header.Del("Authorization")
	header.Del("x-goog-api-key")
	header.Del("x-goog-user-project")
	if p.vertex {
		settings := p.settings
		client, err := VertexOAuthHTTPClient(settings.Proxy, settings)
		if err != nil {
			return geminiCacheUnavailable()
		}
		defer client.CloseIdleConnections()
		token, err := VertexAccessToken(ctx, p.credentials, client)
		if err != nil {
			return &GeminiCacheError{HTTPStatus: http.StatusBadGateway, Status: "UNAVAILABLE", Message: "Cached content upstream authentication failed"}
		}
		header.Set("Authorization", "Bearer "+token)
		header.Set("x-goog-user-project", p.credentials.ProjectID)
		return nil
	}
	value := p.key
	if p.authHeader == "Authorization" {
		value = "Bearer " + value
	}
	header.Set(p.authHeader, value)
	return nil
}

func (p *GeminiCacheProvider) validResourceName(name string) bool {
	if !p.vertex {
		return geminiCacheUpstreamNamePattern.MatchString(name)
	}
	parts := geminiCacheVertexNamePattern.FindStringSubmatch(name)
	// Google may canonicalize a project ID into its project number. The
	// selected credential and bound regional endpoint remain authoritative.
	return len(parts) == 3 && parts[2] == p.region && len(name) <= 512
}

func (p *GeminiCacheProvider) validResourceModel(name, upstreamModel string) bool {
	if !p.vertex {
		return strings.TrimPrefix(name, "models/") == upstreamModel
	}
	parts := geminiCacheVertexModelPattern.FindStringSubmatch(name)
	return len(parts) == 4 && parts[2] == p.region && parts[3] == upstreamModel
}
