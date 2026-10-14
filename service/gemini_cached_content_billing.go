package service

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/setting/billing_setting"
	"github.com/gin-gonic/gin"
)

type GeminiCacheBillingOperation string

const (
	GeminiCacheCreate  GeminiCacheBillingOperation = "create"
	GeminiCacheStorage GeminiCacheBillingOperation = "storage"
)

// GeminiCacheBillingQuote uses a separate pricing identity; the real model
// continues to control authorization, channel selection and upstream routing.
// Its expression and group ratio are frozen for the lifetime of the request.
type GeminiCacheBillingQuote struct {
	Quota           int
	BillingModel    string
	Tokens          int
	StorageDuration time.Duration
	Operation       GeminiCacheBillingOperation
	QuotaClamp      *common.QuotaClamp
	snapshot        billingexpr.BillingSnapshot
	result          billingexpr.TieredResult
}

// QuoteGeminiCacheBilling refuses an unconfigured price even when the caller
// allows unpriced generation models. An explicit zero expression remains free.
// Duration is billed in whole seconds, rounded up, with a 24-hour ceiling.
func QuoteGeminiCacheBilling(modelName string, tokens int, storageDuration time.Duration, operation GeminiCacheBillingOperation, groupRatio float64) (*GeminiCacheBillingQuote, error) {
	if strings.TrimSpace(modelName) == "" || strings.TrimSpace(modelName) != modelName {
		return nil, errors.New("cached content model is required")
	}
	if operation != GeminiCacheCreate && operation != GeminiCacheStorage {
		return nil, errors.New("invalid cached content billing operation")
	}
	if math.IsNaN(groupRatio) || math.IsInf(groupRatio, 0) || groupRatio < 0 {
		return nil, errors.New("invalid cached content billing group ratio")
	}
	if math.IsNaN(common.QuotaPerUnit) || math.IsInf(common.QuotaPerUnit, 0) || common.QuotaPerUnit <= 0 {
		return nil, errors.New("invalid quota conversion rate")
	}
	billingModel := modelName + "-cache-" + string(operation)
	expression, configured := billing_setting.GetBillingExpr(billingModel)
	if billing_setting.GetBillingMode(billingModel) != billing_setting.BillingModeTieredExpr || !configured || strings.TrimSpace(expression) == "" {
		return nil, fmt.Errorf("explicit cache pricing is not configured for %s", billingModel)
	}
	if _, err := billingexpr.CompileFromCache(expression); err != nil {
		return nil, fmt.Errorf("invalid explicit cache pricing: %w", err)
	}
	quote := &GeminiCacheBillingQuote{
		BillingModel: billingModel,
		Operation:    operation,
		snapshot: billingexpr.BillingSnapshot{
			BillingMode:          billing_setting.BillingModeTieredExpr,
			ModelName:            billingModel,
			ExprString:           expression,
			ExprHash:             billingexpr.ExprHashString(expression),
			GroupRatio:           groupRatio,
			PreConsumeMultiplier: 1,
			QuotaPerUnit:         common.QuotaPerUnit,
			ExprVersion:          billingexpr.ExprVersion(expression),
		},
	}
	quote, err := quote.withUsage(tokens, storageDuration)
	if err != nil {
		return quote, err
	}
	quote.snapshot.EstimatedPromptTokens = tokens
	quote.snapshot.EstimatedQuotaBeforeGroup = quote.result.ActualQuotaBeforeGroup
	quote.snapshot.EstimatedQuotaAfterGroup = quote.Quota
	quote.snapshot.EstimatedTier = quote.result.MatchedTier
	quote.snapshot.EstimatedBillingUnit = quote.result.BillingUnit
	quote.snapshot.EstimatedFixedPrice = quote.result.FixedPrice
	quote.result.CrossedTier = false
	return quote, nil
}

