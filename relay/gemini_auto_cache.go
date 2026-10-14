package relay

import (
	"bytes"
	"errors"
	"io"
	"math"
	"net/http"
	"slices"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay/channel"
	"github.com/QuantumNous/new-api/relay/channel/gemini"
	"github.com/QuantumNous/new-api/relay/channel/vertex"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/model_setting"
	"github.com/gin-gonic/gin"
)

// doRequestWithGeminiAutoCache keeps the ordinary outbound request untouched
// until a usable managed cache exists. The cache binding belongs only to this
// attempt; retries on another channel start again with the original request.
func doRequestWithGeminiAutoCache(c *gin.Context, info *relaycommon.RelayInfo, original channel.Adaptor, body io.Reader) (any, channel.Adaptor, error) {
	if !info.ChannelSetting.GeminiAutoCacheEnabled || info.IsChannelTest ||
		(info.RelayMode != relayconstant.RelayModeChatCompletions && info.RelayMode != relayconstant.RelayModeGemini) ||
		model_setting.GetGlobalSettings().PassThroughRequestEnabled || info.ChannelSetting.PassThroughBodyEnabled ||
		info.ChannelSetting.SystemPrompt != "" || len(info.ParamOverride) != 0 || len(info.HeadersOverride) != 0 ||
		info.UseRuntimeHeadersOverride || len(info.RuntimeHeadersOverride) != 0 {
		response, err := original.DoRequest(c, info, body)
		return response, original, err
	}
	if record, bound := common.GetContextKeyType[*model.GeminiCachedContent](c, constant.ContextKeyGeminiCachedContent); bound && record != nil {
		response, err := original.DoRequest(c, info, body)
		return response, original, err
	}
	channelConfig, err := model.GetChannelById(info.ChannelId, true)
	if err != nil || channelConfig == nil || !channelConfig.GetSetting().GeminiAutoCacheEnabled {
		response, requestErr := original.DoRequest(c, info, body)
		return response, original, requestErr
	}
	if _, err := service.GeminiCacheProviderForChannel(channelConfig, info.ApiKey, info.OriginModelName); err != nil {
		response, requestErr := original.DoRequest(c, info, body)
		return response, original, requestErr
	}

	fullBody, err := io.ReadAll(io.LimitReader(body, service.GeminiCacheMaxRequestBytes+1))
	if err != nil {
		return nil, original, err
	}
	if len(fullBody) > service.GeminiCacheMaxRequestBytes {
		response, requestErr := original.DoRequest(c, info, io.MultiReader(bytes.NewReader(fullBody), body))
		return response, original, requestErr
	}
	// Both the regular request and the fallback use the same final body,
	// including the original adaptor's model/conversion decisions.
	fallbackBody, closer, err := relaycommon.NewOutboundJSONBody(fullBody)
	if err != nil {
		return nil, original, err
	}
	defer closer.Close()
	if service.HasGeminiCacheReference(fullBody) || service.HasOpenAIGeminiCacheReference(fullBody) {
		response, requestErr := original.DoRequest(c, info, fallbackBody)
		return response, original, requestErr
	}
	nativeJSON, candidateInfo, err := geminiAutoCacheCandidate(c, info, fullBody)
	if err != nil {
		response, requestErr := original.DoRequest(c, info, fallbackBody)
		return response, original, requestErr
	}
	prepared, err := service.PrepareGeminiAutoCache(c, info, channelConfig, nativeJSON)
	if err != nil || prepared == nil || prepared.Record == nil {
		response, requestErr := original.DoRequest(c, info, fallbackBody)
		return response, original, requestErr
	}

	previousModel, previousDisablePing := info.UpstreamModelName, info.DisablePing
	info.UpstreamModelName = strings.TrimPrefix(info.UpstreamModelName, "models/")
	common.SetContextKey(c, constant.ContextKeyGeminiCachedContent, prepared.Record)
	keepNative := false
	defer func() {
		common.SetContextKey(c, constant.ContextKeyGeminiCachedContent, nil)
		if !keepNative {
			info.UpstreamModelName, info.DisablePing = previousModel, previousDisablePing
		}
	}()
	cacheAdaptor := &geminiCacheAdaptor{automatic: true}
	response, requestErr := cacheAdaptor.DoRequest(c, info, bytes.NewReader(prepared.Body))
	if requestErr != nil && !cacheAdaptor.requestStarted && !c.Writer.Written() {
		common.SetContextKey(c, constant.ContextKeyGeminiCachedContent, nil)
		info.UpstreamModelName, info.DisablePing = previousModel, previousDisablePing
		response, requestErr = original.DoRequest(c, info, fallbackBody)
		return response, original, requestErr
	}
	if requestErr != nil {
		return nil, cacheAdaptor, types.NewErrorWithStatusCode(errors.New(service.LocalizeGeminiCacheError(c, "Cached content request could not be completed")), types.ErrorCodeDoRequestFailed, http.StatusBadGateway, types.ErrOptionWithSkipRetry())
	}
	httpResponse, ok := response.(*http.Response)
	if !ok || httpResponse == nil {
		return nil, cacheAdaptor, types.NewErrorWithStatusCode(errors.New(service.LocalizeGeminiCacheError(c, "Cached content request could not be completed")), types.ErrorCodeBadResponse, http.StatusBadGateway, types.ErrOptionWithSkipRetry())
	}
	if httpResponse.StatusCode != http.StatusOK {
		if !c.Writer.Written() && geminiAutoCacheReferenceRejected(httpResponse) {
			_ = httpResponse.Body.Close()
			service.InvalidateGeminiAutoCache(prepared)
			common.SetContextKey(c, constant.ContextKeyGeminiCachedContent, nil)
			info.UpstreamModelName, info.DisablePing = previousModel, previousDisablePing
			response, requestErr = original.DoRequest(c, info, fallbackBody)
			return response, original, requestErr
		}
		_ = httpResponse.Body.Close()
		status := httpResponse.StatusCode
		if status == http.StatusUnauthorized || status == http.StatusNotFound {
			status = http.StatusForbidden
		}
		return nil, cacheAdaptor, types.NewErrorWithStatusCode(errors.New(service.LocalizeGeminiCacheError(c, "Cached content request rejected by upstream")), types.ErrorCodeBadResponseStatusCode, status, types.ErrOptionWithSkipRetry())
	}
	keepNative = true
	info.AppendRequestConversion(types.RelayFormatGemini)
	info.FinalRequestRelayFormat = types.RelayFormatGemini
	if candidateInfo != nil {
		info.ReasoningEffort = candidateInfo.ReasoningEffort
		info.RecordConversionDiagnostics(c, candidateInfo.ConversionDiagnostics())
	}
	return response, cacheAdaptor, nil
}

