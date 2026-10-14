package router

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/billing_setting"
	"github.com/QuantumNous/new-api/setting/config"
	"github.com/QuantumNous/new-api/setting/model_setting"
	"github.com/bytedance/gopkg/util/gopool"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestListModelsSupportsOpenAIAndGeminiAuthentication(t *testing.T) {
	setupRelayRouterTestDB(t)

	user := model.User{
		Username: "models-user",
		Status:   common.UserStatusEnabled,
		Group:    "default",
		Quota:    100,
	}
	require.NoError(t, model.DB.Create(&user).Error)
	require.NoError(t, model.DB.Create(&model.Token{
		UserId:         user.Id,
		Key:            "modelstestkey",
		Status:         common.TokenStatusEnabled,
		ExpiredTime:    -1,
		UnlimitedQuota: true,
	}).Error)

	engine := gin.New()
	SetRelayRouter(engine)

	tests := []struct {
		name           string
		path           string
		headerName     string
		expectedObject string
		expectedField  string
	}{
		{
			name:           "OpenAI bearer token",
			path:           "/v1/models",
			headerName:     "Authorization",
			expectedObject: "list",
			expectedField:  "data",
		},
		{
			name:          "Gemini API key header",
			path:          "/v1/models",
			headerName:    "x-goog-api-key",
			expectedField: "models",
		},
		{
			name:          "Gemini API key query",
			path:          "/v1/models?key=modelstestkey",
			expectedField: "models",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, test.path, nil)
			if test.headerName != "" {
				value := "modelstestkey"
				if test.headerName == "Authorization" {
					value = "Bearer " + value
				}
				request.Header.Set(test.headerName, value)
			}

			engine.ServeHTTP(recorder, request)

			require.Equal(t, http.StatusOK, recorder.Code)
			var payload map[string]any
			require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &payload))
			assert.Contains(t, payload, test.expectedField)
			assert.NotContains(t, payload, "error")
			if test.expectedObject != "" {
				assert.Equal(t, test.expectedObject, payload["object"])
			}
		})
	}
}

func TestGeminiCachedContentRoutesRequireToken(t *testing.T) {
	setupRelayRouterTestDB(t)
	require.NoError(t, i18n.Init())
	require.NoError(t, model.DB.AutoMigrate(&model.GeminiCachedContent{}))
	user := model.User{Username: "cache-owner", Status: common.UserStatusEnabled, Group: "default", Quota: 100}
	require.NoError(t, model.DB.Create(&user).Error)
	require.NoError(t, model.DB.Create(&model.Token{UserId: user.Id, Key: "cachetestkey", Status: common.TokenStatusEnabled, ExpiredTime: -1, UnlimitedQuota: true}).Error)
	engine := gin.New()
	SetRelayRouter(engine)
	for _, header := range []string{"Authorization", "x-goog-api-key", "query", ""} {
		t.Run(header, func(t *testing.T) {
			path := "/v1beta/cachedContents"
			if header == "query" {
				path += "?key=cachetestkey"
			}
			request := httptest.NewRequest(http.MethodGet, path, nil)
			if header == "Authorization" {
				request.Header.Set(header, "Bearer cachetestkey")
			} else if header == "x-goog-api-key" {
				request.Header.Set(header, "cachetestkey")
			}
			response := httptest.NewRecorder()
			engine.ServeHTTP(response, request)
			if header == "" {
				assert.Equal(t, http.StatusUnauthorized, response.Code)
			} else {
				require.Equal(t, http.StatusOK, response.Code, response.Body.String())
				assert.JSONEq(t, `{"cachedContents":[]}`, response.Body.String())
			}
		})
	}
	limitEngine := gin.New()
	limitEngine.Use(func(c *gin.Context) { c.Set(common.KeyBodyStorage, oversizedGeminiCacheBody{}) })
	SetRelayRouter(limitEngine)
	for _, method := range []string{http.MethodPost, http.MethodPatch} {
		path := "/v1beta/cachedContents"
		if method == http.MethodPatch {
			path += "/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		}
		request := httptest.NewRequest(method, path, nil)
		request.Header.Set("x-goog-api-key", "cachetestkey")
		response := httptest.NewRecorder()
		limitEngine.ServeHTTP(response, request)
		assert.Equal(t, http.StatusRequestEntityTooLarge, response.Code, response.Body.String())
	}
}

// Embedding intentionally leaves Bytes unimplemented: a rejected body must
// never be read in full, even if its storage was previously spooled to disk.
type oversizedGeminiCacheBody struct{ common.BodyStorage }

func (oversizedGeminiCacheBody) Size() int64                    { return service.GeminiCacheMaxRequestBytes + 1 }
func (oversizedGeminiCacheBody) Seek(int64, int) (int64, error) { return 0, nil }
func (oversizedGeminiCacheBody) Close() error                   { return nil }

func setupRelayRouterTestDB(t *testing.T) {
	t.Helper()

	gin.SetMode(gin.TestMode)
	originalIsMasterNode := common.IsMasterNode
	originalRedisEnabled := common.RedisEnabled
	originalSQLitePath := common.SQLitePath
	originalMainDatabaseType := common.MainDatabaseType()
	originalLogDatabaseType := common.LogDatabaseType()
	originalSQLDSN, hadSQLDSN := os.LookupEnv("SQL_DSN")

	common.IsMasterNode = false
	common.RedisEnabled = false
	common.SQLitePath = fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))
	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
	require.NoError(t, os.Setenv("SQL_DSN", "local"))
	require.NoError(t, model.InitDB())
	model.LOG_DB = model.DB
	require.NoError(t, model.DB.AutoMigrate(&model.User{}, &model.Token{}, &model.Ability{}))

	t.Cleanup(func() {
		if sqlDB, err := model.DB.DB(); err == nil {
			_ = sqlDB.Close()
		}
		common.IsMasterNode = originalIsMasterNode
		common.RedisEnabled = originalRedisEnabled
		common.SQLitePath = originalSQLitePath
		common.SetDatabaseTypes(originalMainDatabaseType, originalLogDatabaseType)
		if hadSQLDSN {
			require.NoError(t, os.Setenv("SQL_DSN", originalSQLDSN))
		} else {
			require.NoError(t, os.Unsetenv("SQL_DSN"))
		}
	})
}