// withUsage only accepts measured cache quantities. Client JSON and headers
// are deliberately excluded from the expression's request environment.
func (q *GeminiCacheBillingQuote) withUsage(tokens int, storageDuration time.Duration) (*GeminiCacheBillingQuote, error) {
	if tokens < 1 || tokens > GeminiCacheMaxTokens {
		return nil, fmt.Errorf("cached content token count must be between 1 and %d", GeminiCacheMaxTokens)
	}
	if storageDuration < 0 || storageDuration > GeminiCacheMaxTTL {
		return nil, errors.New("cached content billing duration must be between zero and 24 hours")
	}
	seconds := (storageDuration + time.Second - 1) / time.Second
	body, err := common.Marshal(map[string]any{
		"cache_storage_hours":   float64(seconds) / 3600,
		"cache_storage_seconds": int64(seconds),
		"cache_operation":       string(q.Operation),
	})
	if err != nil {
		return nil, err
	}
	result, err := billingexpr.ComputeTieredQuotaWithRequest(&q.snapshot, billingexpr.TokenParams{
		P: float64(tokens), Len: float64(tokens),
	}, billingexpr.RequestInput{Body: body})
	if err != nil {
		return nil, fmt.Errorf("explicit cache price evaluation failed: %w", err)
	}
	updated := *q
	updated.Tokens = tokens
	updated.StorageDuration = seconds * time.Second
	updated.Quota = result.ActualQuotaAfterGroup
	updated.QuotaClamp = result.Clamp
	updated.result = result
	if result.Clamp != nil {
		return &updated, result.Clamp
	}
	if math.IsNaN(result.ActualQuotaBeforeGroup) || math.IsInf(result.ActualQuotaBeforeGroup, 0) || result.ActualQuotaBeforeGroup < 0 || updated.Quota < 0 {
		return nil, errors.New("explicit cache price must be finite and non-negative")
	}
	return &updated, nil
}

// GeminiCacheBillingSession delegates wallet/token/subscription accounting to
// the ordinary billing session. It does not provide cross-process recovery;
// the caller must persist the resource mutation before exposing its result.
type GeminiCacheBillingSession struct {
	mu      sync.Mutex
	ctx     *gin.Context
	info    *relaycommon.RelayInfo
	billing *BillingSession
	quote   *GeminiCacheBillingQuote
	closed  bool
}