// geminiAutoCacheCandidate isolates converter mutations from the ordinary
// request and its billing snapshot. A failed optimization must not change the
// generation request that will be sent by the original adaptor.
func geminiAutoCacheCandidate(c *gin.Context, info *relaycommon.RelayInfo, fullBody []byte) ([]byte, *relaycommon.RelayInfo, error) {
	if info.GetFinalRequestRelayFormat() == types.RelayFormatGemini {
		return fullBody, nil, nil
	}
	if info.GetFinalRequestRelayFormat() != types.RelayFormatOpenAI {
		return nil, nil, errors.New("automatic caching requires Gemini or OpenAI Chat")
	}
	meta, err := common.DeepCopy(info.ChannelMeta)
	if err != nil {
		return nil, nil, err
	}
	var reasoning *dto.ReasoningConversionState
	if info.ReasoningConversion != nil {
		reasoning, err = common.DeepCopy(info.ReasoningConversion)
		if err != nil {
			return nil, nil, err
		}
	}
	candidateInfo := &relaycommon.RelayInfo{
		OriginModelName: info.OriginModelName, RelayFormat: info.RelayFormat, RelayMode: info.RelayMode,
		ChannelMeta: meta, IsStream: info.IsStream, ReasoningEffort: info.ReasoningEffort, ReasoningConversion: reasoning,
	}
	candidateInfo.ChannelOtherSettings.ToolLossPolicy = string(types.ConversionLossPolicyStrict)
	candidateInfo.SetEstimatePromptTokens(info.GetEstimatePromptTokens())
	candidateInfo.UpstreamModelName = strings.TrimPrefix(info.UpstreamModelName, "models/")
	candidateContext := c.Copy()
	candidateContext.Request = c.Request.Clone(c.Request.Context())
	// Media conversion can allocate temporary files. Track only the candidate's
	// new sources here, then give them to the original request's cleanup. Shared
	// cached files must not be closed while the fallback still needs them.
	common.SetContextKey(candidateContext, constant.ContextKeyFileSourcesToCleanup, []types.FileSource{})
	defer func() {
		sources, _ := common.GetContextKeyType[[]types.FileSource](candidateContext, constant.ContextKeyFileSourcesToCleanup)
		if len(sources) != 0 {
			originalSources, _ := common.GetContextKeyType[[]types.FileSource](c, constant.ContextKeyFileSourcesToCleanup)
			common.SetContextKey(c, constant.ContextKeyFileSourcesToCleanup, append(originalSources, sources...))
		}
	}()
	converter := channel.Adaptor(&gemini.Adaptor{})
	if info.ChannelType == constant.ChannelTypeVertexAi {
		converter = &vertex.Adaptor{RequestMode: vertex.RequestModeGemini}
	}
	var converted any
	if request, native := info.Request.(*dto.GeminiChatRequest); native {
		copyRequest, copyErr := common.DeepCopy(request)
		if copyErr != nil {
			return nil, nil, copyErr
		}
		candidateInfo.Request = copyRequest
		converted, err = converter.ConvertGeminiRequest(candidateContext, candidateInfo, copyRequest)
	} else {
		if !geminiAutoCacheChatConvertible(fullBody) {
			return nil, nil, errors.New("automatic caching would change the OpenAI Chat request")
		}
		request := &dto.GeneralOpenAIRequest{}
		if err := common.Unmarshal(fullBody, request); err != nil {
			return nil, nil, err
		}
		request.SetModelName(candidateInfo.UpstreamModelName)
		candidateInfo.Request = request
		converted, err = converter.ConvertOpenAIRequest(candidateContext, candidateInfo, request)
	}
	if err != nil {
		return nil, nil, err
	}
	if len(candidateInfo.ConversionDiagnostics()) != 0 {
		return nil, nil, errors.New("automatic caching would lose request semantics")
	}
	if _, ok := converted.(*dto.GeminiChatRequest); !ok {
		return nil, nil, errors.New("automatic caching requires a native Gemini request")
	}
	encoded, err := common.Marshal(converted)
	return encoded, candidateInfo, err
}

