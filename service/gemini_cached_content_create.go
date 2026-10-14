package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/gin-gonic/gin"
)

var ErrGeminiCacheBelowMinimumTokens = errors.New("cached content is below the automatic cache minimum token count")

// GeminiCacheCreateReconciliationError means a paid creation or its accounting
// cannot be safely retried. The returned record identifies the retained resource
// and reservation. Unwrap preserves the original public error response.
type GeminiCacheCreateReconciliationError struct {
	Err error
}

func (e *GeminiCacheCreateReconciliationError) Error() string { return e.Err.Error() }
func (e *GeminiCacheCreateReconciliationError) Unwrap() error { return e.Err }

// CreateManagedGeminiCachedContent performs the same paid creation for explicit
// requests and automatic cache creation. Callers must authorize and select the
// channel first. A positive minTokens rejects small prefixes after countTokens,
// before creating a resource or reserving quota; zero preserves explicit calls.
func CreateManagedGeminiCachedContent(c *gin.Context, channel *model.Channel, key, group string, request *GeminiCacheCreateRequest, minTokens int) (record *model.GeminiCachedContent, err error) {
	if request == nil || minTokens < 0 || minTokens > GeminiCacheMaxTokens {
		return nil, geminiCacheInvalid("Invalid cached content request")
	}
	started := time.Now().UTC().Truncate(time.Second)
	provider, err := GeminiCacheProviderForChannel(channel, key, request.Model)
	if err != nil {
		return nil, err
	}
	upstreamModel, err := GeminiCacheUpstreamModel(channel, request.Model)
	if err != nil {
		return nil, err
	}
	// Reject missing pricing before sending even a countTokens request.
	groupRatio := GetUserGroupRatio(c.GetString("user_group"), group)
	if _, err = QuoteGeminiCacheBilling(request.Model, 1, request.ExpiresAt.Sub(started), GeminiCacheCreate, groupRatio); err != nil {
		return nil, geminiCacheInvalid(err.Error())
	}
	tokens, err := CountGeminiCacheTokens(c.Request.Context(), channel, key, request, upstreamModel)
	if err != nil {
		return nil, err
	}
	if tokens < minTokens {
		return nil, ErrGeminiCacheBelowMinimumTokens
	}
	quote, err := QuoteGeminiCacheBilling(request.Model, tokens, request.ExpiresAt.Sub(started), GeminiCacheCreate, groupRatio)
	if err != nil {
		return nil, geminiCacheInvalid(err.Error())
	}
	var rawID [24]byte
	if _, err := rand.Read(rawID[:]); err != nil {
		return nil, err
	}
	record = &model.GeminiCachedContent{
		PublicID: hex.EncodeToString(rawID[:]), UserID: c.GetInt("id"), TokenID: c.GetInt("token_id"),
		ChannelID: channel.Id, Group: group, KeyFingerprint: GeminiCacheKeyFingerprint(key),
		BaseURL: provider.BindingURL, Model: request.Model, UpstreamModel: upstreamModel,
		DisplayName: request.DisplayName, TotalTokens: tokens, ExpiresAt: request.ExpiresAt.Unix(),
	}
	// A generation request can already have billing and stream state. The cache
	// has its own billing session and must not alter that request's context.
	billingContext := c.Copy()
	common.SetContextKey(billingContext, constant.ContextKeyOriginalModel, record.Model)
	info := relaycommon.GenRelayInfoGemini(billingContext, nil)
	info.InitChannelMeta(billingContext)
	// Subscription reservations deduplicate by request ID. A cache creation is
	// a separate paid operation even when initiated by a generation request.
	info.RequestId = "gemini-cache-" + record.PublicID
	info.UpstreamModelName = record.UpstreamModel
	info.UsingGroup = record.Group
	billing, apiErr := NewGeminiCacheBillingSession(billingContext, info, quote)
	if apiErr != nil {
		return nil, apiErr
	}
	retainReservation := false
	defer func() {
		if !retainReservation {
			billing.Refund(billingContext)
		} else if err != nil {
			err = &GeminiCacheCreateReconciliationError{Err: err}
		}
	}()
	if err := model.CreateGeminiCachedContent(record); err != nil {
		return nil, err
	}
	// Once the paid mutation starts, finish bookkeeping even if the client leaves.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), 6*time.Minute)
	defer cancel()
	resource, err := CreateGeminiCachedContent(ctx, channel, key, request, upstreamModel)
	if err != nil {
		var cacheErr *GeminiCacheError
		if errors.As(err, &cacheErr) && cacheErr.OutcomeUncertain {
			retainReservation = true
			if cacheErr.UpstreamName != "" {
				record.UpstreamName = cacheErr.UpstreamName
				if saveErr := model.DB.Model(record).Update("upstream_name", cacheErr.UpstreamName).Error; saveErr != nil {
					common.SysError(common.LogText("Unable to persist uncertain Gemini cache identity: public_id=%s", record.PublicID))
				}
			}
			billing.LogReconciliation(billingContext, record.PublicID, "create_outcome_unknown")
		} else {
			_, _ = model.SoftDeleteGeminiCachedContent(record.ID, "")
		}
		return record, err
	}
	record.UpstreamName, record.TotalTokens = resource.Name, resource.TotalTokens
	record.ExpiresAt, record.PaidUntil = resource.ExpireTime.Unix(), resource.ExpireTime.Unix()
	cleanup := true
	defer func() {
		if !cleanup {
			return
		}
		if err := DeleteGeminiCachedContent(context.WithoutCancel(ctx), channel, key, record.UpstreamName, record.Model); err != nil {
			retainReservation = true
			billing.LogReconciliation(billingContext, record.PublicID, "create_cleanup_failed")
			return
		}
		_, _ = model.SoftDeleteGeminiCachedContent(record.ID, "")
	}()
	// Persist the upstream identity while still hidden, before committing quota.
	if err := model.DB.Model(&model.GeminiCachedContent{}).Where("id = ? AND state = ?", record.ID, model.GeminiCachedContentPending).
		Updates(map[string]any{"upstream_name": record.UpstreamName, "total_tokens": record.TotalTokens, "expires_at": record.ExpiresAt, "paid_until": record.PaidUntil}).Error; err != nil {
		return record, err
	}
	if err := billing.ReserveActual(resource.TotalTokens, resource.ExpireTime.Sub(started)); err != nil {
		return record, err
	}
	activated, err := model.ActivateGeminiCachedContent(record)
	if err != nil || !activated {
		return record, errors.New("unable to activate cached content")
	}
	cleanup = false
	// The complete charge is already reserved. Preserve both the resource and
	// its reservation if final accounting fails; list/get can recover the ID.
	retainReservation = true
	if err := billing.SettleAndLog(billingContext, record.PublicID); err != nil {
		billing.LogReconciliation(billingContext, record.PublicID, "create_settlement_failed")
		return record, err
	}
	return record, nil
}
