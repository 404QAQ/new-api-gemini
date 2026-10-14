package controller

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	taskdto "github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

func geminiCacheControllerError(c *gin.Context, err error) {
	status, message := http.StatusInternalServerError, "Cached content operation could not be completed"
	var cacheErr *service.GeminiCacheError
	var apiErr *types.NewAPIError
	switch {
	case errors.As(err, &cacheErr):
		status, message = cacheErr.HTTPStatus, cacheErr.Message
	case errors.As(err, &apiErr):
		status = apiErr.StatusCode
		if status == http.StatusForbidden {
			message = "Insufficient quota or access denied for cached content"
		}
	}
	googleStatus := "INTERNAL"
	switch status {
	case 400:
		googleStatus = "INVALID_ARGUMENT"
	case 401:
		googleStatus = "UNAUTHENTICATED"
	case 403:
		googleStatus = "PERMISSION_DENIED"
	case 404:
		googleStatus = "NOT_FOUND"
	case 409:
		googleStatus = "FAILED_PRECONDITION"
	case 413, 429:
		googleStatus = "RESOURCE_EXHAUSTED"
	case 503:
		googleStatus = "UNAVAILABLE"
	case 504:
		googleStatus = "DEADLINE_EXCEEDED"
	}
	c.JSON(status, gin.H{"error": gin.H{"code": status, "message": service.LocalizeGeminiCacheError(c, message), "status": googleStatus}})
}

func geminiCacheRequestError(status int, message string) error {
	return &service.GeminiCacheError{HTTPStatus: status, Message: message}
}

func geminiCacheBody(c *gin.Context) ([]byte, error) {
	storage, err := common.GetBodyStorage(c)
	if err != nil {
		return nil, geminiCacheRequestError(http.StatusBadRequest, "Unable to read cached content request")
	}
	if storage.Size() > service.GeminiCacheMaxRequestBytes {
		return nil, geminiCacheRequestError(http.StatusRequestEntityTooLarge, "Cached content request exceeds the size limit")
	}
	return storage.Bytes()
}