func NewGeminiCacheBillingSession(c *gin.Context, info *relaycommon.RelayInfo, quote *GeminiCacheBillingQuote) (*GeminiCacheBillingSession, *types.NewAPIError) {
	if info == nil || quote == nil || quote.snapshot.ExprString == "" {
		return nil, types.NewErrorWithStatusCode(errors.New("cached content billing quote is required"), types.ErrorCodeModelPriceError, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
	}
	if info.Billing != nil {
		return nil, types.NewErrorWithStatusCode(errors.New("cached content billing session already exists"), types.ErrorCodeModelPriceError, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
	}
	info.BillingModelName = quote.BillingModel
	info.ForcePreConsume = true
	info.ForceReserve = true
	info.QuotaClamp = quote.QuotaClamp
	snapshot := quote.snapshot
	info.TieredBillingSnapshot = &snapshot
	info.PriceData.GroupRatioInfo.GroupRatio = snapshot.GroupRatio
	info.PriceData.QuotaToPreConsume = quote.Quota
	var billing *BillingSession
	if quote.Quota > 0 {
		if apiErr := PreConsumeBilling(c, quote.Quota, info); apiErr != nil {
			return nil, apiErr
		}
		billing = info.Billing.(*BillingSession)
	}
	copyQuote := *quote
	return &GeminiCacheBillingSession{ctx: c, info: info, billing: billing, quote: &copyQuote}, nil
}

// ReserveActual reconciles measured tokens/duration against the frozen price.
// A larger charge must be fully reserved before the cache becomes usable.
// Smaller reservations are refunded only when SettleAndLog commits the charge.
func (s *GeminiCacheBillingSession) ReserveActual(tokens int, storageDuration time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("cached content billing session is closed")
	}
	quote, err := s.quote.withUsage(tokens, storageDuration)
	if quote != nil && quote.QuotaClamp != nil {
		s.info.QuotaClamp = quote.QuotaClamp
	}
	if err != nil {
		return err
	}
	if s.billing == nil && quote.Quota > 0 {
		if apiErr := PreConsumeBilling(s.ctx, quote.Quota, s.info); apiErr != nil {
			return apiErr
		}
		s.billing = s.info.Billing.(*BillingSession)
	} else if s.billing != nil {
		if err := s.billing.Reserve(quote.Quota); err != nil {
			return err
		}
	}
	s.quote = quote
	return nil
}

// SettleAndLog is idempotent within this request. The caller serializes
// cross-request cache mutations using the persisted resource lease.
func (s *GeminiCacheBillingSession) SettleAndLog(c *gin.Context, publicID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	if s.billing != nil {
		if err := SettleBilling(c, s.info, s.quote.Quota); err != nil {
			return err
		}
	}
	s.closed = true
	model.UpdateUserUsedQuotaAndRequestCount(s.info.UserId, s.quote.Quota)
	model.UpdateChannelUsedQuota(s.info.ChannelId, s.quote.Quota)
	other := model.NewLogOther()
	other.SetPublic("cache_operation", string(s.quote.Operation))
	other.SetPublic("cached_content", "cachedContents/"+publicID)
	other.SetPublic("cache_storage_tokens", s.quote.Tokens)
	other.SetPublic("cache_storage_seconds", int64(s.quote.StorageDuration/time.Second))
	other.SetPublic("cache_storage_hours", s.quote.StorageDuration.Hours())
	other.SetPublic("billing_model", s.quote.BillingModel)
	other.SetPublic("request_model", s.info.OriginModelName)
	other.SetPublic("group_ratio", s.quote.snapshot.GroupRatio)
	inputTokens := 0
	if s.quote.Operation == GeminiCacheCreate {
		inputTokens = s.quote.Tokens
	}
	other.SetPublic("cache_creation_input_tokens", inputTokens)
	appendBillingInfo(s.info, other)
	appendRequestPath(c, s.info, other)
	AppendRelayLogAdminInfo(c, s.info, other)
	InjectTieredBillingInfo(other, s.info, &s.quote.result)
	attachQuotaSaturation(c, s.info, other)
	model.RecordConsumeLog(c, s.info.UserId, model.RecordConsumeLogParams{
		ChannelId:      s.info.ChannelId,
		PromptTokens:   inputTokens,
		ModelName:      s.quote.BillingModel,
		TokenName:      c.GetString("token_name"),
		Quota:          s.quote.Quota,
		TokenId:        s.info.TokenId,
		UseTimeSeconds: int(max(time.Since(s.info.StartTime).Seconds(), 0)),
		Group:          s.info.UsingGroup,
		Other:          other,
	})
	return nil
}

func (s *GeminiCacheBillingSession) Refund(c *gin.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	if s.billing != nil {
		s.billing.Refund(c)
	}
}

// LogReconciliation preserves the reservation audit when an upstream outcome
// cannot be proven. It does not settle or refund the uncertain operation.
func (s *GeminiCacheBillingSession) LogReconciliation(c *gin.Context, publicID, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	reserved := 0
	if s.billing != nil {
		reserved = s.billing.GetPreConsumedQuota()
	}
	other := model.NewLogOther()
	other.SetPublic("cache_reconciliation_required", true)
	other.SetPublic("cached_content", "cachedContents/"+publicID)
	other.SetPublic("cache_operation", string(s.quote.Operation))
	other.SetAdmin("cache_reconciliation_reason", reason)
	other.SetAdmin("cache_reserved_quota", reserved)
	other.SetAdmin("cache_billing_snapshot", s.quote.snapshot)
	appendBillingInfo(s.info, other)
	appendRequestPath(c, s.info, other)
	model.RecordErrorLog(c, s.info.UserId, s.info.ChannelId, s.quote.BillingModel, c.GetString("token_name"), "Request failed", s.info.TokenId,
		int(max(time.Since(s.info.StartTime).Seconds(), 0)), false, s.info.UsingGroup, other)
	common.SysError(common.LogText("Gemini cached content requires manual reconciliation: public_id=%s operation=%s reserved_quota=%d reason=%s", publicID, s.quote.Operation, reserved, reason))
}
