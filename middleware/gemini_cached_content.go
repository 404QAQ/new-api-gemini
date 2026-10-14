package middleware

import (
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

// AuthorizeGeminiCache checks permissions even for pinned channels. Ordinary
// distribution deliberately lets administrator pins bypass some of these checks.
func AuthorizeGeminiCache(c *gin.Context, modelName, group string, channelID int) error {
	if common.GetContextKeyBool(c, constant.ContextKeyTokenModelLimitEnabled) {
		limits, _ := common.GetContextKeyType[map[string]bool](c, constant.ContextKeyTokenModelLimit)
		if !TokenModelLimitAllows(limits, modelName) {
			return errors.New("This token has no access to the cached content model")
		}
	}
	usingGroup := common.GetContextKeyString(c, constant.ContextKeyUsingGroup)
	userGroup := common.GetContextKeyString(c, constant.ContextKeyUserGroup)
	if usingGroup == "auto" {
		if !slices.Contains(service.GetRequestAutoGroups(c, userGroup), group) {
			return errors.New("Cached content group is not accessible")
		}
	} else if usingGroup != group {
		return errors.New("Cached content group is not accessible")
	}
	if !service.IsUserSelectableGroup(userGroup, group) || !model.IsChannelEnabledForGroupModel(group, modelName, channelID) {
		return errors.New("Cached content channel is not available for this model and group")
	}
	for _, pin := range service.GetChannelConstraints(c).Pins {
		if pin.ChannelId != channelID {
			return errors.New("Cached content conflicts with the requested channel")
		}
	}
	return nil
}

func prepareGeminiCacheReference(c *gin.Context) bool {
	path := c.Request.URL.Path
	if c.Request.Method != http.MethodPost || (!strings.HasPrefix(path, "/v1beta/models/") && !strings.HasPrefix(path, "/v1/models/")) {
		return true
	}
	storage, err := common.GetBodyStorage(c)
	if err != nil {
		abortGeminiCacheReference(c, invalidRequestStatus(err), "Unable to read Gemini request")
		return false
	}
	if !storage.IsDisk() {
		body, readErr := storage.Bytes()
		if readErr != nil {
			abortGeminiCacheReference(c, 400, "Unable to read Gemini request")
			return false
		}
		if !service.HasGeminiCacheReference(body) {
			return true
		}
	}
	reader, readErr := storage.NewReader()
	if readErr != nil {
		abortGeminiCacheReference(c, 400, "Unable to read Gemini request")
		return false
	}
	name, scanErr := service.GeminiCacheReferenceNameInReader(reader)
	closeErr := reader.Close()
	if scanErr != nil {
		message := "Unable to read Gemini request"
		var cacheErr *service.GeminiCacheError
		if errors.As(scanErr, &cacheErr) {
			message = cacheErr.Message
		}
		abortGeminiCacheReference(c, 400, message)
		return false
	}
	if closeErr != nil {
		abortGeminiCacheReference(c, 400, "Unable to read Gemini request")
		return false
	}
	if name == "" {
		return true
	}
	id, hasCachePrefix := strings.CutPrefix(name, "cachedContents/")
	local := false
	if hasCachePrefix && service.ValidGeminiCachePublicID(id) {
		var existsErr error
		local, existsErr = model.GeminiCachedContentPublicIDExists(id)
		if existsErr != nil {
			abortGeminiCacheReference(c, 503, "Unable to verify cached content ownership")
			return false
		}
	}
	if !local {
		// External resources retain the provider's existing validation and size
		// semantics. They can never fall through to shared Gemini API keys.
		service.GetChannelConstraints(c).AddFilter(dto.ChannelFilter{Kind: dto.FilterGeminiExternalCache})
		return true
	}
	if storage.Size() > service.GeminiCacheMaxRequestBytes {
		abortGeminiCacheReference(c, 413, "Cached content request exceeds the size limit")
		return false
	}
	body, readErr := storage.Bytes()
	if readErr != nil {
		abortGeminiCacheReference(c, 400, "Unable to read Gemini request")
		return false
	}
	if _, validationErr := service.ValidateGeminiCacheReference(body); validationErr != nil {
		abortGeminiCacheReference(c, 400, validationErr.Error())
		return false
	}
	if !strings.HasSuffix(path, ":generateContent") && !strings.HasSuffix(path, ":streamGenerateContent") {
		abortGeminiCacheReference(c, 400, "Cached content requires generateContent or streamGenerateContent")
		return false
	}
	record, err := model.GetGeminiCachedContent(c.GetInt("id"), c.GetInt("token_id"), id)
	if err != nil || record.Model != extractModelNameFromGeminiPath(path) {
		abortGeminiCacheReference(c, http.StatusForbidden, "CachedContent not found (or permission denied)")
		return false
	}
	if err := AuthorizeGeminiCache(c, record.Model, record.Group, record.ChannelID); err != nil {
		abortGeminiCacheReference(c, http.StatusForbidden, err.Error())
		return false
	}
	common.SetContextKey(c, constant.ContextKeyGeminiCachedContent, record)
	if common.GetContextKeyString(c, constant.ContextKeyUsingGroup) == "auto" {
		common.SetContextKey(c, constant.ContextKeyAutoGroup, record.Group)
	}
	service.GetChannelConstraints(c).AddPin(dto.ChannelPin{ChannelId: record.ChannelID, Source: dto.PinSourceGeminiCache, Rank: dto.PinRankOriginTask, RetryMode: dto.PinRetrySingleAttempt})
	return true
}

func abortGeminiCacheReference(c *gin.Context, status int, message string) {
	abortWithOpenAiMessage(c, status, service.LocalizeGeminiCacheError(c, message))
}
