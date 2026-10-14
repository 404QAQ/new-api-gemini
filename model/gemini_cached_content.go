package model

import (
	"errors"
	"net/url"
	"regexp"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
)

const (
	GeminiCachedContentPending  = "pending"
	GeminiCachedContentActive   = "active"
	GeminiCachedContentMutating = "mutating"
	GeminiCachedContentDeleted  = "deleted"
)

var ErrGeminiCachedContentInvalid = errors.New("invalid Gemini cached content binding")

// GeminiCachedContent binds an opaque public resource to its owner and exact
// upstream credential. Content and plaintext API keys are never stored here.
type GeminiCachedContent struct {
	ID                int64  `json:"-" gorm:"primaryKey"`
	PublicID          string `json:"-" gorm:"type:varchar(64);not null;uniqueIndex"`
	UserID            int    `json:"-" gorm:"not null;index:idx_gemini_cache_owner,priority:1"`
	TokenID           int    `json:"-" gorm:"not null;index:idx_gemini_cache_owner,priority:2"`
	ChannelID         int    `json:"-" gorm:"not null"`
	Group             string `json:"-" gorm:"type:varchar(64);not null;index:idx_gemini_cache_owner,priority:3"`
	KeyFingerprint    string `json:"-" gorm:"type:varchar(64);not null"`
	BaseURL           string `json:"-" gorm:"type:varchar(512);not null"`
	Model             string `json:"-" gorm:"type:varchar(255);not null"`
	UpstreamModel     string `json:"-" gorm:"type:varchar(255);not null"`
	UpstreamName      string `json:"-" gorm:"type:varchar(512);not null"`
	DisplayName       string `json:"-" gorm:"type:varchar(512);not null"`
	TotalTokens       int    `json:"-" gorm:"not null"`
	CreatedAt         int64  `json:"-" gorm:"autoCreateTime"`
	UpdatedAt         int64  `json:"-" gorm:"autoUpdateTime"`
	ExpiresAt         int64  `json:"-" gorm:"not null;index"`
	PaidUntil         int64  `json:"-" gorm:"not null"`
	State             string `json:"-" gorm:"type:varchar(32);not null;index:idx_gemini_cache_owner,priority:4"`
	MutationToken     string `json:"-" gorm:"type:varchar(64);not null"`
	MutationExpiresAt int64  `json:"-" gorm:"not null"`
}

func CreateGeminiCachedContent(record *GeminiCachedContent) error {
	if record == nil || record.ID != 0 || record.UserID <= 0 || record.TokenID <= 0 || record.ChannelID <= 0 ||
		record.PublicID == "" || len(record.PublicID) > 64 || len(record.Group) > 64 ||
		len(record.KeyFingerprint) != 64 || record.BaseURL == "" || len(record.BaseURL) > 512 ||
		record.Model == "" || len(record.Model) > 255 || record.UpstreamModel == "" || len(record.UpstreamModel) > 255 ||
		len(record.UpstreamName) > 512 || len(record.DisplayName) > 512 || record.TotalTokens < 0 ||
		(record.State != "" && record.State != GeminiCachedContentPending) {
		return ErrGeminiCachedContentInvalid
	}
	record.State = GeminiCachedContentPending
	record.MutationToken = ""
	record.MutationExpiresAt = 0
	record.PaidUntil = record.ExpiresAt
	return DB.Create(record).Error
}

func GetGeminiCachedContent(userID, tokenID int, publicID string) (*GeminiCachedContent, error) {
	var record GeminiCachedContent
	err := DB.Where("user_id = ? AND token_id = ? AND public_id = ? AND state = ? AND expires_at > ?",
		userID, tokenID, publicID, GeminiCachedContentActive, common.GetTimestamp()).First(&record).Error
	if err != nil {
		return nil, err
	}
	return &record, nil
}