func geminiCachePublicID() (string, error) {
	var raw [24]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

func geminiCacheBillingInfo(c *gin.Context, record *model.GeminiCachedContent) *relaycommon.RelayInfo {
	common.SetContextKey(c, constant.ContextKeyOriginalModel, record.Model)
	info := relaycommon.GenRelayInfoGemini(c, nil)
	info.InitChannelMeta(c)
	info.UpstreamModelName = record.UpstreamModel
	info.UsingGroup = record.Group
	return info
}

func geminiCacheSelectedChannel(c *gin.Context, modelName string) (*model.Channel, string, error) {
	group := common.GetContextKeyString(c, constant.ContextKeyUsingGroup)
	service.GetChannelConstraints(c).AddFilter(taskdto.ChannelFilter{Kind: taskdto.FilterGeminiExplicitCache})
	channel, selectedGroup, selectErr := service.SelectChannelForRequest(c, modelName, &service.RetryParam{Ctx: c, ModelName: modelName, TokenGroup: group})
	if selectErr != nil || channel == nil {
		return nil, "", geminiCacheRequestError(http.StatusServiceUnavailable, "No eligible Gemini explicit cache channel")
	}
	if selectedGroup == "" {
		selectedGroup = group
	}
	if err := middleware.AuthorizeGeminiCache(c, modelName, selectedGroup, channel.Id); err != nil {
		return nil, "", geminiCacheRequestError(http.StatusForbidden, err.Error())
	}
	if apiErr := middleware.SetupContextForSelectedChannel(c, channel, modelName); apiErr != nil {
		return nil, "", apiErr
	}
	return channel, selectedGroup, nil
}

func GeminiCachedContentCreate(c *gin.Context) {
	raw, err := geminiCacheBody(c)
	if err != nil {
		geminiCacheControllerError(c, err)
		return
	}
	started := time.Now().UTC().Truncate(time.Second)
	request, err := service.ParseGeminiCacheCreate(raw, started)
	if err != nil {
		geminiCacheControllerError(c, err)
		return
	}
	channel, group, err := geminiCacheSelectedChannel(c, request.Model)
	if err != nil {
		geminiCacheControllerError(c, err)
		return
	}
	key := common.GetContextKeyString(c, constant.ContextKeyChannelKey)
	provider, err := service.GeminiCacheProviderForChannel(channel, key, request.Model)
	if err != nil {
		geminiCacheControllerError(c, err)
		return
	}
	upstreamModel, err := service.GeminiCacheUpstreamModel(channel, request.Model)
	if err != nil {
		geminiCacheControllerError(c, err)
		return
	}
	// Reject missing pricing before sending even a countTokens request.
	groupRatio := service.GetUserGroupRatio(c.GetString("user_group"), group)
	if _, err = service.QuoteGeminiCacheBilling(request.Model, 1, request.ExpiresAt.Sub(started), service.GeminiCacheCreate, groupRatio); err != nil {
		geminiCacheControllerError(c, geminiCacheRequestError(400, err.Error()))
		return
	}
	tokens, err := service.CountGeminiCacheTokens(c.Request.Context(), channel, key, request, upstreamModel)
	if err != nil {
		geminiCacheControllerError(c, err)
		return
	}
	quote, err := service.QuoteGeminiCacheBilling(request.Model, tokens, request.ExpiresAt.Sub(started), service.GeminiCacheCreate, groupRatio)
	if err != nil {
		geminiCacheControllerError(c, geminiCacheRequestError(400, err.Error()))
		return
	}
	publicID, err := geminiCachePublicID()
	if err != nil {
		geminiCacheControllerError(c, err)
		return
	}
	record := &model.GeminiCachedContent{PublicID: publicID, UserID: c.GetInt("id"), TokenID: c.GetInt("token_id"), ChannelID: channel.Id, Group: group, KeyFingerprint: service.GeminiCacheKeyFingerprint(key), BaseURL: provider.BindingURL, Model: request.Model, UpstreamModel: upstreamModel, DisplayName: request.DisplayName, TotalTokens: tokens, ExpiresAt: request.ExpiresAt.Unix()}
	billing, apiErr := service.NewGeminiCacheBillingSession(c, geminiCacheBillingInfo(c, record), quote)
	if apiErr != nil {
		geminiCacheControllerError(c, apiErr)
		return
	}
	retainReservation := false
	defer func() {
		if !retainReservation {
			billing.Refund(c)
		}
	}()
	if err := model.CreateGeminiCachedContent(record); err != nil {
		geminiCacheControllerError(c, err)
		return
	}
	// Once the paid mutation starts, finish bookkeeping even if the client leaves.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), 6*time.Minute)
	defer cancel()
	resource, err := service.CreateGeminiCachedContent(ctx, channel, key, request, upstreamModel)
	if err != nil {
		var cacheErr *service.GeminiCacheError
		if errors.As(err, &cacheErr) && cacheErr.OutcomeUncertain {
			retainReservation = true
			if cacheErr.UpstreamName != "" {
				if saveErr := model.DB.Model(record).Update("upstream_name", cacheErr.UpstreamName).Error; saveErr != nil {
					common.SysError(common.LogText("Unable to persist uncertain Gemini cache identity: public_id=%s", record.PublicID))
				}
			}
			billing.LogReconciliation(c, record.PublicID, "create_outcome_unknown")
		} else {
			_, _ = model.SoftDeleteGeminiCachedContent(record.ID, "")
		}
		geminiCacheControllerError(c, err)
		return
	}
	record.UpstreamName, record.TotalTokens = resource.Name, resource.TotalTokens
	record.ExpiresAt, record.PaidUntil = resource.ExpireTime.Unix(), resource.ExpireTime.Unix()
	cleanup := true
	defer func() {
		if !cleanup {
			return
		}
		if err := service.DeleteGeminiCachedContent(context.WithoutCancel(ctx), channel, key, record.UpstreamName, record.Model); err != nil {
			retainReservation = true
			billing.LogReconciliation(c, record.PublicID, "create_cleanup_failed")
			return
		}
		_, _ = model.SoftDeleteGeminiCachedContent(record.ID, "")
	}()
	// Persist the upstream identity while still hidden, before committing quota.
	if err := model.DB.Model(&model.GeminiCachedContent{}).Where("id = ? AND state = ?", record.ID, model.GeminiCachedContentPending).
		Updates(map[string]any{"upstream_name": record.UpstreamName, "total_tokens": record.TotalTokens, "expires_at": record.ExpiresAt, "paid_until": record.PaidUntil}).Error; err != nil {
		geminiCacheControllerError(c, err)
		return
	}
	if err := billing.ReserveActual(resource.TotalTokens, resource.ExpireTime.Sub(started)); err != nil {
		geminiCacheControllerError(c, err)
		return
	}
	activated, err := model.ActivateGeminiCachedContent(record)
	if err != nil || !activated {
		geminiCacheControllerError(c, errors.New("unable to activate cached content"))
		return
	}
	cleanup = false
	// The complete charge is already reserved. Preserve both the resource and
	// its reservation if final accounting fails; list/get can recover the ID.
	retainReservation = true
	if err := billing.SettleAndLog(c, record.PublicID); err != nil {
		billing.LogReconciliation(c, record.PublicID, "create_settlement_failed")
		geminiCacheControllerError(c, err)
		return
	}
	c.JSON(http.StatusOK, service.GeminiCacheClientResponse(record))
}

func geminiCacheOwnedRecord(c *gin.Context) (*model.GeminiCachedContent, error) {
	id := c.Param("cacheID")
	if !service.ValidGeminiCachePublicID(id) {
		return nil, geminiCacheRequestError(403, "CachedContent not found (or permission denied)")
	}
	record, err := model.GetGeminiCachedContent(c.GetInt("id"), c.GetInt("token_id"), id)
	if err != nil {
		return nil, geminiCacheRequestError(403, "CachedContent not found (or permission denied)")
	}
	if err := middleware.AuthorizeGeminiCache(c, record.Model, record.Group, record.ChannelID); err != nil {
		return nil, geminiCacheRequestError(403, err.Error())
	}
	return record, nil
}

func GeminiCachedContentGet(c *gin.Context) {
	record, err := geminiCacheOwnedRecord(c)
	if err != nil {
		geminiCacheControllerError(c, err)
		return
	}
	c.JSON(http.StatusOK, service.GeminiCacheClientResponse(record))
}

func GeminiCachedContentList(c *gin.Context) {
	limit := 50
	if raw := c.Query("pageSize"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 1000 {
			geminiCacheControllerError(c, geminiCacheRequestError(400, "pageSize must be between 1 and 1000"))
			return
		}
		limit = value
	}
	before := int64(0)
	if raw := c.Query("pageToken"); raw != "" {
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || value < 1 {
			geminiCacheControllerError(c, geminiCacheRequestError(400, "Invalid pageToken"))
			return
		}
		before = value
	}
	groups := []string{c.GetString("group")}
	if groups[0] == "auto" {
		groups = service.GetRequestAutoGroups(c, c.GetString("user_group"))
	}
	records := make([]model.GeminiCachedContent, 0)
	for _, group := range groups {
		found, err := model.ListGeminiCachedContents(c.GetInt("id"), c.GetInt("token_id"), group, before, limit)
		if err != nil {
			geminiCacheControllerError(c, err)
			return
		}
		records = append(records, found...)
	}
	slices.SortFunc(records, func(a, b model.GeminiCachedContent) int {
		if a.ID > b.ID {
			return -1
		}
		if a.ID < b.ID {
			return 1
		}
		return 0
	})
	more := len(records) >= limit
	if len(records) > limit {
		records = records[:limit]
	}
	items := make([]map[string]any, 0, len(records))
	for i := range records {
		if middleware.AuthorizeGeminiCache(c, records[i].Model, records[i].Group, records[i].ChannelID) == nil {
			items = append(items, service.GeminiCacheClientResponse(&records[i]))
		}
	}
	response := gin.H{"cachedContents": items}
	if more {
		response["nextPageToken"] = strconv.FormatInt(records[len(records)-1].ID, 10)
	}
	c.JSON(http.StatusOK, response)
}

// Taking the lease hides the cache before changing upstream state. Re-read it
// after the CAS so a preceding completed mutation cannot make our price stale.
func geminiCacheBeginMutation(c *gin.Context) (*model.GeminiCachedContent, *model.Channel, string, error) {
	record, err := geminiCacheOwnedRecord(c)
	if err != nil {
		return nil, nil, "", err
	}
	channel, err := model.GetChannelById(record.ChannelID, true)
	if err != nil {
		return nil, nil, "", geminiCacheRequestError(409, "Cached content channel is unavailable")
	}
	key, _, err := service.GeminiCacheBinding(channel, record)
	if err != nil {
		return nil, nil, "", err
	}
	mutation, err := geminiCachePublicID()
	if err != nil {
		return nil, nil, "", err
	}
	claimed, err := model.BeginGeminiCachedContentMutation(record.ID, mutation, time.Now().Add(2*time.Minute).Unix())
	if err != nil {
		return nil, nil, "", err
	}
	if !claimed {
		return nil, nil, "", geminiCacheRequestError(409, "Another cached content operation is in progress")
	}
	record, err = model.GetGeminiCachedContentForRecovery(c.GetInt("id"), c.GetInt("token_id"), record.PublicID)
	if err != nil {
		return nil, nil, "", err
	}
	return record, channel, key, nil
}

func GeminiCachedContentPatch(c *gin.Context) {
	if mask := c.Query("updateMask"); mask != "" && mask != "ttl" && mask != "expireTime" {
		geminiCacheControllerError(c, geminiCacheRequestError(400, "Only ttl or expireTime may be updated"))
		return
	}
	raw, err := geminiCacheBody(c)
	if err != nil {
		geminiCacheControllerError(c, err)
		return
	}
	patch, err := service.ParseGeminiCachePatch(raw, time.Now().UTC().Truncate(time.Second))
	if err != nil {
		geminiCacheControllerError(c, err)
		return
	}
	record, channel, key, err := geminiCacheBeginMutation(c)
	if err != nil {
		geminiCacheControllerError(c, err)
		return
	}
	release := true
	defer func() {
		if release {
			_, _ = model.CancelGeminiCachedContentMutation(record.ID, record.MutationToken)
		}
	}()
	common.SetContextKey(c, constant.ContextKeyGeminiCachedContent, record)
	if apiErr := middleware.SetupContextForSelectedChannel(c, channel, record.Model); apiErr != nil {
		geminiCacheControllerError(c, apiErr)
		return
	}
	extension := max(patch.ExpiresAt.Unix()-record.PaidUntil, 0)
	var billing *service.GeminiCacheBillingSession
	retainReservation := false
	if extension > 0 {
		quote, quoteErr := service.QuoteGeminiCacheBilling(record.Model, record.TotalTokens, time.Duration(extension)*time.Second, service.GeminiCacheStorage, service.GetUserGroupRatio(c.GetString("user_group"), record.Group))
		if quoteErr != nil {
			geminiCacheControllerError(c, geminiCacheRequestError(400, quoteErr.Error()))
			return
		}
		var apiErr *types.NewAPIError
		billing, apiErr = service.NewGeminiCacheBillingSession(c, geminiCacheBillingInfo(c, record), quote)
		if apiErr != nil {
			geminiCacheControllerError(c, apiErr)
			return
		}
		defer func() {
			if !retainReservation {
				billing.Refund(c)
			}
		}()
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), 90*time.Second)
	defer cancel()
	resource, err := service.PatchGeminiCachedContent(ctx, channel, key, record.UpstreamName, patch, record.UpstreamModel, record.Model)
	var cacheErr *service.GeminiCacheError
	if errors.As(err, &cacheErr) && !cacheErr.OutcomeUncertain {
		// A definite rejection did not extend upstream storage.
		geminiCacheControllerError(c, err)
		return
	}
	release = false // A timeout can still mean the upstream mutation succeeded.
	completed := false
	defer func() {
		if completed {
			return
		}
		oldExpiry := time.Unix(record.ExpiresAt, 0).UTC()
		encoded, _ := common.Marshal(oldExpiry.Format(time.RFC3339))
		old := &service.GeminiCachePatchRequest{Body: map[string]common.RawMessage{"expireTime": encoded}, ExpiresAt: oldExpiry, UpdateMask: "expireTime"}
		restored, rollbackErr := service.PatchGeminiCachedContent(context.WithoutCancel(ctx), channel, key, record.UpstreamName, old, record.UpstreamModel, record.Model)
		if rollbackErr == nil && restored.ExpireTime.Unix() == record.ExpiresAt {
			_, _ = model.CancelGeminiCachedContentMutation(record.ID, record.MutationToken)
		} else {
			retainReservation = true
			if billing != nil {
				billing.LogReconciliation(c, record.PublicID, "patch_rollback_failed")
			} else {
				common.SysError(common.LogText("Gemini cached content mutation remains locked: public_id=%s", record.PublicID))
			}
		}
	}()
	if err != nil {
		geminiCacheControllerError(c, err)
		return
	}
	if resource.TotalTokens != record.TotalTokens {
		geminiCacheControllerError(c, geminiCacheRequestError(502, "Cached content token count changed unexpectedly"))
		return
	}
	if billing != nil {
		actualExtension := max(resource.ExpireTime.Unix()-record.PaidUntil, 0)
		if err := billing.ReserveActual(record.TotalTokens, time.Duration(actualExtension)*time.Second); err != nil {
			geminiCacheControllerError(c, err)
			return
		}
	}
	expires := resource.ExpireTime.Unix()
	paidUntil := max(record.PaidUntil, expires)
	finished, err := model.FinishGeminiCachedContentMutation(record.ID, record.MutationToken, expires, paidUntil)
	if err != nil || !finished {
		geminiCacheControllerError(c, errors.New("unable to finish cached content update"))
		return
	}
	completed = true
	retainReservation = true
	if billing != nil {
		if err := billing.SettleAndLog(c, record.PublicID); err != nil {
			billing.LogReconciliation(c, record.PublicID, "patch_settlement_failed")
			geminiCacheControllerError(c, err)
			return
		}
	}
	record.ExpiresAt, record.PaidUntil, record.UpdatedAt = expires, paidUntil, time.Now().Unix()
	c.JSON(http.StatusOK, service.GeminiCacheClientResponse(record))
}

func GeminiCachedContentDelete(c *gin.Context) {
	record, channel, key, err := geminiCacheBeginMutation(c)
	if err != nil {
		geminiCacheControllerError(c, err)
		return
	}
	// Retain the lock on uncertain upstream failure. No generation can reuse a
	// resource whose deletion may already have completed.
	if err := service.DeleteGeminiCachedContent(context.WithoutCancel(c.Request.Context()), channel, key, record.UpstreamName, record.Model); err != nil {
		var cacheErr *service.GeminiCacheError
		if errors.As(err, &cacheErr) && !cacheErr.OutcomeUncertain {
			_, _ = model.CancelGeminiCachedContentMutation(record.ID, record.MutationToken)
		} else {
			common.SysError(common.LogText("Gemini cached content deletion remains locked: public_id=%s", record.PublicID))
		}
		geminiCacheControllerError(c, err)
		return
	}
	deleted, err := model.SoftDeleteGeminiCachedContent(record.ID, record.MutationToken)
	if err != nil || !deleted {
		geminiCacheControllerError(c, errors.New("unable to finish cached content deletion"))
		return
	}
	c.JSON(http.StatusOK, gin.H{})
}
