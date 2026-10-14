package service

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// GeminiAutoCacheResult contains an attempt-local optimization. The caller must
// retain its complete original body for fallback and must not mutate the client
// request or its generation billing snapshot. Key is a digest, never an API key.
type GeminiAutoCacheResult struct {
	Record *model.GeminiCachedContent
	Body   []byte
	Key    string
	slot   *model.GeminiAutoCacheSlot
}

type geminiAutoCachePrefix struct {
	Key    string
	Length int
}

// Gemini cached contents are immutable prompt prefixes, not separate messages:
// https://ai.google.dev/gemini-api/docs/generate-content/caching
// Split only before a real user turn. Function responses stay in the suffix
// with their call; never split a part, tool interaction or thought signature.
func geminiAutoCachePrefixes(raw, scope []byte) (map[string]common.RawMessage, []common.RawMessage, []geminiAutoCachePrefix, error) {
	if HasGeminiCacheReference(raw) {
		return nil, nil, nil, nil
	}
	fields, err := decodeGeminiCacheObject(raw)
	if err != nil {
		return nil, nil, nil, err
	}
	for field := range fields {
		for _, canonical := range []string{"contents", "systemInstruction", "tools", "toolConfig"} {
			if field != canonical && (strings.EqualFold(field, canonical) || field == "system_instruction" || field == "tool_config") {
				return nil, nil, nil, geminiCacheInvalid("Invalid cached content request")
			}
		}
	}
	var contents []common.RawMessage
	if common.Unmarshal(fields["contents"], &contents) != nil || len(contents) == 0 || len(contents) > 256 {
		return nil, nil, nil, nil
	}
	immutable := make(map[string]common.RawMessage)
	for _, field := range []string{"systemInstruction", "tools", "toolConfig"} {
		if value, exists := fields[field]; exists {
			immutable[field] = value
		}
	}
	encoded, err := common.Marshal(immutable)
	if err != nil {
		return nil, nil, nil, err
	}
	digest := sha256.New()
	digest.Write(scope)
	digest.Write([]byte{'\n'})
	digest.Write(encoded)
	digest.Write([]byte{'\n'})
	var prefixes []geminiAutoCachePrefix
	for i, content := range contents {
		value := gjson.ParseBytes(content)
		parts := value.Get("parts")
		role := value.Get("role").String()
		if !value.IsObject() || !parts.IsArray() || len(parts.Array()) == 0 || (role != "" && role != "user" && role != "model") {
			return nil, nil, nil, nil
		}
		userTurn := role == "user" || role == ""
		for _, part := range parts.Array() {
			// Remote objects (notably gs:// URIs) can change without changing
			// their URI. Automatic caching must not freeze an unseen revision.
			if part.Get("fileData").Exists() || part.Get("file_data").Exists() {
				return nil, nil, nil, nil
			}
			if part.Get("functionResponse").Exists() || part.Get("functionCall").Exists() {
				userTurn = false
			}
		}
		if userTurn && (i > 0 || fields["systemInstruction"] != nil) {
			prefixes = append(prefixes, geminiAutoCachePrefix{Key: hex.EncodeToString(digest.Sum(nil)), Length: i})
		}
		encoded, err := common.Marshal(content)
		if err != nil {
			return nil, nil, nil, err
		}
		digest.Write(encoded)
		digest.Write([]byte{'\n'})
	}
	return fields, contents, prefixes, nil
}

func geminiAutoCacheCreateRequest(fields map[string]common.RawMessage, contents []common.RawMessage, length int, modelName string, ttl int) (*GeminiCacheCreateRequest, error) {
	body := make(map[string]common.RawMessage)
	for _, field := range []string{"systemInstruction", "tools", "toolConfig"} {
		if value, exists := fields[field]; exists {
			body[field] = value
		}
	}
	if length > 0 {
		body["contents"], _ = common.Marshal(contents[:length])
	}
	body["model"], _ = common.Marshal(modelName)
	body["ttl"], _ = common.Marshal(fmt.Sprintf("%ds", ttl))
	body["displayName"] = common.RawMessage(`"new-api automatic cache"`)
	raw, err := common.Marshal(body)
	if err != nil {
		return nil, err
	}
	return ParseGeminiCacheCreate(raw, time.Now().UTC().Truncate(time.Second))
}

