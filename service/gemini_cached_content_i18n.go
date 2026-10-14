package service

import (
	"strings"

	"github.com/QuantumNous/new-api/i18n"
	"github.com/gin-gonic/gin"
)

var geminiCacheErrorKeys = map[string]string{
	"Unable to verify cached content ownership":                                    i18n.MsgGeminiCacheOwnershipUnavailable,
	"Cached content request exceeds the size limit":                                i18n.MsgGeminiCacheRequestSizeLimit,
	"Cached content channel configuration is unavailable or has changed":           i18n.MsgGeminiCacheChannelChanged,
	"Invalid cached content response from upstream":                                i18n.MsgGeminiCacheInvalidUpstreamResponse,
	"JSON nesting exceeds the supported limit":                                     i18n.MsgGeminiCacheJSONNestingLimit,
	"Duplicate JSON fields are not supported":                                      i18n.MsgGeminiCacheDuplicateJSONFields,
	"Expected a valid JSON object within the request size limit":                   i18n.MsgGeminiCacheInvalidJSONSize,
	"Expected a JSON object":                                                       i18n.MsgGeminiCacheExpectedJSONObject,
	"Invalid JSON object":                                                          i18n.MsgGeminiCacheInvalidJSONObject,
	"Specify exactly one of ttl or expireTime":                                     i18n.MsgGeminiCacheExclusiveExpiration,
	"Specify ttl or expireTime":                                                    i18n.MsgGeminiCacheExpirationRequired,
	"ttl must be a positive duration in seconds ending in s":                       i18n.MsgGeminiCacheInvalidTTL,
	"ttl must be between 1 and 86400 seconds":                                      i18n.MsgGeminiCacheTTLRange,
	"expireTime must be an RFC3339 timestamp":                                      i18n.MsgGeminiCacheExpirationFormat,
	"expireTime must be between 1 and 86400 seconds in the future":                 i18n.MsgGeminiCacheExpirationRange,
	"Rounded expiration must not exceed 86400 seconds":                             i18n.MsgGeminiCacheRoundedExpirationRange,
	"Unsupported or server-managed cached content field":                           i18n.MsgGeminiCacheUnsupportedField,
	"model is required":                                                            i18n.MsgGeminiCacheModelRequired,
	"Invalid model name":                                                           i18n.MsgGeminiCacheInvalidModel,
	"contents must be a non-empty array":                                           i18n.MsgGeminiCacheContentsArray,
	"Each content must contain a non-empty parts array":                            i18n.MsgGeminiCachePartsArray,
	"systemInstruction must be an object":                                          i18n.MsgGeminiCacheSystemInstructionObject,
	"toolConfig must be an object":                                                 i18n.MsgGeminiCacheToolConfigObject,
	"tools must be an array":                                                       i18n.MsgGeminiCacheToolsArray,
	"contents or systemInstruction is required":                                    i18n.MsgGeminiCacheContentRequired,
	"displayName must be a string of at most 128 characters":                       i18n.MsgGeminiCacheDisplayName,
	"Only ttl or expireTime may be updated":                                        i18n.MsgGeminiCacheExpirationUpdateOnly,
	"Use generateContentRequest instead of generate_content_request":               i18n.MsgGeminiCacheGenerateRequestAlias,
	"generateContentRequest must be an object":                                     i18n.MsgGeminiCacheGenerateRequestObject,
	"Use cachedContent instead of cached_content":                                  i18n.MsgGeminiCacheCachedContentAlias,
	"cachedContent must be a resource name":                                        i18n.MsgGeminiCacheResourceName,
	"Conflicting cachedContent references":                                         i18n.MsgGeminiCacheReferenceConflict,
	"cachedContent cannot be combined with systemInstruction, tools or toolConfig": i18n.MsgGeminiCacheImmutableFields,
	"Unable to encode cached content request":                                      i18n.MsgGeminiCacheEncodeFailed,
	"Cached content upstream request timed out":                                    i18n.MsgGeminiCacheUpstreamTimeout,
	"Cached content upstream request failed":                                       i18n.MsgGeminiCacheUpstreamFailed,
	"Invalid cached content request":                                               i18n.MsgGeminiCacheInvalidRequest,
	"Invalid cached content update":                                                i18n.MsgGeminiCacheInvalidUpdate,
	"cached content model is required":                                             i18n.MsgGeminiCacheBillingModelRequired,
	"invalid cached content billing operation":                                     i18n.MsgGeminiCacheBillingOperation,
	"invalid cached content billing group ratio":                                   i18n.MsgGeminiCacheBillingGroupRatio,
	"invalid quota conversion rate":                                                i18n.MsgGeminiCacheQuotaConversion,
	"cached content billing duration must be between zero and 24 hours":            i18n.MsgGeminiCacheBillingDuration,
	"explicit cache price must be finite and non-negative":                         i18n.MsgGeminiCacheFinitePrice,
	"cached content billing quote is required":                                     i18n.MsgGeminiCacheQuoteRequired,
	"cached content billing session already exists":                                i18n.MsgGeminiCacheBillingSessionExists,
	"cached content billing session is closed":                                     i18n.MsgGeminiCacheBillingSessionClosed,
	"Cached content operation could not be completed":                              i18n.MsgGeminiCacheOperationFailed,
	"Insufficient quota or access denied for cached content":                       i18n.MsgGeminiCacheQuotaOrAccessDenied,
	"Unable to read cached content request":                                        i18n.MsgGeminiCacheReadFailed,
	"No eligible Gemini explicit cache channel":                                    i18n.MsgGeminiCacheNoEligibleChannel,
	"CachedContent not found (or permission denied)":                               i18n.MsgGeminiCacheNotFound,
	"pageSize must be between 1 and 1000":                                          i18n.MsgGeminiCachePageSize,
	"Invalid pageToken":                                                            i18n.MsgGeminiCacheInvalidPageToken,
	"Cached content channel is unavailable":                                        i18n.MsgGeminiCacheChannelUnavailable,
	"Another cached content operation is in progress":                              i18n.MsgGeminiCacheOperationInProgress,
	"Cached content token count changed unexpectedly":                              i18n.MsgGeminiCacheTokenCountChanged,
	"This token has no access to the cached content model":                         i18n.MsgGeminiCacheModelAccessDenied,
	"Cached content group is not accessible":                                       i18n.MsgGeminiCacheGroupAccessDenied,
	"Cached content channel is not available for this model and group":             i18n.MsgGeminiCacheChannelModelGroup,
	"Cached content conflicts with the requested channel":                          i18n.MsgGeminiCacheChannelConflict,
	"Unable to read Gemini request":                                                i18n.MsgGeminiCacheReadGeminiFailed,
	"Cached content requires generateContent or streamGenerateContent":             i18n.MsgGeminiCacheGenerateOnly,
	"Cached content request could not be completed":                                i18n.MsgGeminiCacheRelayFailed,
	"Cached content request rejected by upstream":                                  i18n.MsgGeminiCacheRelayRejected,
}

