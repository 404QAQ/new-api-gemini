package i18n

// Message keys of i18n/locales/*.yaml, for text the backend translates itself:
// errors returned to AI clients, emails and notifications. Web console messages
// are not listed here; the web console translates them (see common.Message).

// Errors returned to AI clients
const (
	MsgDatabaseError          = "common.database_error"
	MsgAuthUserBanned         = "auth.user_banned"
	MsgTokenGetInfoFailed     = "token.get_info_failed"
	MsgTokenInvalid           = "token.invalid"
	MsgTokenNotProvided       = "token.not_provided"
	MsgTokenStatusUnavailable = "token.status_unavailable"

	MsgAuthClientIPUnresolved     = "auth.client_ip_unresolved"
	MsgAuthTokenIPNotAllowed      = "auth.token_ip_not_allowed"
	MsgAuthTokenGroupForbidden    = "auth.token_group_forbidden"
	MsgAuthTokenGroupDeprecated   = "auth.token_group_deprecated"
	MsgAuthChannelSelectForbidden = "auth.channel_select_forbidden"

	MsgRateLimitReached      = "rate_limit.reached"
	MsgRateLimitTotalReached = "rate_limit.total_reached"

	MsgDistributorInvalidRequest               = "distributor.invalid_request"
	MsgDistributorInvalidChannelId             = "distributor.invalid_channel_id"
	MsgDistributorChannelDisabled              = "distributor.channel_disabled"
	MsgDistributorTokenNoModelAccess           = "distributor.token_no_model_access"
	MsgDistributorTokenModelForbidden          = "distributor.token_model_forbidden"
	MsgDistributorModelNameRequired            = "distributor.model_name_required"
	MsgDistributorInvalidPlayground            = "distributor.invalid_playground_request"
	MsgDistributorGroupAccessDenied            = "distributor.group_access_denied"
	MsgDistributorGetChannelFailed             = "distributor.get_channel_failed"
	MsgDistributorNoAvailableChannel           = "distributor.no_available_channel"
	MsgDistributorNoAvailableChannelTaskPlugin = "distributor.no_available_channel_task_plugin"
	MsgDistributorInvalidParseModel            = "distributor.invalid_request_parse_model"

	MsgQuotaUserInsufficient             = "quota.user_insufficient"
	MsgQuotaPreConsumeInsufficient       = "quota.pre_consume_insufficient"
	MsgQuotaSubscriptionInsufficient     = "quota.subscription_insufficient"
	MsgRelayModelPriceNotConfigured      = "relay.model_price_not_configured"
	MsgRelayModelPriceNotConfiguredAdmin = "relay.model_price_not_configured_admin"
	MsgRelayTaskChannelDisabled          = "relay.task_channel_disabled"
	MsgRelayGroupSaturated               = "relay.group_saturated"
	MsgRelayGroupUpstreamSaturated       = "relay.group_upstream_saturated"
	MsgRelayRetryGetChannelFailed        = "relay.retry_get_channel_failed"
	MsgRelayRetryNoAvailableChannel      = "relay.retry_no_available_channel"
	MsgRelayAccessTokenUnsupported       = "relay.access_token_unsupported"
)

// Emails and the name of the token created at registration, in the
// recipient's language
const (
	MsgUserDefaultTokenName = "user.default_token_name"

	MsgEmailVerificationSubject  = "email.verification.subject"
	MsgEmailVerificationGreeting = "email.verification.greeting"
	MsgEmailVerificationCode     = "email.verification.code"
	MsgEmailVerificationValidity = "email.verification.validity"

	MsgEmailPasswordResetSubject      = "email.password_reset.subject"
	MsgEmailPasswordResetGreeting     = "email.password_reset.greeting"
	MsgEmailPasswordResetLink         = "email.password_reset.link"
	MsgEmailPasswordResetLinkFallback = "email.password_reset.link_fallback"
	MsgEmailPasswordResetValidity     = "email.password_reset.validity"
)