// FindGeminiCachedContentsByPublicIDs loads reusable cache bindings in one
// query. Prefix matching may check up to 256 candidate IDs for one owner.
func FindGeminiCachedContentsByPublicIDs(userID, tokenID int, publicIDs []string) ([]GeminiCachedContent, error) {
	if userID <= 0 || tokenID <= 0 || len(publicIDs) > 256 {
		return nil, ErrGeminiCachedContentInvalid
	}
	if len(publicIDs) == 0 {
		return []GeminiCachedContent{}, nil
	}
	records := make([]GeminiCachedContent, 0, len(publicIDs))
	err := DB.Where("user_id = ? AND token_id = ? AND public_id IN ? AND state = ? AND expires_at > ?",
		userID, tokenID, publicIDs, GeminiCachedContentActive, common.GetTimestamp()).Find(&records).Error
	return records, err
}

// GetGeminiCachedContentForRecovery includes pending and uncertain mutations so
// callers can reconcile upstream state without exposing the resource for reuse.
func GetGeminiCachedContentForRecovery(userID, tokenID int, publicID string) (*GeminiCachedContent, error) {
	var record GeminiCachedContent
	err := DB.Where("user_id = ? AND token_id = ? AND public_id = ? AND state <> ?",
		userID, tokenID, publicID, GeminiCachedContentDeleted).First(&record).Error
	if err != nil {
		return nil, err
	}
	return &record, nil
}

func ListGeminiCachedContents(userID, tokenID int, group string, beforeID int64, limit int) ([]GeminiCachedContent, error) {
	if userID <= 0 || tokenID <= 0 || limit <= 0 || limit > 1000 || beforeID < 0 {
		return nil, ErrGeminiCachedContentInvalid
	}
	query := DB.Where("user_id = ? AND token_id = ? AND state = ? AND expires_at > ?",
		userID, tokenID, GeminiCachedContentActive, common.GetTimestamp()).Where(map[string]any{"group": group})
	if beforeID > 0 {
		query = query.Where("id < ?", beforeID)
	}
	records := make([]GeminiCachedContent, 0)
	err := query.Order("id DESC").Limit(limit).Find(&records).Error
	return records, err
}

func ActivateGeminiCachedContent(record *GeminiCachedContent) (bool, error) {
	now := common.GetTimestamp()
	if record == nil || record.ID <= 0 || record.UpstreamName == "" || len(record.UpstreamName) > 512 ||
		record.UpstreamModel == "" || len(record.UpstreamModel) > 255 || len(record.DisplayName) > 512 ||
		record.TotalTokens < 0 || record.ExpiresAt <= now || record.PaidUntil < record.ExpiresAt {
		return false, ErrGeminiCachedContentInvalid
	}
	result := DB.Model(&GeminiCachedContent{}).Where("id = ? AND state = ?", record.ID, GeminiCachedContentPending).
		Updates(map[string]any{
			"upstream_name": record.UpstreamName, "upstream_model": record.UpstreamModel,
			"display_name": record.DisplayName, "total_tokens": record.TotalTokens,
			"expires_at": record.ExpiresAt, "paid_until": record.PaidUntil,
			"state": GeminiCachedContentActive, "updated_at": now,
		})
	if result.Error == nil && result.RowsAffected == 1 {
		record.State = GeminiCachedContentActive
		record.UpdatedAt = now
	}
	return result.RowsAffected == 1, result.Error
}

// An expired lease is deliberately not reclaimed: the previous upstream
// mutation may have succeeded and still needs settlement or reconciliation.
func BeginGeminiCachedContentMutation(id int64, token string, leaseUntil int64) (bool, error) {
	now := common.GetTimestamp()
	if id <= 0 || token == "" || len(token) > 64 || leaseUntil <= now {
		return false, ErrGeminiCachedContentInvalid
	}
	result := DB.Model(&GeminiCachedContent{}).Where("id = ? AND state = ? AND expires_at > ?", id, GeminiCachedContentActive, now).
		Updates(map[string]any{
			"state": GeminiCachedContentMutating, "mutation_token": token,
			"mutation_expires_at": leaseUntil, "updated_at": now,
		})
	return result.RowsAffected == 1, result.Error
}

