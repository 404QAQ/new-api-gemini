package relay

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay/channel"
	"github.com/QuantumNous/new-api/relay/channel/gemini"
	"github.com/QuantumNous/new-api/relay/channel/vertex"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/sjson"
)

// Managed caches always use the native Gemini transport, including when the
// caller speaks OpenAI Chat or the channel normally uses another converter.
// The provider owns the endpoint and credentials used by both CRUD and reuse.
type geminiCacheAdaptor struct {
	gemini.Adaptor
	provider *service.GeminiCacheProvider
}

func (a *geminiCacheAdaptor) Init(info *relaycommon.RelayInfo) {
	// Cache creation accepts Google's models/ prefix in channel mappings.
	// Use the same canonical name for the native URL and final binding check.
	info.UpstreamModelName = strings.TrimPrefix(info.UpstreamModelName, "models/")
}

func (a *geminiCacheAdaptor) ConvertOpenAIRequest(c *gin.Context, info *relaycommon.RelayInfo, request *dto.GeneralOpenAIRequest) (any, error) {
	request.SetModelName(info.UpstreamModelName)
	var converted any
	var err error
	if info.ChannelType == constant.ChannelTypeVertexAi {
		converter := vertex.Adaptor{RequestMode: vertex.RequestModeGemini}
		converted, err = converter.ConvertOpenAIRequest(c, info, request)
	} else {
		converted, err = a.Adaptor.ConvertOpenAIRequest(c, info, request)
	}
	if err != nil {
		return nil, err
	}
	geminiRequest, ok := converted.(*dto.GeminiChatRequest)
	record, bound := common.GetContextKeyType[*model.GeminiCachedContent](c, constant.ContextKeyGeminiCachedContent)
	if !ok || !bound || record == nil {
		return nil, errors.New("Invalid cached content request")
	}
	geminiRequest.CachedContent = "cachedContents/" + record.PublicID
	return geminiRequest, nil
}

func (a *geminiCacheAdaptor) ConvertGeminiRequest(c *gin.Context, info *relaycommon.RelayInfo, request *dto.GeminiChatRequest) (any, error) {
	request.SetModelName(info.UpstreamModelName)
	if info.ChannelType == constant.ChannelTypeVertexAi {
		converter := vertex.Adaptor{RequestMode: vertex.RequestModeGemini}
		return converter.ConvertGeminiRequest(c, info, request)
	}
	return a.Adaptor.ConvertGeminiRequest(c, info, request)
}

func (a *geminiCacheAdaptor) GetRequestURL(info *relaycommon.RelayInfo) (string, error) {
	if a.provider == nil {
		return "", errors.New("Cached content channel is unavailable")
	}
	if info.IsStream {
		info.DisablePing = true
	}
	return a.provider.GenerationURL(info.UpstreamModelName, info.IsStream), nil
}

func (a *geminiCacheAdaptor) SetupRequestHeader(c *gin.Context, header *http.Header, info *relaycommon.RelayInfo) error {
	channel.SetupApiRequestHeader(info, c, header)
	header.Set("Content-Type", "application/json")
	return a.provider.SetupHeader(c.Request.Context(), header)
}

func (a *geminiCacheAdaptor) DoRequest(c *gin.Context, info *relaycommon.RelayInfo, body io.Reader) (any, error) {
	record, bound := common.GetContextKeyType[*model.GeminiCachedContent](c, constant.ContextKeyGeminiCachedContent)
	if !bound || record == nil || info.ChannelId != record.ChannelID || info.UpstreamModelName != record.UpstreamModel ||
		service.GeminiCacheKeyFingerprint(info.ApiKey) != record.KeyFingerprint || record.ExpiresAt <= time.Now().Unix() ||
		len(info.ParamOverride) != 0 || len(info.HeadersOverride) != 0 || info.ChannelSetting.SystemPrompt != "" {
		return nil, errors.New("CachedContent not found (or permission denied)")
	}
	// Recheck live configuration after conversion, including affinity overrides
	// and credentials removed while the request was being prepared.
	channelConfig, err := model.GetChannelById(record.ChannelID, true)
	if err != nil {
		return nil, errors.New("Cached content channel is unavailable")
	}
	if _, _, err = service.GeminiCacheBinding(channelConfig, record); err != nil {
		return nil, err
	}
	if _, err = model.GetGeminiCachedContent(record.UserID, record.TokenID, record.PublicID); err != nil {
		return nil, errors.New("CachedContent not found (or permission denied)")
	}
	a.provider, err = service.GeminiCacheProviderForChannel(channelConfig, info.ApiKey, record.Model)
	if err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(io.LimitReader(body, service.GeminiCacheMaxRequestBytes+1))
	if err != nil {
		return nil, err
	}
	name, err := service.ValidateGeminiCacheReference(raw)
	if err != nil {
		return nil, err
	}
	if name != "cachedContents/"+record.PublicID {
		return nil, errors.New("CachedContent not found (or permission denied)")
	}
	raw, err = sjson.SetBytes(raw, "cachedContent", record.UpstreamName)
	if err != nil {
		return nil, err
	}
	outbound, closer, err := relaycommon.NewOutboundJSONBody(raw)
	if err != nil {
		return nil, err
	}
	defer closer.Close()
	return channel.DoApiRequest(a, c, info, outbound)
}