// Low-quota notices, in the recipient's saved language
const (
	MsgNotifyQuotaLowTitle             = "notify.quota_low_title"
	MsgNotifySubscriptionQuotaLowTitle = "notify.subscription_quota_low_title"
	MsgNotifyQuotaLowBark              = "notify.quota_low_bark"
	MsgNotifyQuotaLowGotify            = "notify.quota_low_gotify"
	MsgNotifyQuotaLowEmail             = "notify.quota_low_email"
)

// Channel notices sent to the root user and model update watchers
const (
	MsgChannelNotifyDisabledSubject = "channel.notify_disabled_subject"
	MsgChannelNotifyDisabledContent = "channel.notify_disabled_content"
	MsgChannelNotifyEnabledSubject  = "channel.notify_enabled_subject"
	MsgChannelNotifyEnabledContent  = "channel.notify_enabled_content"
	MsgChannelTestCompletedSubject  = "channel.test_completed_subject"
	MsgChannelTestCompletedContent  = "channel.test_completed_content"
	MsgChannelResponseTimeExceeded  = "channel.response_time_exceeded"

	MsgChannelUpstreamUpdateNotifySubject   = "channel.upstream_update_notify_subject"
	MsgChannelUpstreamUpdateSummary         = "channel.upstream_update_summary"
	MsgChannelUpstreamUpdateChangedChannels = "channel.upstream_update_changed_channels"
	MsgChannelUpstreamUpdateMoreChannels    = "channel.upstream_update_more_channels"
	MsgChannelUpstreamUpdateAddedModels     = "channel.upstream_update_added_models"
	MsgChannelUpstreamUpdateRemovedModels   = "channel.upstream_update_removed_models"
	MsgChannelUpstreamUpdateFailedChannels  = "channel.upstream_update_failed_channels"
	MsgChannelUpstreamUpdateMoreOmitted     = "channel.upstream_update_more_omitted"
)