func TestGeminiCacheLifecycle(t *testing.T) {
	for _, providerType := range []int{constant.ChannelTypeGemini, constant.ChannelTypeVertexAi, constant.ChannelTypeNewAPI, constant.ChannelTypeOpenAI, constant.ChannelTypeAdvancedCustom} {
		t.Run(fmt.Sprint(providerType), func(t *testing.T) { testGeminiCacheLifecycle(t, providerType) })
	}
}

type geminiCacheTokenTransport struct{}

func (geminiCacheTokenTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.String() != "https://oauth2.googleapis.com/token" {
		return nil, errors.New("unexpected OAuth endpoint")
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"access_token":"vertex-test-token","token_type":"Bearer","expires_in":3600}`)), Request: r}, nil
}

func testGeminiCacheLifecycle(t *testing.T, providerType int) {
	require.NoError(t, i18n.Init())
	setupRelayRouterTestDB(t)
	require.NoError(t, model.DB.AutoMigrate(&model.Channel{}, &model.GeminiCachedContent{}, &model.Log{}))
	previousStreamingTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = previousStreamingTimeout })
	previousMemory, previousBatch, previousLogs := common.MemoryCacheEnabled, common.BatchUpdateEnabled, common.LogConsumeEnabled
	common.MemoryCacheEnabled, common.BatchUpdateEnabled, common.LogConsumeEnabled = false, false, true
	t.Cleanup(func() {
		common.MemoryCacheEnabled, common.BatchUpdateEnabled, common.LogConsumeEnabled = previousMemory, previousBatch, previousLogs
	})
	settings := config.GlobalConfig.Get("billing_setting").(*billing_setting.BillingSetting)
	previousSettings := *settings
	t.Cleanup(func() { *settings = previousSettings })
	// Refunds enqueue cache updates after the database refund. Drain those
	// workers before restoring globals or closing this fixture's database.
	t.Cleanup(func() {
		require.Eventually(t, func() bool { return gopool.WorkerCount() == 0 }, 5*time.Second, time.Millisecond)
	})
	settings.BillingMode = map[string]string{"gemini-2.5-flash": billing_setting.BillingModeTieredExpr, "gemini-2.5-flash-cache-create": billing_setting.BillingModeTieredExpr, "gemini-2.5-flash-cache-storage": billing_setting.BillingModeTieredExpr}
	settings.BillingExpr = map[string]string{"gemini-2.5-flash": `tier("generation",p*0.3+c*2.5)`, "gemini-2.5-flash-cache-create": `tier("cache",p*0.3+p*(param("cache_storage_hours")??0))`, "gemini-2.5-flash-cache-storage": `tier("storage",p*(param("cache_storage_hours")??0))`}
	service.InitHttpClient()
	cacheKey, disabledKey := "upstream-original", "upstream-disabled"
	collectionPath := "/v1beta/cachedContents"
	resourceName := "cachedContents/private-upstream"
	resourceModel := "models/gemini-2.5-flash"
	if providerType == constant.ChannelTypeVertexAi {
		privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
		require.NoError(t, err)
		credentials := service.VertexCredentials{ProjectID: "test-project", ClientEmail: "cache@example.invalid", PrivateKey: string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(privateKey)}))}
		keyJSON, err := common.Marshal(credentials)
		require.NoError(t, err)
		cacheKey = string(keyJSON)
		credentials.ProjectID = "disabled-project"
		disabledJSON, err := common.Marshal(credentials)
		require.NoError(t, err)
		disabledKey = string(disabledJSON)
		credentials.ProjectID = "test-project"
		_, err = service.VertexAccessToken(context.Background(), credentials, &http.Client{Transport: geminiCacheTokenTransport{}})
		require.NoError(t, err)
		collectionPath = "/v1/projects/test-project/locations/us-central1/cachedContents"
		// Real Google responses may replace project ID with its project number.
		resourceName = "projects/123456/locations/us-central1/cachedContents/private-upstream"
		resourceModel = "projects/123456/locations/us-central1/publishers/google/models/gemini-2.5-flash"
	}
	var mu sync.Mutex
	var upstreamExpiry string
	cacheTotalTokens := 2048
	var cleanupObserved atomic.Bool
	var failedStateCommit atomic.Int32
	var uncertainCreate atomic.Int32
	var rejectDelete atomic.Bool
	var failedRollback atomic.Bool
	var patchAttempts atomic.Int32
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		if r.Method == http.MethodPatch && failedStateCommit.Load() == 2 {
			cleanupObserved.Store(true)
		}
		switch providerType {
		case constant.ChannelTypeGemini:
			assert.Equal(t, cacheKey, r.Header.Get("x-goog-api-key"))
		case constant.ChannelTypeVertexAi:
			assert.Equal(t, "Bearer vertex-test-token", r.Header.Get("Authorization"))
			assert.Equal(t, "test-project", r.Header.Get("x-goog-user-project"))
			assert.Empty(t, r.Header.Get("x-goog-api-key"))
		default:
			assert.Equal(t, "Bearer "+cacheKey, r.Header.Get("Authorization"))
			assert.Empty(t, r.Header.Get("x-goog-api-key"))
		}
		raw, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		var body map[string]any
		if len(raw) > 0 {
			require.NoError(t, common.Unmarshal(raw, &body))
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodDelete && rejectDelete.Load() {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		if r.Method == http.MethodPatch && failedRollback.Load() && patchAttempts.Add(1) > 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if (r.Method == http.MethodDelete && uncertainCreate.Load() != 0) ||
			(r.Method == http.MethodPost && r.URL.Path == collectionPath && uncertainCreate.Load() == 2) {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, ":countTokens"):
			if providerType == constant.ChannelTypeVertexAi {
				assert.NotContains(t, body, "generateContentRequest")
				assert.Contains(t, body, "contents")
				assert.Equal(t, "projects/test-project/locations/us-central1/publishers/google/models/gemini-2.5-flash", body["model"])
			} else {
				assert.Contains(t, body, "generateContentRequest")
			}
			_, _ = w.Write([]byte(`{"totalTokens":2048}`))
		case r.Method == http.MethodDelete:
			cleanupObserved.Store(true)
			_, _ = w.Write([]byte(`{}`))
		case strings.HasSuffix(r.URL.Path, ":generateContent"), strings.HasSuffix(r.URL.Path, ":streamGenerateContent"):
			assert.Equal(t, resourceName, body["cachedContent"])
			if providerType == constant.ChannelTypeVertexAi {
				assert.NotContains(t, string(raw), `"id":"call_cache"`, "Vertex strips function call and response IDs")
			}
			response := `{"candidates":[{"content":{"role":"model","parts":[{"text":"cached answer"}]},"finishReason":"STOP","index":0}],"usageMetadata":{"promptTokenCount":2058,"candidatesTokenCount":3,"totalTokenCount":2061,"cachedContentTokenCount":2048},"modelVersion":"gemini-2.5-flash"}`
			if strings.HasSuffix(r.URL.Path, ":streamGenerateContent") {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprintf(w, "data: %s\n\n", response)
			} else {
				_, _ = w.Write([]byte(response))
			}
		default:
			if value, ok := body["expireTime"].(string); ok {
				upstreamExpiry = value
			}
			response, err := common.Marshal(map[string]any{"name": resourceName, "model": resourceModel, "expireTime": upstreamExpiry, "usageMetadata": map[string]any{"totalTokenCount": cacheTotalTokens}})
			require.NoError(t, err)
			_, _ = w.Write(response)
		}
	}))
	defer server.Close()
	user := model.User{Username: "cache-lifecycle", AffCode: common.GetUUID(), Status: common.UserStatusEnabled, Group: "default", Quota: 1000000, Setting: `{"billing_preference":"wallet_only"}`}
	require.NoError(t, model.DB.Create(&user).Error)
	token := model.Token{UserId: user.Id, Key: "cachelifecyclekey", Status: common.TokenStatusEnabled, ExpiredTime: -1, RemainQuota: 1000000, Group: "default"}
	require.NoError(t, model.DB.Create(&token).Error)
	otherToken := model.Token{UserId: user.Id, Key: "cacheotherkey", Status: common.TokenStatusEnabled, ExpiredTime: -1, RemainQuota: 1000000, Group: "default"}
	require.NoError(t, model.DB.Create(&otherToken).Error)
	channel := model.Channel{Type: providerType, Status: common.ChannelStatusEnabled, Key: cacheKey + "\n" + disabledKey, Name: "cache lifecycle", Models: "gemini-2.5-flash", Group: "default", BaseURL: &server.URL, Setting: common.GetPointer(`{"gemini_explicit_cache_enabled":true}`), ChannelInfo: model.ChannelInfo{IsMultiKey: true, MultiKeyStatusList: map[int]int{1: 2}}}
	if providerType == constant.ChannelTypeVertexAi {
		channel.Other = `{"gemini-2.5-flash":"us-central1","default":"europe-west4"}`
	}
	if providerType == constant.ChannelTypeAdvancedCustom {
		routes := []dto.AdvancedCustomRoute{}
		for _, action := range []string{"generateContent", "streamGenerateContent"} {
			path := "/v1beta/models/{model}:" + action
			routes = append(routes, dto.AdvancedCustomRoute{IncomingPath: path, UpstreamPath: path})
		}
		channel.SetOtherSettings(dto.ChannelOtherSettings{AdvancedCustom: &dto.AdvancedCustomConfig{Routes: routes}})
	}
	require.NoError(t, channel.Insert())
	unsupported := model.Channel{Type: constant.ChannelTypeGemini, Status: common.ChannelStatusEnabled, Key: "unused", Name: "higher priority without cache support", Models: "gemini-2.5-flash", Group: "default", BaseURL: common.GetPointer("https://cache-disabled.invalid"), Priority: common.GetPointer(int64(100))}
	require.NoError(t, unsupported.Insert())
	engine := gin.New()
	SetRelayRouter(engine)
	doRequest := func(method, path, body, key string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+key)
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		engine.ServeHTTP(response, request)
		return response
	}
	createBody := `{"model":"models/gemini-2.5-flash","ttl":"3600s","contents":[{"role":"user","parts":[{"text":"cached source"}]}]}`
	configuredPrice := settings.BillingExpr["gemini-2.5-flash-cache-create"]
	delete(settings.BillingExpr, "gemini-2.5-flash-cache-create")
	missingPrice := doRequest(http.MethodPost, "/v1beta/cachedContents", createBody, token.Key)
	require.NotEqual(t, http.StatusOK, missingPrice.Code)
	mu.Lock()
	assert.Empty(t, calls, "missing cache price must prevent even countTokens")
	mu.Unlock()
	settings.BillingExpr["gemini-2.5-flash-cache-create"] = configuredPrice
	response := doRequest(http.MethodPost, "/v1beta/cachedContents", `{"model":"models/gemini-2.5-flash","ttl":"3600s","contents":[{"role":"user","parts":[{"text":"a sufficiently large cached source"}]}]}`, token.Key)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var resource map[string]any
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &resource))
	name, ok := resource["name"].(string)
	require.True(t, ok)
	assert.NotEqual(t, "cachedContents/private-upstream", name)
	require.NotContains(t, response.Body.String(), "private-upstream")
	var binding model.GeminiCachedContent
	require.NoError(t, model.DB.Where("public_id = ?", strings.TrimPrefix(name, "cachedContents/")).First(&binding).Error)
	assert.Equal(t, model.GeminiCachedContentActive, binding.State)
	assert.Equal(t, service.GeminiCacheKeyFingerprint(cacheKey), binding.KeyFingerprint)
	var afterCreate model.User
	require.NoError(t, model.DB.First(&afterCreate, user.Id).Error)
	assert.Less(t, afterCreate.Quota, user.Quota)
	response = doRequest(http.MethodGet, "/v1beta/"+name, "", token.Key)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	response = doRequest(http.MethodGet, "/v1beta/cachedContents", "", token.Key)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), name)
	response = doRequest(http.MethodGet, "/v1beta/"+name, "", otherToken.Key)
	assert.NotEqual(t, http.StatusOK, response.Code)
	require.NoError(t, model.DB.Model(&token).Updates(map[string]any{"model_limits_enabled": true, "model_limits": "gemini-2.5-pro"}).Error)
	response = doRequest(http.MethodGet, "/v1beta/"+name, "", token.Key)
	assert.NotEqual(t, http.StatusOK, response.Code)
	require.NoError(t, model.DB.Model(&token).Updates(map[string]any{"model_limits_enabled": false, "model_limits": ""}).Error)
	require.NoError(t, model.DB.Model(&binding).Update("group", "other-group").Error)
	response = doRequest(http.MethodGet, "/v1beta/"+name, "", token.Key)
	assert.NotEqual(t, http.StatusOK, response.Code)
	require.NoError(t, model.DB.Model(&binding).Update("group", "default").Error)

	// Reordering credentials must keep the same upstream account and update its index.
	channel.Key = disabledKey + "\n" + cacheKey
	channel.ChannelInfo.MultiKeyStatusList = map[int]int{0: 2}
	require.NoError(t, model.DB.Save(&channel).Error)
	generateBody := `{"cachedContent":"` + name + `","contents":[{"role":"user","parts":[{"text":"summarize"}]}]}`
	for _, action := range []string{"generateContent", "streamGenerateContent"} {
		response = doRequest(http.MethodPost, "/v1beta/models/gemini-2.5-flash:"+action, generateBody, token.Key)
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		assert.Contains(t, response.Body.String(), "cached answer")
	}
	// Both client protocols reuse the same native provider even with global and
	// per-channel passthrough enabled. Responses routing must not swallow cache.
	global := model_setting.GetGlobalSettings()
	previousPassThrough := global.PassThroughRequestEnabled
	t.Cleanup(func() { global.PassThroughRequestEnabled = previousPassThrough })
	global.PassThroughRequestEnabled = true
	channel.Setting = common.GetPointer(`{"gemini_explicit_cache_enabled":true,"pass_through_body_enabled":true}`)
	require.NoError(t, model.DB.Save(&channel).Error)
	chatBody := `{"model":"gemini-2.5-flash","messages":[{"role":"user","content":"summarize"}],"extra_body":{"google":{"cached_content":"` + name + `"}}}`
	for _, stream := range []bool{false, true} {
		body := strings.TrimSuffix(chatBody, "}") + fmt.Sprintf(`,"stream":%t}`, stream)
		response = doRequest(http.MethodPost, "/v1/chat/completions", body, token.Key)
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		assert.Contains(t, response.Body.String(), "cached answer")
		assert.Contains(t, response.Body.String(), "choices")
		assert.NotContains(t, response.Body.String(), "private-upstream")
	}
	if providerType == constant.ChannelTypeVertexAi {
		nativeTools := `{"cachedContent":"` + name + `","contents":[{"role":"model","parts":[{"functionCall":{"id":"call_cache","name":"lookup","args":{}}}]},{"role":"user","parts":[{"functionResponse":{"id":"call_cache","name":"lookup","response":{"result":"ok"}}}]}]}`
		response = doRequest(http.MethodPost, "/v1beta/models/gemini-2.5-flash:generateContent", nativeTools, token.Key)
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		chatTools := `{"model":"gemini-2.5-flash","messages":[{"role":"user","content":"lookup"},{"role":"assistant","tool_calls":[{"id":"call_cache","type":"function","function":{"name":"lookup","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_cache","content":"ok"}],"extra_body":{"google":{"cached_content":"` + name + `"}}}`
		response = doRequest(http.MethodPost, "/v1/chat/completions", chatTools, token.Key)
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	}
	response = doRequest(http.MethodPost, "/v1/chat/completions", chatBody, otherToken.Key)
	assert.NotEqual(t, http.StatusOK, response.Code)
	global.PassThroughRequestEnabled = previousPassThrough
	channel.Setting = common.GetPointer(`{"gemini_explicit_cache_enabled":true}`)
	require.NoError(t, model.DB.Save(&channel).Error)
	if providerType == constant.ChannelTypeVertexAi {
		oldRegion := channel.Other
		channel.Other = "europe-west4"
		require.NoError(t, model.DB.Save(&channel).Error)
		response = doRequest(http.MethodPost, "/v1/chat/completions", chatBody, token.Key)
		assert.NotEqual(t, http.StatusOK, response.Code)
		channel.Other = oldRegion
		require.NoError(t, model.DB.Save(&channel).Error)
	}
	response = doRequest(http.MethodPost, "/v1beta/models/gemini-2.5-pro:generateContent", generateBody, token.Key)
	assert.NotEqual(t, http.StatusOK, response.Code)
	response = doRequest(http.MethodPost, "/v1beta/models/gemini-2.5-flash:generateContent", generateBody, otherToken.Key)
	assert.NotEqual(t, http.StatusOK, response.Code)
	mu.Lock()
	beforeRejected := len(calls)
	mu.Unlock()
	channel.ModelMapping = common.GetPointer(`{"gemini-2.5-flash":"gemini-2.5-pro"}`)
	require.NoError(t, model.DB.Save(&channel).Error)
	response = doRequest(http.MethodPost, "/v1beta/models/gemini-2.5-flash:generateContent", generateBody, token.Key)
	require.NotEqual(t, http.StatusOK, response.Code)
	channel.ModelMapping = common.GetPointer(`{"gemini-2.5-flash":"models/gemini-2.5-flash"}`)
	require.NoError(t, model.DB.Save(&channel).Error)
	response = doRequest(http.MethodPost, "/v1/chat/completions", chatBody, token.Key)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	response = doRequest(http.MethodPost, "/v1beta/models/gemini-2.5-flash:generateContent", generateBody, token.Key)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	mu.Lock()
	beforeRejected = len(calls)
	mu.Unlock()
	channel.ModelMapping = nil
	require.NoError(t, model.DB.Save(&channel).Error)
	originalExpiry := binding.ExpiresAt
	require.NoError(t, model.DB.Model(&binding).Update("expires_at", time.Now().Add(-time.Second).Unix()).Error)
	response = doRequest(http.MethodPost, "/v1beta/models/gemini-2.5-flash:generateContent", generateBody, token.Key)
	require.NotEqual(t, http.StatusOK, response.Code)
	require.NoError(t, model.DB.Model(&binding).Update("expires_at", originalExpiry).Error)
	mu.Lock()
	assert.Len(t, calls, beforeRejected, "expired resources and mapping drift must not reach upstream")
	mu.Unlock()
	response = doRequest(http.MethodPatch, "/v1beta/"+name+"?updateMask=ttl", `{"ttl":"7200s"}`, token.Key)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.NoError(t, model.DB.First(&binding, binding.ID).Error)
	assert.Equal(t, model.GeminiCachedContentActive, binding.State)
	assert.Greater(t, binding.PaidUntil, time.Now().Add(time.Hour).Unix())
	var afterExtend model.User
	require.NoError(t, model.DB.First(&afterExtend, user.Id).Error)
	response = doRequest(http.MethodPatch, "/v1beta/"+name, `{"ttl":"3600s"}`, token.Key)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var afterShrink model.User
	require.NoError(t, model.DB.First(&afterShrink, user.Id).Error)
	assert.Equal(t, afterExtend.Quota, afterShrink.Quota)
	paidExpiry := time.Unix(binding.PaidUntil, 0).UTC().Format(time.RFC3339)
	response = doRequest(http.MethodPatch, "/v1beta/"+name, `{"expireTime":"`+paidExpiry+`"}`, token.Key)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var afterRestore model.User
	require.NoError(t, model.DB.First(&afterRestore, user.Id).Error)
	assert.Equal(t, afterShrink.Quota, afterRestore.Quota, "restoring previously paid storage must not charge twice")

	rejectDelete.Store(true)
	response = doRequest(http.MethodDelete, "/v1beta/"+name, "", token.Key)
	require.Equal(t, http.StatusTooManyRequests, response.Code)
	require.NoError(t, model.DB.First(&binding, binding.ID).Error)
	assert.Equal(t, model.GeminiCachedContentActive, binding.State, "a definite rejection must release the mutation lease")
	rejectDelete.Store(false)
	response = doRequest(http.MethodDelete, "/v1beta/"+name, "", token.Key)
	require.Contains(t, []int{http.StatusOK, http.StatusNoContent}, response.Code, response.Body.String())
	response = doRequest(http.MethodGet, "/v1beta/"+name, "", token.Key)
	assert.NotEqual(t, http.StatusOK, response.Code)
	response = doRequest(http.MethodPost, "/v1beta/models/gemini-2.5-flash:generateContent", generateBody, token.Key)
	assert.NotEqual(t, http.StatusOK, response.Code)
	require.NoError(t, model.DB.First(&binding, binding.ID).Error)
	assert.Equal(t, model.GeminiCachedContentDeleted, binding.State)
	var logs []model.Log
	require.NoError(t, model.DB.Where("user_id = ?", user.Id).Find(&logs).Error)
	assert.GreaterOrEqual(t, len(logs), 4)
	mu.Lock()
	assert.Contains(t, calls, "POST "+collectionPath)
	deletePath := "/v1beta/" + resourceName
	if providerType == constant.ChannelTypeVertexAi {
		deletePath = "/v1/" + resourceName
	}
	assert.Contains(t, calls, "DELETE "+deletePath)
	mu.Unlock()
	// An upstream count exceeding countTokens must reserve the difference before
	// activation. If the wallet cannot fund it, delete the upstream cache and refund.
	require.NoError(t, model.DB.Model(&user).Update("quota", 2000).Error)
	require.NoError(t, model.DB.Model(&token).Update("remain_quota", 2000).Error)
	mu.Lock()
	cacheTotalTokens = 1_000_000
	mu.Unlock()
	cleanupObserved.Store(false)
	refunded := make(chan struct{}, 1)
	const callback = "gemini_cache_lifecycle_refund"
	require.NoError(t, model.DB.Callback().Update().After("gorm:commit_or_rollback_transaction").Register(callback, func(tx *gorm.DB) {
		if cleanupObserved.Load() && tx.Statement.Table == "tokens" && tx.Error == nil {
			select {
			case refunded <- struct{}{}:
			default:
			}
		}
	}))
	t.Cleanup(func() { require.NoError(t, model.DB.Callback().Update().Remove(callback)) })
	response = doRequest(http.MethodPost, "/v1beta/cachedContents", createBody, token.Key)
	require.NotEqual(t, http.StatusOK, response.Code, response.Body.String())
	require.True(t, cleanupObserved.Load(), "failed supplemental reservation must delete the cache")
	select {
	case <-refunded:
	case <-time.After(5 * time.Second):
		t.Fatal("cache refund did not finish")
	}
	var refundedUser model.User
	var refundedToken model.Token
	require.NoError(t, model.DB.First(&refundedUser, user.Id).Error)
	require.NoError(t, model.DB.First(&refundedToken, token.Id).Error)
	assert.Equal(t, 2000, refundedUser.Quota)
	assert.Equal(t, 2000, refundedToken.RemainQuota)
	var active int64
	require.NoError(t, model.DB.Model(&model.GeminiCachedContent{}).Where("state = ?", model.GeminiCachedContentActive).Count(&active).Error)
	assert.Zero(t, active)
	const stateCallback = "gemini_cache_lifecycle_state_failure"
	require.NoError(t, model.DB.Callback().Update().Before("gorm:update").Register(stateCallback, func(tx *gorm.DB) {
		if tx.Statement.Table != "gemini_cached_contents" {
			return
		}
		values, ok := tx.Statement.Dest.(map[string]any)
		if !ok || values["state"] != model.GeminiCachedContentActive {
			return
		}
		_, activates := values["upstream_name"]
		_, finishes := values["expires_at"]
		mode := failedStateCommit.Load()
		if (mode == 1 && activates) || (mode == 2 && finishes && !activates) {
			tx.AddError(errors.New("injected cached content state commit failure"))
		}
	}))
	t.Cleanup(func() { require.NoError(t, model.DB.Callback().Update().Remove(stateCallback)) })
	mu.Lock()
	cacheTotalTokens = 2048
	mu.Unlock()
	require.NoError(t, model.DB.Model(&user).Update("quota", 10000).Error)
	require.NoError(t, model.DB.Model(&token).Update("remain_quota", 10000).Error)
	// The observer is reused below, so discard setup updates before the next reserve.
	select {
	case <-refunded:
	default:
	}
	cleanupObserved.Store(false)
	failedStateCommit.Store(1)
	response = doRequest(http.MethodPost, "/v1beta/cachedContents", createBody, token.Key)
	require.NotEqual(t, http.StatusOK, response.Code)
	require.True(t, cleanupObserved.Load())
	select {
	case <-refunded:
	case <-time.After(5 * time.Second):
		t.Fatal("activation failure refund did not finish")
	}
	require.NoError(t, model.DB.First(&refundedUser, user.Id).Error)
	require.NoError(t, model.DB.First(&refundedToken, token.Id).Error)
	assert.Equal(t, 10000, refundedUser.Quota)
	assert.Equal(t, 10000, refundedToken.RemainQuota)
	require.NoError(t, model.DB.Model(&model.GeminiCachedContent{}).Where("state = ?", model.GeminiCachedContentActive).Count(&active).Error)
	assert.Zero(t, active)
	failedStateCommit.Store(0)
	cleanupObserved.Store(false)
	response = doRequest(http.MethodPost, "/v1beta/cachedContents", createBody, token.Key)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &resource))
	secondName := resource["name"].(string)
	var beforeFailedPatch model.User
	require.NoError(t, model.DB.First(&beforeFailedPatch, user.Id).Error)
	var secondBinding model.GeminiCachedContent
	require.NoError(t, model.DB.Where("public_id = ?", strings.TrimPrefix(secondName, "cachedContents/")).First(&secondBinding).Error)
	priorExpiry, priorPaid := secondBinding.ExpiresAt, secondBinding.PaidUntil
	failedStateCommit.Store(2)
	response = doRequest(http.MethodPatch, "/v1beta/"+secondName, `{"ttl":"7200s"}`, token.Key)
	require.NotEqual(t, http.StatusOK, response.Code)
	select {
	case <-refunded:
	case <-time.After(5 * time.Second):
		t.Fatal("mutation failure refund did not finish")
	}
	require.NoError(t, model.DB.First(&refundedUser, user.Id).Error)
	assert.Equal(t, beforeFailedPatch.Quota, refundedUser.Quota)
	require.NoError(t, model.DB.First(&secondBinding, secondBinding.ID).Error)
	assert.Equal(t, model.GeminiCachedContentActive, secondBinding.State)
	assert.Equal(t, priorExpiry, secondBinding.ExpiresAt)
	assert.Equal(t, priorPaid, secondBinding.PaidUntil)
	mu.Lock()
	assert.Equal(t, time.Unix(priorExpiry, 0).UTC().Format(time.RFC3339), upstreamExpiry)
	mu.Unlock()
	failedStateCommit.Store(0)
	// An unconfirmed rollback must keep both the lease and the paid reservation.
	require.NoError(t, model.DB.First(&beforeFailedPatch, user.Id).Error)
	failedStateCommit.Store(2)
	failedRollback.Store(true)
	response = doRequest(http.MethodPatch, "/v1beta/"+secondName, `{"ttl":"7200s"}`, token.Key)
	require.NotEqual(t, http.StatusOK, response.Code)
	require.NoError(t, model.DB.First(&refundedUser, user.Id).Error)
	assert.Less(t, refundedUser.Quota, beforeFailedPatch.Quota)
	require.NoError(t, model.DB.First(&secondBinding, secondBinding.ID).Error)
	assert.Equal(t, model.GeminiCachedContentMutating, secondBinding.State)
	assert.Equal(t, priorPaid, secondBinding.PaidUntil)
	failedRollback.Store(false)
	failedStateCommit.Store(0)

	for _, tc := range []struct {
		name         string
		mode         int32
		tokens       int
		stateFailure int32
	}{
		{"activation cleanup failed", 1, 2048, 1},
		{"create outcome unknown", 2, 2048, 0},
		{"invalid response cleanup failed", 3, service.GeminiCacheMaxTokens + 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			uncertainCreate.Store(tc.mode)
			failedStateCommit.Store(tc.stateFailure)
			mu.Lock()
			cacheTotalTokens = tc.tokens
			mu.Unlock()
			require.NoError(t, model.DB.Model(&user).Update("quota", 10000).Error)
			require.NoError(t, model.DB.Model(&token).Update("remain_quota", 10000).Error)
			response = doRequest(http.MethodPost, "/v1beta/cachedContents", createBody, token.Key)
			require.NotEqual(t, http.StatusOK, response.Code)
			require.NoError(t, model.DB.First(&refundedUser, user.Id).Error)
			require.NoError(t, model.DB.First(&refundedToken, token.Id).Error)
			assert.Less(t, refundedUser.Quota, 10000)
			assert.Equal(t, refundedUser.Quota, refundedToken.RemainQuota)
			var pending model.GeminiCachedContent
			require.NoError(t, model.DB.Order("id DESC").First(&pending).Error)
			assert.Equal(t, model.GeminiCachedContentPending, pending.State)
			if tc.mode != 2 {
				assert.Equal(t, resourceName, pending.UpstreamName)
			}
			var audit model.Log
			require.NoError(t, model.DB.Where("type = ?", model.LogTypeError).Order("id DESC").First(&audit).Error)
			assert.Contains(t, audit.Other, pending.PublicID)
			assert.Contains(t, audit.Other, `"cache_reconciliation_required":true`)
			assert.Contains(t, audit.Other, `"cache_reserved_quota":`)
		})
	}
}