func FinishGeminiCachedContentMutation(id int64, token string, expiresAt, paidUntil int64) (bool, error) {
	if id <= 0 || token == "" || len(token) > 64 || expiresAt <= common.GetTimestamp() || paidUntil < expiresAt {
		return false, ErrGeminiCachedContentInvalid
	}
	result := DB.Model(&GeminiCachedContent{}).
		Where("id = ? AND state = ? AND mutation_token = ? AND paid_until <= ?", id, GeminiCachedContentMutating, token, paidUntil).
		Updates(map[string]any{
			"state": GeminiCachedContentActive, "expires_at": expiresAt, "paid_until": paidUntil,
			"mutation_token": "", "mutation_expires_at": 0, "updated_at": common.GetTimestamp(),
		})
	return result.RowsAffected == 1, result.Error
}

func CancelGeminiCachedContentMutation(id int64, token string) (bool, error) {
	if id <= 0 || token == "" || len(token) > 64 {
		return false, ErrGeminiCachedContentInvalid
	}
	result := DB.Model(&GeminiCachedContent{}).Where("id = ? AND state = ? AND mutation_token = ?", id, GeminiCachedContentMutating, token).
		Updates(map[string]any{
			"state": GeminiCachedContentActive, "mutation_token": "", "mutation_expires_at": 0, "updated_at": common.GetTimestamp(),
		})
	return result.RowsAffected == 1, result.Error
}

func SoftDeleteGeminiCachedContent(id int64, token string) (bool, error) {
	if id <= 0 || len(token) > 64 {
		return false, ErrGeminiCachedContentInvalid
	}
	query := DB.Model(&GeminiCachedContent{}).Where("id = ?", id)
	if token == "" {
		query = query.Where("state = ?", GeminiCachedContentPending)
	} else {
		query = query.Where("state = ? AND mutation_token = ?", GeminiCachedContentMutating, token)
	}
	result := query.Updates(map[string]any{
		"state": GeminiCachedContentDeleted, "mutation_token": "", "mutation_expires_at": 0, "updated_at": common.GetTimestamp(),
	})
	return result.RowsAffected == 1, result.Error
}

// GeminiCacheChannelBaseURL is shared by candidate selection and binding checks.
func GeminiCacheChannelBaseURL(channel *Channel) (string, bool) {
	if channel == nil || channel.Status != common.ChannelStatusEnabled {
		return "", false
	}
	switch channel.Type {
	case constant.ChannelTypeGemini, constant.ChannelTypeVertexAi, constant.ChannelTypeNewAPI, constant.ChannelTypeOpenAI, constant.ChannelTypeAdvancedCustom:
	default:
		return "", false
	}
	base := strings.TrimRight(strings.TrimSpace(channel.GetBaseURL()), "/")
	if base == "" && channel.Type == constant.ChannelTypeGemini {
		base = constant.GetChannelBaseURL(constant.ChannelTypeGemini)
	}
	parsed, err := url.Parse(base)
	if base != "" && (err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.RawPath != "" || strings.Contains(base, "\\") || (parsed.Scheme != "https" && parsed.Scheme != "http")) {
		return "", false
	}
	if base == "" && channel.Type != constant.ChannelTypeVertexAi {
		return "", false
	}
	official := channel.Type == constant.ChannelTypeGemini && parsed.Scheme == "https" && strings.EqualFold(parsed.Hostname(), "generativelanguage.googleapis.com") && (parsed.Port() == "" || parsed.Port() == "443") && parsed.Path == ""
	settings := dto.ChannelSettings{}
	if channel.Setting != nil && *channel.Setting != "" && common.UnmarshalJsonStr(*channel.Setting, &settings) != nil {
		return "", false
	}
	// Cached resources bind their system and tool configuration at creation.
	// General relay overrides would make later references a different request.
	if settings.SystemPrompt != "" {
		return "", false
	}
	for _, override := range []*string{channel.ParamOverride, channel.HeaderOverride} {
		if override == nil || strings.TrimSpace(*override) == "" {
			continue
		}
		var values map[string]any
		if common.UnmarshalJsonStr(*override, &values) != nil || len(values) != 0 {
			return "", false
		}
	}
	if channel.Type == constant.ChannelTypeVertexAi {
		var other dto.ChannelOtherSettings
		if channel.OtherSettings != "" && common.UnmarshalJsonStr(channel.OtherSettings, &other) != nil {
			return "", false
		}
		if other.VertexKeyType == dto.VertexKeyTypeAPIKey {
			return "", false
		}
	} else if !official && !settings.GeminiExplicitCacheEnabled {
		return "", false
	}
	return base, true
}