// geminiAutoCacheChatConvertible is deliberately conservative for gateways
// whose ordinary transport is OpenAI Chat. Their provider-specific options
// must keep that transport instead of disappearing in an optional conversion.
// Requests already converted to Gemini follow their existing native behavior.
func geminiAutoCacheChatConvertible(body []byte) bool {
	var fields map[string]common.RawMessage
	if common.Unmarshal(body, &fields) != nil {
		return false
	}
	for field := range fields {
		switch field {
		case "model", "messages", "stream", "stream_options", "temperature", "top_p", "max_tokens", "max_completion_tokens", "seed", "stop", "tools", "tool_choice", "reasoning_effort":
		default:
			return false
		}
	}
	var request dto.GeneralOpenAIRequest
	if common.Unmarshal(body, &request) != nil || (request.MaxTokens != nil && request.MaxCompletionTokens != nil) {
		return false
	}
	if request.Seed != nil && (math.Trunc(*request.Seed) != *request.Seed || *request.Seed < -9_007_199_254_740_991 || *request.Seed > 9_007_199_254_740_991) {
		return false
	}
	if raw, exists := fields["stream_options"]; exists && string(raw) != "null" {
		var options map[string]common.RawMessage
		if common.Unmarshal(raw, &options) != nil {
			return false
		}
		for field := range options {
			if field != "include_usage" {
				return false
			}
		}
	}
	if raw, exists := fields["stop"]; exists && string(raw) != "null" {
		var stop string
		if common.Unmarshal(raw, &stop) == nil {
			if stop == "" {
				return false
			}
		} else {
			var stops []string
			if common.Unmarshal(raw, &stops) != nil || len(stops) > 5 || slices.Contains(stops, "") {
				return false
			}
		}
	}
	var messages []map[string]common.RawMessage
	if common.Unmarshal(fields["messages"], &messages) != nil {
		return false
	}
	for _, message := range messages {
		for field := range message {
			if !slices.Contains([]string{"role", "content", "name", "tool_calls", "tool_call_id"}, field) {
				return false
			}
		}
		var role string
		if common.Unmarshal(message["role"], &role) != nil || !slices.Contains([]string{"system", "developer", "user", "assistant", "tool"}, role) {
			return false
		}
		if _, named := message["name"]; named && role != "tool" {
			return false
		}
		content := message["content"]
		if len(content) == 0 || string(content) == "null" {
			continue
		}
		var text string
		if common.Unmarshal(content, &text) == nil {
			if strings.Contains(text, "](data:") {
				return false
			}
			continue
		}
		if role == "system" || role == "developer" || role == "tool" {
			return false
		}
		var parts []map[string]common.RawMessage
		if common.Unmarshal(content, &parts) != nil {
			return false
		}
		for _, part := range parts {
			var kind string
			if len(part) != 2 || common.Unmarshal(part["type"], &kind) != nil || kind != "text" || common.Unmarshal(part["text"], &text) != nil {
				return false
			}
			if strings.Contains(text, "](data:") {
				return false
			}
		}
	}
	return true
}

// geminiAutoCacheReferenceRejected permits a single full-request retry only
// for an explicit cache-reference rejection, never an ambiguous network/5xx
// failure or a generic invalid generation parameter.
func geminiAutoCacheReferenceRejected(response *http.Response) bool {
	if response == nil || response.Body == nil || (response.StatusCode != http.StatusBadRequest && response.StatusCode != http.StatusNotFound) {
		return false
	}
	const maxErrorBytes = 64 << 10
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxErrorBytes+1))
	if err != nil || len(raw) > maxErrorBytes {
		return false
	}
	var envelope struct {
		Error struct {
			Status  string `json:"status"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if common.Unmarshal(raw, &envelope) != nil {
		return false
	}
	message := strings.ToLower(envelope.Error.Message)
	mentionsCache := strings.Contains(message, "cachedcontent") || strings.Contains(message, "cached content") || strings.Contains(message, "cached_content")
	if !mentionsCache || (envelope.Error.Status != "INVALID_ARGUMENT" && envelope.Error.Status != "NOT_FOUND" && envelope.Error.Status != "FAILED_PRECONDITION") {
		return false
	}
	return strings.Contains(message, "expired") || strings.Contains(message, "not found") || strings.Contains(message, "does not exist") || strings.Contains(message, "invalid") || strings.Contains(message, "deleted")
}