// Gemini explicit cache errors returned to AI clients
const (
	MsgGeminiCacheRequestSizeLimit        = "gemini_cache.request_size_limit"
	MsgGeminiCacheChannelChanged          = "gemini_cache.channel_changed"
	MsgGeminiCacheInvalidUpstreamResponse = "gemini_cache.invalid_upstream_response"
	MsgGeminiCacheJSONNestingLimit        = "gemini_cache.json_nesting_limit"
	MsgGeminiCacheDuplicateJSONFields     = "gemini_cache.duplicate_json_fields"
	MsgGeminiCacheInvalidJSONSize         = "gemini_cache.invalid_json_size"
	MsgGeminiCacheExpectedJSONObject      = "gemini_cache.expected_json_object"
	MsgGeminiCacheInvalidJSONObject       = "gemini_cache.invalid_json_object"
	MsgGeminiCacheExclusiveExpiration     = "gemini_cache.exclusive_expiration"
	MsgGeminiCacheExpirationRequired      = "gemini_cache.expiration_required"
	MsgGeminiCacheInvalidTTL              = "gemini_cache.invalid_ttl"
	MsgGeminiCacheTTLRange                = "gemini_cache.ttl_range"
	MsgGeminiCacheExpirationFormat        = "gemini_cache.expiration_format"
	MsgGeminiCacheExpirationRange         = "gemini_cache.expiration_range"
	MsgGeminiCacheRoundedExpirationRange  = "gemini_cache.rounded_expiration_range"
	MsgGeminiCacheUnsupportedField        = "gemini_cache.unsupported_field"
	MsgGeminiCacheModelRequired           = "gemini_cache.model_required"
	MsgGeminiCacheInvalidModel            = "gemini_cache.invalid_model"
	MsgGeminiCacheContentsArray           = "gemini_cache.contents_array"
	MsgGeminiCachePartsArray              = "gemini_cache.parts_array"
	MsgGeminiCacheSystemInstructionObject = "gemini_cache.system_instruction_object"
	MsgGeminiCacheToolConfigObject        = "gemini_cache.tool_config_object"
	MsgGeminiCacheToolsArray              = "gemini_cache.tools_array"
	MsgGeminiCacheContentRequired         = "gemini_cache.content_required"
	MsgGeminiCacheDisplayName             = "gemini_cache.display_name"
	MsgGeminiCacheExpirationUpdateOnly    = "gemini_cache.expiration_update_only"
	MsgGeminiCacheGenerateRequestAlias    = "gemini_cache.generate_request_alias"
	MsgGeminiCacheGenerateRequestObject   = "gemini_cache.generate_request_object"
	MsgGeminiCacheCachedContentAlias      = "gemini_cache.cached_content_alias"
	MsgGeminiCacheResourceName            = "gemini_cache.resource_name"
	MsgGeminiCacheReferenceConflict       = "gemini_cache.reference_conflict"
	MsgGeminiCacheImmutableFields         = "gemini_cache.immutable_fields"
	MsgGeminiCacheEncodeFailed            = "gemini_cache.encode_failed"
	MsgGeminiCacheUpstreamTimeout         = "gemini_cache.upstream_timeout"
	MsgGeminiCacheUpstreamFailed          = "gemini_cache.upstream_failed"
	MsgGeminiCacheInvalidRequest          = "gemini_cache.invalid_request"
	MsgGeminiCacheInvalidUpdate           = "gemini_cache.invalid_update"
	MsgGeminiCacheBillingModelRequired    = "gemini_cache.billing_model_required"
	MsgGeminiCacheBillingOperation        = "gemini_cache.billing_operation"
	MsgGeminiCacheBillingGroupRatio       = "gemini_cache.billing_group_ratio"
	MsgGeminiCacheQuotaConversion         = "gemini_cache.quota_conversion"
	MsgGeminiCacheBillingDuration         = "gemini_cache.billing_duration"
	MsgGeminiCacheFinitePrice             = "gemini_cache.finite_price"
	MsgGeminiCacheQuoteRequired           = "gemini_cache.quote_required"
	MsgGeminiCacheBillingSessionExists    = "gemini_cache.billing_session_exists"
	MsgGeminiCacheBillingSessionClosed    = "gemini_cache.billing_session_closed"
	MsgGeminiCacheOperationFailed         = "gemini_cache.operation_failed"
	MsgGeminiCacheQuotaOrAccessDenied     = "gemini_cache.quota_or_access_denied"
	MsgGeminiCacheReadFailed              = "gemini_cache.read_failed"
	MsgGeminiCacheNoEligibleChannel       = "gemini_cache.no_eligible_channel"
	MsgGeminiCacheNotFound                = "gemini_cache.not_found"
	MsgGeminiCachePageSize                = "gemini_cache.page_size"
	MsgGeminiCacheInvalidPageToken        = "gemini_cache.invalid_page_token"
	MsgGeminiCacheChannelUnavailable      = "gemini_cache.channel_unavailable"
	MsgGeminiCacheOperationInProgress     = "gemini_cache.operation_in_progress"
	MsgGeminiCacheTokenCountChanged       = "gemini_cache.token_count_changed"
	MsgGeminiCacheModelAccessDenied       = "gemini_cache.model_access_denied"
	MsgGeminiCacheGroupAccessDenied       = "gemini_cache.group_access_denied"
	MsgGeminiCacheChannelModelGroup       = "gemini_cache.channel_model_group"
	MsgGeminiCacheChannelConflict         = "gemini_cache.channel_conflict"
	MsgGeminiCacheReadGeminiFailed        = "gemini_cache.read_gemini_failed"
	MsgGeminiCacheGenerateOnly            = "gemini_cache.generate_only"
	MsgGeminiCacheRelayFailed             = "gemini_cache.relay_failed"
	MsgGeminiCacheRelayRejected           = "gemini_cache.relay_rejected"
	MsgGeminiCachePricingNotConfigured    = "gemini_cache.pricing_not_configured"
	MsgGeminiCacheInvalidPricing          = "gemini_cache.invalid_pricing"
	MsgGeminiCacheTokenCountRange         = "gemini_cache.token_count_range"
	MsgGeminiCachePriceEvaluation         = "gemini_cache.price_evaluation"
	MsgGeminiCacheUpstreamRejected        = "gemini_cache.upstream_rejected"
	MsgGeminiCacheOwnershipUnavailable    = "gemini_cache.ownership_unavailable"
)