func geminiAutoCacheReferenceBody(fields map[string]common.RawMessage, contents []common.RawMessage, length int, name string) ([]byte, error) {
	body := maps.Clone(fields)
	for _, field := range []string{"systemInstruction", "tools", "toolConfig"} {
		delete(body, field)
	}
	body["contents"], _ = common.Marshal(contents[length:])
	body["cachedContent"], _ = common.Marshal(name)
	return common.Marshal(body)
}

// PrepareGeminiAutoCache never changes info, c, or nativeJSON. An unavailable
// optimization returns nil/error so the relay can send the original request.
func PrepareGeminiAutoCache(c *gin.Context, info *relaycommon.RelayInfo, channel *model.Channel, nativeJSON []byte) (*GeminiAutoCacheResult, error) {
	if info == nil || channel == nil || info.UserId <= 0 || info.TokenId <= 0 ||
		info.UserId != c.GetInt("id") || info.TokenId != c.GetInt("token_id") || info.ChannelId != channel.Id ||
		len(info.ParamOverride) != 0 || len(info.HeadersOverride) != 0 || info.ChannelSetting.SystemPrompt != "" {
		return nil, nil
	}
	var settings dto.ChannelSettings
	if channel.Setting == nil || common.UnmarshalJsonStr(*channel.Setting, &settings) != nil || !settings.GeminiAutoCacheEnabled || settings.PassThroughBodyEnabled {
		return nil, nil
	}
	minimum, ttl := dto.GeminiAutoCacheDefaultMinTokens, dto.GeminiAutoCacheDefaultTTLSeconds
	if settings.GeminiAutoCacheMinTokens != nil {
		minimum = *settings.GeminiAutoCacheMinTokens
	}
	if settings.GeminiAutoCacheTTLSeconds != nil {
		ttl = *settings.GeminiAutoCacheTTLSeconds
	}
	if settings.ValidateGeminiAutoCache() != nil {
		return nil, nil
	}
	provider, err := GeminiCacheProviderForChannel(channel, info.ApiKey, info.OriginModelName)
	if err != nil {
		return nil, nil
	}
	upstreamModel, err := GeminiCacheUpstreamModel(channel, info.OriginModelName)
	if err != nil || upstreamModel != strings.TrimPrefix(info.UpstreamModelName, "models/") {
		return nil, nil
	}
	binding := &model.GeminiCachedContent{UserID: info.UserId, TokenID: info.TokenId, Group: info.UsingGroup,
		ChannelID: channel.Id, KeyFingerprint: GeminiCacheKeyFingerprint(info.ApiKey), BaseURL: provider.BindingURL,
		Model: info.OriginModelName, UpstreamModel: upstreamModel}
	if _, _, err := GeminiCacheBinding(channel, binding); err != nil {
		return nil, nil
	}
	scope, err := common.Marshal([]any{"gemini-auto-v1", binding.UserID, binding.TokenID, binding.Group, binding.ChannelID,
		channel.Type, binding.KeyFingerprint, binding.BaseURL, binding.Model, binding.UpstreamModel})
	if err != nil {
		return nil, err
	}
	fields, contents, prefixes, err := geminiAutoCachePrefixes(nativeJSON, scope)
	if err != nil || len(prefixes) == 0 {
		return nil, err
	}
	keys := make([]string, len(prefixes))
	for i := range prefixes {
		keys[i] = prefixes[i].Key
	}
	slots, err := model.FindGeminiAutoCacheSlots(keys)
	if err != nil {
		return nil, err
	}
	byKey := make(map[string]*model.GeminiAutoCacheSlot, len(slots))
	publicIDs := make([]string, 0, len(slots))
	for i := range slots {
		byKey[slots[i].Key] = &slots[i]
		if slots[i].PublicID != "" {
			publicIDs = append(publicIDs, slots[i].PublicID)
		}
	}
	records, err := model.FindGeminiCachedContentsByPublicIDs(binding.UserID, binding.TokenID, publicIDs)
	if err != nil {
		return nil, err
	}
	byPublicID := make(map[string]*model.GeminiCachedContent, len(records))
	for i := range records {
		byPublicID[records[i].PublicID] = &records[i]
	}
	now, blocked := time.Now().Unix(), false
	for i := len(prefixes) - 1; i >= 0; i-- {
		prefix := prefixes[i]
		slot := byKey[prefix.Key]
		if slot == nil {
			continue
		}
		if slot.PublicID != "" {
			record := byPublicID[slot.PublicID]
			if record != nil && record.Group == binding.Group && record.ChannelID == binding.ChannelID && record.KeyFingerprint == binding.KeyFingerprint &&
				record.BaseURL == binding.BaseURL && record.Model == binding.Model && record.UpstreamModel == binding.UpstreamModel && record.ExpiresAt > now+5 {
				if _, _, err := GeminiCacheBinding(channel, record); err == nil {
					body, err := geminiAutoCacheReferenceBody(fields, contents, prefix.Length, "cachedContents/"+record.PublicID)
					return &GeminiAutoCacheResult{Record: record, Body: body, Key: prefix.Key, slot: slot}, err
				}
			}
		}
		// A pending creation, cooldown or uncertain operation on an older prefix
		// also blocks creating a larger paid prefix while the dialog grows.
		blocked = blocked || slot.HoldUntil > now
	}
	if blocked || c.Request.Context().Err() != nil {
		return nil, nil
	}
	// The relay's local estimate covers the complete generation prompt, so an
	// estimate below the cache threshold cannot make the reusable prefix reach
	// that threshold. Treat an under-estimate as a missed optimization rather
	// than trusting it for billing; unknown estimates still use countTokens.
	if estimate := info.GetEstimatePromptTokens(); estimate > 0 && estimate < minimum {
		return nil, ErrGeminiCacheBelowMinimumTokens
	}
	// Missing prices are a configuration issue, not a per-prefix failure.
	// Avoid creating durable cooldown rows for every unpriced conversation.
	if _, err := QuoteGeminiCacheBilling(binding.Model, 1, time.Duration(ttl)*time.Second, GeminiCacheCreate, GetUserGroupRatio(c.GetString("user_group"), info.UsingGroup)); err != nil {
		return nil, err
	}
	prefix := prefixes[len(prefixes)-1]
	request, err := geminiAutoCacheCreateRequest(fields, contents, prefix.Length, binding.Model, ttl)
	if err != nil {
		return nil, err
	}
	// A crash must not enable another paid creation before the first resource's
	// requested absolute expiry and bookkeeping timeout. No process-only lock.
	slot, claimed, err := model.ClaimGeminiAutoCacheSlot(prefix.Key, now, request.ExpiresAt.Add(8*time.Minute).Unix())
	if err != nil || !claimed {
		return nil, err
	}
	record, err := CreateManagedGeminiCachedContent(c, channel, info.ApiKey, info.UsingGroup, request, minimum)
	if err != nil {
		var reconciliation *GeminiCacheCreateReconciliationError
		if errors.As(err, &reconciliation) {
			// An untrusted/malformed upstream response may not confirm expiry.
			// Retain the reservation and fence for the maximum supported lifetime.
			_, _ = model.FinishGeminiAutoCacheSlot(slot, "", time.Now().Add(GeminiCacheMaxTTL+8*time.Minute).Unix())
		} else {
			_, _ = model.FinishGeminiAutoCacheSlot(slot, "", time.Now().Add(time.Minute).Unix())
		}
		return nil, err
	}
	finished, err := model.FinishGeminiAutoCacheSlot(slot, record.PublicID, record.ExpiresAt)
	if err != nil || !finished {
		return nil, err
	}
	body, err := geminiAutoCacheReferenceBody(fields, contents, prefix.Length, "cachedContents/"+record.PublicID)
	return &GeminiAutoCacheResult{Record: record, Body: body, Key: prefix.Key, slot: slot}, err
}

// InvalidateGeminiAutoCache only stops automatic reuse. Paid resources remain
// visible through the explicit management API, with their original accounting.
func InvalidateGeminiAutoCache(result *GeminiAutoCacheResult) {
	if result == nil || result.slot == nil {
		return
	}
	_, _ = model.FinishGeminiAutoCacheSlot(result.slot, "", time.Now().Add(time.Minute).Unix())
}