// GeminiCacheChannelConfig contains the credential-independent provider policy.
// Candidate selection and outbound requests share it so an unsupported channel
// cannot shadow a usable channel merely through a higher routing priority.
type GeminiCacheChannelConfig struct {
	BaseURL    string
	Region     string
	AuthHeader string
}

var geminiCacheRegionPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

func GeminiCacheChannelConfigForModel(channel *Channel, localModel string) (*GeminiCacheChannelConfig, bool) {
	base, supported := GeminiCacheChannelBaseURL(channel)
	if !supported {
		return nil, false
	}
	var other dto.ChannelOtherSettings
	if channel.OtherSettings != "" && common.UnmarshalJsonStr(channel.OtherSettings, &other) != nil {
		return nil, false
	}
	config := &GeminiCacheChannelConfig{BaseURL: base, AuthHeader: "x-goog-api-key"}
	if channel.Type == constant.ChannelTypeGemini {
		return config, true
	}
	config.AuthHeader = "Authorization"
	if channel.Type == constant.ChannelTypeVertexAi {
		region := strings.TrimSpace(channel.Other)
		if strings.HasPrefix(region, "{") {
			var regions map[string]string
			if common.UnmarshalJsonStr(region, &regions) != nil {
				return nil, false
			}
			region = strings.TrimSpace(regions[localModel])
			if region == "" {
				region = strings.TrimSpace(regions["default"])
			}
		}
		// Vertex explicit caches are regional resources. Express mode is
		// already excluded by the base eligibility check.
		if region == "global" || !geminiCacheRegionPattern.MatchString(region) {
			return nil, false
		}
		config.Region = region
		return config, true
	}
	if channel.Type != constant.ChannelTypeAdvancedCustom {
		return config, true
	}
	advanced := other.AdvancedCustom
	if advanced == nil || advanced.Validate() != nil || localModel == "" || strings.ContainsAny(localModel, "/\\") {
		return nil, false
	}
	for _, action := range []string{"generateContent", "streamGenerateContent"} {
		route, found := advanced.MatchPathForModel("/v1beta/models/"+localModel+":"+action, localModel)
		if !found || !route.IsNative() {
			return nil, false
		}
		path := strings.TrimSpace(route.UpstreamPath)
		if path != "/v1beta/models/{model}:"+action && !(action == "streamGenerateContent" && path == "/v1beta/models/{model}:generateContent") {
			return nil, false
		}
		authHeader := "Authorization"
		if auth := route.Auth; auth != nil {
			if strings.TrimSpace(auth.Type) != dto.AdvancedCustomAuthTypeHeader {
				return nil, false
			}
			switch {
			case strings.EqualFold(strings.TrimSpace(auth.Name), "Authorization") && auth.Value == "Bearer {api_key}":
			case strings.EqualFold(strings.TrimSpace(auth.Name), "x-goog-api-key") && auth.Value == "{api_key}":
				authHeader = "x-goog-api-key"
			default:
				return nil, false
			}
		}
		if action == "streamGenerateContent" && authHeader != config.AuthHeader {
			return nil, false
		}
		config.AuthHeader = authHeader
	}
	return config, true
}

// GeminiCachedContentPublicIDExists includes expired and tombstoned bindings.
// Relay must never reinterpret an inaccessible local ID as an external cache.
func GeminiCachedContentPublicIDExists(publicID string) (bool, error) {
	var count int64
	err := DB.Model(&GeminiCachedContent{}).Where("public_id = ?", publicID).Count(&count).Error
	return count > 0, err
}