// LocalizeGeminiCacheError translates cache errors while preserving diagnostic values.
func LocalizeGeminiCacheError(c *gin.Context, message string) string {
	if model, ok := strings.CutPrefix(message, "explicit cache pricing is not configured for "); ok {
		return i18n.T(c, i18n.MsgGeminiCachePricingNotConfigured, map[string]any{"Model": model})
	}
	if detail, ok := strings.CutPrefix(message, "invalid explicit cache pricing: "); ok {
		return i18n.T(c, i18n.MsgGeminiCacheInvalidPricing, map[string]any{"Error": detail})
	}
	if limit, ok := strings.CutPrefix(message, "cached content token count must be between 1 and "); ok {
		return i18n.T(c, i18n.MsgGeminiCacheTokenCountRange, map[string]any{"Max": limit})
	}
	if detail, ok := strings.CutPrefix(message, "explicit cache price evaluation failed: "); ok {
		return i18n.T(c, i18n.MsgGeminiCachePriceEvaluation, map[string]any{"Error": detail})
	}
	if status, ok := strings.CutPrefix(message, "Cached content upstream request rejected ("); ok {
		if status, ok := strings.CutSuffix(status, ")"); ok {
			return i18n.T(c, i18n.MsgGeminiCacheUpstreamRejected, map[string]any{"Status": status})
		}
	}
	if key, ok := geminiCacheErrorKeys[message]; ok {
		return i18n.T(c, key)
	}
	return message
}
