package middleware

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	hostdto "github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/model_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestChannelMatchesExpectedTaskPluginUsesGenericChannelSetting(t *testing.T) {
	channel := &model.Channel{Type: constant.ChannelTypeTaskPlugin}
	channel.SetSetting(dto.ChannelSettings{TaskPluginKey: "generic-alpha"})

	assert.True(t, channelMatchesExpectedTaskPlugin(nil, channel, "generic-alpha"))
	assert.False(t, channelMatchesExpectedTaskPlugin(nil, channel, "generic-beta"))
	assert.False(t, channelMatchesExpectedTaskPlugin(nil, channel, ""))
}

func TestChannelMatchesExpectedTaskPluginUsesPinnedLegacyIndex(t *testing.T) {
	registry := jsplugin.NewRegistry()
	alpha, err := registry.Register(distributorTaskPluginSource("legacy-alpha", constant.ChannelTypeKling), jsplugin.Options{})
	require.NoError(t, err)
	pinnedGeneration := registry.Generation()

	require.NoError(t, registry.Unregister("legacy-alpha"))
	_, err = registry.Register(distributorTaskPluginSource("legacy-beta", constant.ChannelTypeKling), jsplugin.Options{})
	require.NoError(t, err)

	c, _ := gin.CreateTestContext(nil)
	c.Set(jsplugin.ContextKeyPinnedPlugin, jsplugin.PinnedPlugin{
		Generation: pinnedGeneration,
		Plugin:     alpha,
	})
	channel := &model.Channel{Type: constant.ChannelTypeKling}

	assert.True(t, channelMatchesExpectedTaskPlugin(c, channel, "legacy-alpha"))
	assert.False(t, channelMatchesExpectedTaskPlugin(c, channel, "legacy-beta"))
	assert.False(t, channelMatchesExpectedTaskPlugin(c, &model.Channel{Type: constant.ChannelTypeJimeng}, "legacy-alpha"))
}

func TestChannelMatchesExpectedTaskPluginRejectsUnindexedLegacyChannel(t *testing.T) {
	registry := jsplugin.NewRegistry()
	plugin, err := registry.Register(distributorTaskPluginSource("legacy-alpha", constant.ChannelTypeKling), jsplugin.Options{})
	require.NoError(t, err)

	c, _ := gin.CreateTestContext(nil)
	c.Set(jsplugin.ContextKeyPinnedPlugin, jsplugin.PinnedPlugin{
		Generation: registry.Generation(),
		Plugin:     plugin,
	})

	assert.False(t, channelMatchesExpectedTaskPlugin(c, &model.Channel{Type: constant.ChannelTypeJimeng}, "legacy-alpha"))
	assert.False(t, channelMatchesExpectedTaskPlugin(c, &model.Channel{Type: 0}, "legacy-alpha"))
	assert.True(t, channelMatchesExpectedTaskPlugin(c, &model.Channel{Type: constant.ChannelTypeJimeng}, ""))
	assert.False(t, channelMatchesExpectedTaskPlugin(nil, &model.Channel{Type: constant.ChannelTypeKling}, "legacy-alpha"))

	c.Set("expected_task_plugin_key", "legacy-alpha")
	setupErr := SetupContextForSelectedChannel(c, &model.Channel{Type: constant.ChannelTypeJimeng}, "task-model")
	require.NotNil(t, setupErr)
	assert.Contains(t, setupErr.Error(), "does not match")
}

func TestSharedEndpointRebindsToSelectedLegacyProvider(t *testing.T) {
	registry := jsplugin.NewRegistry()
	_, err := registry.Register(distributorEndpointPluginSource("gemini-shared", constant.ChannelTypeGemini), jsplugin.Options{})
	require.NoError(t, err)
	_, err = registry.Register(distributorEndpointPluginSource("vertex-shared", constant.ChannelTypeVertexAi), jsplugin.Options{})
	require.NoError(t, err)
	candidates := registry.Generation().LookupEndpointCandidates("POST", "/v1/responses", "task-model")
	require.Len(t, candidates, 2)

	c, _ := gin.CreateTestContext(nil)
	c.Set(jsplugin.ContextKeyPinnedPlugin, jsplugin.PinnedPlugin{Generation: registry.Generation(), Plugin: candidates[0].Plugin})
	c.Set(jsplugin.ContextKeyPinnedEndpoint, jsplugin.PinnedEndpoint{
		Generation: registry.Generation(),
		Plugin:     candidates[0].Plugin,
		Protocol:   candidates[0].Protocol,
		Operation:  candidates[0].Operation,
		Model:      "task-model",
		Candidates: candidates,
	})
	c.Set("expected_task_plugin_key", candidates[0].Plugin.Meta.Key)

	geminiChannel := &model.Channel{Id: 1, Type: constant.ChannelTypeGemini}
	vertexChannel := &model.Channel{Id: 2, Type: constant.ChannelTypeVertexAi}
	assert.True(t, channelMatchesExpectedTaskPlugin(c, geminiChannel, candidates[0].Plugin.Meta.Key))
	assert.True(t, channelMatchesExpectedTaskPlugin(c, vertexChannel, candidates[0].Plugin.Meta.Key))
	assert.False(t, channelMatchesExpectedTaskPlugin(c, &model.Channel{Type: constant.ChannelTypeKling}, candidates[0].Plugin.Meta.Key))

	require.Nil(t, SetupContextForSelectedChannel(c, vertexChannel, "task-model"))
	pinnedValue, exists := c.Get(jsplugin.ContextKeyPinnedEndpoint)
	require.True(t, exists)
	pinned, ok := pinnedValue.(jsplugin.PinnedEndpoint)
	require.True(t, ok)
	assert.Equal(t, "vertex-shared", pinned.Plugin.Meta.Key)
	assert.Equal(t, "vertex-shared", c.GetString("expected_task_plugin_key"))
	assert.Equal(t, "vertex-shared", c.GetString("task_plugin_key"))
	assert.True(t, channelMatchesExpectedTaskPlugin(c, geminiChannel, "vertex-shared"), "a retry may select another declared provider")
}

func distributorTaskPluginSource(key string, channelType int) string {
	return fmt.Sprintf(`
export const meta = {
  apiVersion: 1,
  key: %q,
  name: %q,
  version: "1.0.0",
  author: {name: "Test"},
  channelTypes: [%d],
  models: ["task-model"],
  fetchMode: "per_task",
};
export function buildSubmitRequest() { return {}; }
export function parseSubmitResponse() { return {taskId: "task"}; }
export function buildQueryRequest() { return {}; }
export function parseTaskResult() { return {status: "SUCCESS"}; }
`, key, key, channelType)
}

func distributorEndpointPluginSource(key string, channelType int) string {
	return fmt.Sprintf(`
export const meta = {
  apiVersion: 1,
  key: %q,
  name: %q,
  version: "1.0.0",
  author: {name: "Test"},
  channelTypes: [%d],
  models: ["task-model"],
  fetchMode: "per_task",
  protocols: [{name: "openai_responses", supports: ["stream", "sync", "background"]}],
};
export function buildSubmitRequest() { return {}; }
export function parseSubmitResponse() { return {taskId: "task"}; }
export function buildQueryRequest() { return {}; }
export function parseTaskResult() { return {status: "SUCCESS"}; }
export const protocols = {openai_responses: {
  decodeRequest: function(ctx) { return {kind: "submit", model: "task-model", requestBody: ctx.body.value}; },
  renderEvents: function() { return {events: [], state: null, done: false}; },
  renderFinal: function() { return {output: []}; },
}};
`, key, key, channelType)
}

func TestTokenModelLimitAllowsLegacyAliasAndModifierVariant(t *testing.T) {
	aliasOnly := map[string]bool{"claude-3-7-sonnet-thinking": true}
	assert.True(t, TokenModelLimitAllows(aliasOnly, "claude-3-7-sonnet-thinking"))
	assert.False(t, TokenModelLimitAllows(aliasOnly, "claude-3-7-sonnet"))

	baseOnly := map[string]bool{"claude-3-7-sonnet": true}
	assert.True(t, TokenModelLimitAllows(baseOnly, "claude-3-7-sonnet@thinking:on"))
	assert.True(t, TokenModelLimitAllows(baseOnly, "claude-3-7-sonnet-thinking"))

	wildcard := map[string]bool{"gemini-2.5-flash-thinking-*": true}
	assert.True(t, TokenModelLimitAllows(wildcard, "gemini-2.5-flash-thinking-8192"))
}

func TestTokenModelLimitAllowsExemptAtNameByFullName(t *testing.T) {
	settings := model_setting.GetGlobalSettings()
	original := append([]string(nil), settings.ThinkingModelBlacklist...)
	t.Cleanup(func() { settings.ThinkingModelBlacklist = original })
	settings.ThinkingModelBlacklist = append(original, "re:.*@sha256:.*")

	fullOnly := map[string]bool{"opaque@sha256:deadbeef": true}
	assert.True(t, TokenModelLimitAllows(fullOnly, "opaque@sha256:deadbeef"))

	baseOnly := map[string]bool{"opaque": true}
	assert.False(t, TokenModelLimitAllows(baseOnly, "opaque@sha256:deadbeef"))
}

func TestDistributeHidesTaskPluginDetailsButLogsDiagnostics(t *testing.T) {
	require.NoError(t, i18n.Init())
	previousCacheEnabled := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = true
	t.Cleanup(func() { common.MemoryCacheEnabled = previousCacheEnabled })

	const group = "private-plugin-error-test-group"
	for _, locale := range []struct{ language, message string }{
		{"en", "No available channel for model task-model under group " + group + ": the model is claimed by a task plugin, which has no enabled channel serving it (distributor)"},
		{"zh-CN", "分组 " + group + " 下模型 task-model 无可用渠道：该模型由任务插件认领，但当前没有启用的渠道可服务此模型（distributor）"},
		{"zh-TW", "分組 " + group + " 下模型 task-model 無可用管道：該模型由任務插件認領，但目前沒有啟用的管道可服務此模型（distributor）"},
	} {
		for _, providerCount := range []int{1, 2} {
			t.Run(fmt.Sprintf("%s/%d_providers", locale.language, providerCount), func(t *testing.T) {
				registry := jsplugin.NewRegistry()
				keys := []string{"private-provider-alpha", "private-provider-beta"}[:providerCount]
				for index, key := range keys {
					_, err := registry.Register(distributorEndpointPluginSource(key, constant.ChannelTypeKling+index), jsplugin.Options{})
					require.NoError(t, err)
				}
				generation := registry.Generation()
				candidates := generation.LookupEndpointCandidates("POST", "/v1/responses", "task-model")
				require.Len(t, candidates, providerCount)

				var logs bytes.Buffer
				common.LogWriterMu.Lock()
				previousWriter := gin.DefaultErrorWriter
				gin.DefaultErrorWriter = &logs
				common.LogWriterMu.Unlock()
				t.Cleanup(func() {
					common.LogWriterMu.Lock()
					gin.DefaultErrorWriter = previousWriter
					common.LogWriterMu.Unlock()
				})

				router := gin.New()
				router.POST("/v1/responses", RequestId(), func(c *gin.Context) {
					common.SetContextKey(c, constant.ContextKeyUsingGroup, group)
					c.Set("resolved_task_model", "task-model")
					c.Set("expected_task_plugin_key", keys[0])
					c.Set(jsplugin.ContextKeyPinnedPlugin, jsplugin.PinnedPlugin{Generation: generation, Plugin: candidates[0].Plugin})
					if providerCount > 1 {
						c.Set(jsplugin.ContextKeyPinnedEndpoint, jsplugin.PinnedEndpoint{
							Generation: generation, Plugin: candidates[0].Plugin,
							Protocol: candidates[0].Protocol, Operation: candidates[0].Operation,
							Model: "task-model", Candidates: candidates,
						})
					}
				}, Distribute(), func(c *gin.Context) {
					t.Error("unavailable requests must stop before the relay handler")
					c.Status(http.StatusNoContent)
				})
				request := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
				request.Header.Set("Accept-Language", locale.language)
				recorder := httptest.NewRecorder()
				router.ServeHTTP(recorder, request)

				require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
				requestID := recorder.Header().Get(common.RequestIdKey)
				require.NotEmpty(t, requestID)
				assert.JSONEq(t, fmt.Sprintf(`{"error":{"message":%q,"type":"new_api_error","code":"model_not_found"}}`,
					locale.message+" (request id: "+requestID+")"), recorder.Body.String())
				assert.NotContains(t, recorder.Body.String(), "disable or override")
				for _, key := range keys {
					assert.NotContains(t, recorder.Body.String(), key)
					assert.Contains(t, logs.String(), key)
				}
				assert.Contains(t, logs.String(), requestID)
				assert.Contains(t, logs.String(), `group="`+group+`"`)
				assert.Contains(t, logs.String(), `model="task-model"`)
				assert.Contains(t, logs.String(), "reason=no_eligible_channel")
			})
		}
	}
}

func TestNoAvailableChannelMessageWithoutPlugin(t *testing.T) {
	require.NoError(t, i18n.Init())
	plain, _ := gin.CreateTestContext(nil)
	plain.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	plain.Request.Header.Set("Accept-Language", "en")
	generic := noAvailableChannelMessage(plain, "default", "gpt-4o")
	assert.NotContains(t, generic, "task plugin")
	assert.Contains(t, generic, "gpt-4o")
}

func TestSharedEndpointRebindsToSelectedType61Plugin(t *testing.T) {
	registry := jsplugin.NewRegistry()
	for _, key := range []string{"alpha", "beta"} {
		source := strings.Replace(distributorEndpointPluginSource(key, 0), "channelTypes: [0],", "", 1)
		_, err := registry.Register(source, jsplugin.Options{})
		require.NoError(t, err)
	}
	generation := registry.Generation()
	candidates := generation.LookupEndpointCandidates("POST", "/v1/responses", "task-model")
	require.Len(t, candidates, 2)
	c, _ := gin.CreateTestContext(nil)
	c.Set(jsplugin.ContextKeyPinnedPlugin, jsplugin.PinnedPlugin{Generation: generation, Plugin: candidates[0].Plugin})
	c.Set(jsplugin.ContextKeyPinnedEndpoint, jsplugin.PinnedEndpoint{Generation: generation, Plugin: candidates[0].Plugin, Protocol: candidates[0].Protocol, Operation: candidates[0].Operation, Model: "task-model", Candidates: candidates})
	c.Set("expected_task_plugin_key", "alpha")
	channel := &model.Channel{Id: 2, Type: constant.ChannelTypeTaskPlugin}
	channel.SetSetting(dto.ChannelSettings{TaskPluginKey: "unrelated"})
	assert.False(t, channelMatchesExpectedTaskPlugin(c, channel, "alpha"))
	channel.SetSetting(dto.ChannelSettings{TaskPluginKey: "beta"})
	require.Nil(t, SetupContextForSelectedChannel(c, channel, "task-model"))
	assert.Equal(t, "beta", c.GetString("task_plugin_key"))
	assert.Equal(t, "beta", c.GetString("expected_task_plugin_key"))
	assert.Equal(t, "beta", c.MustGet(jsplugin.ContextKeyPinnedEndpoint).(jsplugin.PinnedEndpoint).Plugin.Meta.Key)
}

func TestChannelMatchesExpectedTaskPluginUsesNewAPIExtensions(t *testing.T) {
	channel := &model.Channel{Type: constant.ChannelTypeNewAPI}
	channel.SetSetting(dto.ChannelSettings{TaskPluginKey: "single", TaskExtendPluginKeys: []string{"ext-alpha", "ext-beta"}})

	assert.True(t, channelMatchesExpectedTaskPlugin(nil, channel, "ext-alpha"))
	assert.True(t, channelMatchesExpectedTaskPlugin(nil, channel, "single"), "the single key stays valid on a New API channel")
	assert.False(t, channelMatchesExpectedTaskPlugin(nil, channel, "ext-gamma"))
	assert.True(t, channelMatchesExpectedTaskPlugin(nil, channel, ""), "requests without a pinned plugin still use the gateway")
	assert.False(t, channelMatchesExpectedTaskPlugin(nil, &model.Channel{Type: constant.ChannelTypeNewAPI}, "ext-alpha"))
}

func TestSharedEndpointRebindsToBoundNewAPIExtension(t *testing.T) {
	registry := jsplugin.NewRegistry()
	for _, key := range []string{"alpha", "beta"} {
		source := strings.Replace(distributorEndpointPluginSource(key, 0), "channelTypes: [0],", "", 1)
		_, err := registry.Register(source, jsplugin.Options{})
		require.NoError(t, err)
	}
	generation := registry.Generation()
	candidates := generation.LookupEndpointCandidates("POST", "/v1/responses", "task-model")
	require.Len(t, candidates, 2)
	pin := func(expected string) *gin.Context {
		pinned := candidates[0]
		if candidates[1].Plugin.Meta.Key == expected {
			pinned = candidates[1]
		}
		c, _ := gin.CreateTestContext(nil)
		c.Set(jsplugin.ContextKeyPinnedPlugin, jsplugin.PinnedPlugin{Generation: generation, Plugin: pinned.Plugin})
		c.Set(jsplugin.ContextKeyPinnedEndpoint, jsplugin.PinnedEndpoint{Generation: generation, Plugin: pinned.Plugin, Protocol: pinned.Protocol, Operation: pinned.Operation, Model: "task-model", Candidates: candidates})
		c.Set("expected_task_plugin_key", expected)
		return c
	}

	channel := &model.Channel{Id: 3, Type: constant.ChannelTypeNewAPI}
	channel.SetSetting(dto.ChannelSettings{TaskExtendPluginKeys: []string{"unrelated"}})
	assert.False(t, channelMatchesExpectedTaskPlugin(pin("alpha"), channel, "alpha"))

	channel.SetSetting(dto.ChannelSettings{TaskExtendPluginKeys: []string{"beta"}})
	c := pin("alpha")
	require.Nil(t, SetupContextForSelectedChannel(c, channel, "task-model"))
	assert.Equal(t, "beta", c.GetString("task_plugin_key"), "the only bound candidate executes")
	assert.Equal(t, "beta", c.GetString("expected_task_plugin_key"))
	assert.Equal(t, "beta", c.MustGet(jsplugin.ContextKeyPinnedEndpoint).(jsplugin.PinnedEndpoint).Plugin.Meta.Key)

	channel.SetSetting(dto.ChannelSettings{TaskExtendPluginKeys: []string{"alpha", "beta"}})
	c = pin("beta")
	require.Nil(t, SetupContextForSelectedChannel(c, channel, "task-model"))
	assert.Equal(t, "alpha", c.GetString("task_plugin_key"), "the first bound candidate executes regardless of the earlier pin")
	assert.Equal(t, "alpha", c.MustGet(jsplugin.ContextKeyPinnedEndpoint).(jsplugin.PinnedEndpoint).Plugin.Meta.Key)
}

func TestGeminiCachePreparationPreservesVertexResourceNames(t *testing.T) {
	for _, action := range []string{"generateContent", "streamGenerateContent", "countTokens"} {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		body := `{"cachedContent":"projects/test-project/locations/us-central1/cachedContents/vertex-cache","contents":[{"role":"user","parts":[{"text":"continue"}]}]}`
		ctx.Request = httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-2.5-flash:"+action, strings.NewReader(body))
		t.Cleanup(func() { common.CleanupBodyStorage(ctx) })
		require.True(t, prepareGeminiCacheReference(ctx), recorder.Body.String())
		_, bound := common.GetContextKeyType[*model.GeminiCachedContent](ctx, constant.ContextKeyGeminiCachedContent)
		assert.False(t, bound, "Vertex resources must follow their existing channel selection")
		filters := service.GetChannelConstraints(ctx).Filters
		allowed, _ := model.ChannelSatisfiesFilters(&model.Channel{Type: constant.ChannelTypeVertexAi}, "gemini-2.5-flash", filters)
		assert.True(t, allowed)
		allowed, _ = model.ChannelSatisfiesFilters(&model.Channel{Type: constant.ChannelTypeGemini}, "gemini-2.5-flash", filters)
		assert.False(t, allowed)
		storage, err := common.GetBodyStorage(ctx)
		require.NoError(t, err)
		replayed, err := io.ReadAll(storage)
		require.NoError(t, err)
		assert.Equal(t, body, string(replayed))
	}
}

type geminiCacheDiskStorageProbe struct {
	*os.File
	size       int64
	wholeReads int
}

func (s *geminiCacheDiskStorageProbe) Bytes() ([]byte, error) {
	s.wholeReads++
	return nil, errors.New("full body materialization forbidden")
}
func (s *geminiCacheDiskStorageProbe) Size() int64  { return s.size }
func (s *geminiCacheDiskStorageProbe) IsDisk() bool { return true }
func (s *geminiCacheDiskStorageProbe) NewReader() (io.ReadCloser, error) {
	return os.Open(s.File.Name())
}

func TestGeminiCachePreparationStreamsOrdinaryDiskBody(t *testing.T) {
	for _, tc := range []struct{ path, body string }{
		{"/v1beta/models/gemini-2.5-flash:generateContent", `{"contents":[{"parts":[{"text":"cachedContent is ordinary text"}]}]}`},
		{"/v1/chat/completions", `{"model":"gemini-2.5-flash","messages":[{"role":"user","content":"cached_content is ordinary text"}]}`},
	} {
		t.Run(tc.path, func(t *testing.T) {
			file, err := os.Create(filepath.Join(t.TempDir(), "request.json"))
			require.NoError(t, err)
			_, err = file.WriteString(tc.body)
			require.NoError(t, err)
			storage := &geminiCacheDiskStorageProbe{File: file, size: int64(len(tc.body))}
			t.Cleanup(func() { require.NoError(t, file.Close()) })
			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx.Request = httptest.NewRequest(http.MethodPost, tc.path, nil)
			ctx.Set(common.KeyBodyStorage, storage)
			require.True(t, prepareGeminiCacheReference(ctx))
			assert.Zero(t, storage.wholeReads)
			replayed, err := io.ReadAll(storage)
			require.NoError(t, err)
			assert.Equal(t, tc.body, string(replayed), "independent scanner must leave the relay reader at the start")
		})
	}
}

func setupGeminiCacheRoutingDB(t *testing.T) {
	t.Helper()
	previousDB, previousLogDB := model.DB, model.LOG_DB
	previousMaster, previousPath := common.IsMasterNode, common.SQLitePath
	previousMainType, previousLogType := common.MainDatabaseType(), common.LogDatabaseType()
	previousMemory, previousRedis := common.MemoryCacheEnabled, common.RedisEnabled
	var database *gorm.DB
	t.Cleanup(func() {
		if database != nil {
			sqlDB, err := database.DB()
			require.NoError(t, err)
			require.NoError(t, sqlDB.Close())
		}
		model.DB, model.LOG_DB = previousDB, previousLogDB
		common.IsMasterNode, common.SQLitePath = previousMaster, previousPath
		common.SetDatabaseTypes(previousMainType, previousLogType)
		common.MemoryCacheEnabled, common.RedisEnabled = previousMemory, previousRedis
	})
	t.Setenv("SQL_DSN", "local")
	common.IsMasterNode = false
	common.SQLitePath = filepath.Join(t.TempDir(), "cache-routing.db")
	common.MemoryCacheEnabled, common.RedisEnabled = false, false
	require.NoError(t, model.InitDB())
	database = model.DB
	require.NoError(t, model.DB.AutoMigrate(&model.Channel{}, &model.GeminiCachedContent{}, &model.Ability{}))
}

func TestGeminiCacheExternalReferencesRetainNonGeminiRouting(t *testing.T) {
	require.NoError(t, i18n.Init())
	for _, provider := range []int{constant.ChannelTypeNewAPI, constant.ChannelTypeAdvancedCustom, constant.ChannelTypeVertexAi} {
		t.Run(fmt.Sprint(provider), func(t *testing.T) {
			setupGeminiCacheRoutingDB(t)
			gemini := model.Channel{Type: constant.ChannelTypeGemini, Name: "shared Gemini key", Status: common.ChannelStatusEnabled, Key: "shared-key", Models: "gemini-2.5-flash", Group: "default", Priority: common.GetPointer(int64(100))}
			require.NoError(t, gemini.Insert())
			external := model.Channel{Type: provider, Name: "external cache provider", Status: common.ChannelStatusEnabled, Key: "external-key", Models: "gemini-2.5-flash", Group: "default"}
			if provider == constant.ChannelTypeAdvancedCustom {
				external.OtherSettings = `{"advanced_custom":{"advanced_routes":[{"incoming_path":"/v1beta/models/{model}:generateContent","upstream_path":"/v1beta/models/{model}:generateContent"}]}}`
			}
			require.NoError(t, external.Insert())
			engine := gin.New()
			engine.POST("/v1beta/models/*model", func(c *gin.Context) {
				c.Set("id", 1)
				c.Set("token_id", 1)
				common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
				common.SetContextKey(c, constant.ContextKeyUserGroup, "default")
			}, Distribute(), func(c *gin.Context) {
				assert.Equal(t, external.Id, common.GetContextKeyInt(c, constant.ContextKeyChannelId))
				c.Status(http.StatusNoContent)
			})
			dispatchBody := func(body string) *httptest.ResponseRecorder {
				t.Helper()
				request := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-2.5-flash:generateContent", strings.NewReader(body))
				request.Header.Set("Content-Type", "application/json")
				response := httptest.NewRecorder()
				engine.ServeHTTP(response, request)
				return response
			}
			dispatch := func(name string) *httptest.ResponseRecorder {
				t.Helper()
				return dispatchBody(`{"cachedContent":"` + name + `","contents":[{"role":"user","parts":[{"text":"continue"}]}]}`)
			}
			const publicID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			for _, name := range []string{"cachedContents/foo", "cachedContents/" + publicID, "projects/project-id/locations/us-central1/cachedContents/cache-id"} {
				response := dispatch(name)
				require.Equal(t, http.StatusNoContent, response.Code, response.Body.String())
			}
			record := model.GeminiCachedContent{PublicID: publicID, UserID: 99, TokenID: 99, ChannelID: gemini.Id, Group: "default", Model: "gemini-2.5-flash", State: model.GeminiCachedContentActive, ExpiresAt: time.Now().Add(time.Hour).Unix()}
			require.NoError(t, model.DB.Create(&record).Error)
			for _, body := range []string{
				`{"CachedContent":"cachedContents/` + publicID + `"}`,
				`{"cachedContent":"cachedContents/external","CachedContent":"cachedContents/` + publicID + `"}`,
				`{"cachedContent":"cachedContents/` + publicID + `","CachedContent":"cachedContents/` + publicID + `"}`,
				`{"GenerateContentRequest":{"CachedContent":"cachedContents/` + publicID + `"}}`,
				`{"cachedContent":"cachedContents/external","generateContentRequeſt":{"cachedContent":"cachedContents/` + publicID + `"}}`,
			} {
				response := dispatchBody(body)
				assert.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
			}
			for _, state := range []string{model.GeminiCachedContentActive, model.GeminiCachedContentPending, model.GeminiCachedContentMutating, model.GeminiCachedContentDeleted} {
				require.NoError(t, model.DB.Model(&record).Update("state", state).Error)
				response := dispatch("cachedContents/" + publicID)
				assert.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
			}
			require.NoError(t, model.DB.Model(&record).Updates(map[string]any{"state": model.GeminiCachedContentActive, "user_id": 1, "token_id": 1, "expires_at": time.Now().Add(-time.Second).Unix()}).Error)
			response := dispatch("cachedContents/" + publicID)
			assert.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
			const callback = "gemini_cache_reference_lookup_failure"
			require.NoError(t, model.DB.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
				if tx.Statement.Table == "gemini_cached_contents" {
					tx.AddError(errors.New("cache ownership database unavailable"))
				}
			}))
			response = dispatch("cachedContents/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
			assert.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
			require.NoError(t, model.DB.Callback().Query().Remove(callback))
		})
	}
}

func TestGeminiCacheOpenAIReferencesBindOwnerAndCredential(t *testing.T) {
	require.NoError(t, i18n.Init())
	setupGeminiCacheRoutingDB(t)
	channel := model.Channel{Type: constant.ChannelTypeGemini, Name: "managed Gemini", Status: common.ChannelStatusEnabled, Key: "other-key\noriginal-key", Models: "gemini-alias", Group: "default"}
	channel.ModelMapping = common.GetPointer(`{"gemini-alias":"gemini-2.5-flash"}`)
	channel.ChannelInfo.IsMultiKey = true
	require.NoError(t, channel.Insert())
	record := model.GeminiCachedContent{
		PublicID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", UserID: 1, TokenID: 2,
		ChannelID: channel.Id, Group: "default", Model: "gemini-alias", UpstreamModel: "gemini-2.5-flash",
		KeyFingerprint: service.GeminiCacheKeyFingerprint("original-key"), BaseURL: "https://generativelanguage.googleapis.com",
		State: model.GeminiCachedContentActive, ExpiresAt: time.Now().Add(time.Hour).Unix(),
	}
	require.NoError(t, model.DB.Create(&record).Error)
	const body = `{"model":"gemini-alias","messages":[{"role":"user","content":"continue"}],"extra_body":{"google":{"cached_content":"cachedContents/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}}`
	for _, tc := range []struct {
		name   string
		body   string
		change func(*gin.Context, *model.GeminiCachedContent)
		status int
	}{
		{name: "owner gets the original credential", status: http.StatusNoContent},
		{name: "other user", change: func(c *gin.Context, _ *model.GeminiCachedContent) { c.Set("id", 99) }, status: http.StatusForbidden},
		{name: "other token", change: func(c *gin.Context, _ *model.GeminiCachedContent) { c.Set("token_id", 99) }, status: http.StatusForbidden},
		{name: "different model", body: strings.Replace(body, "gemini-alias", "other-model", 1), status: http.StatusForbidden},
		{name: "different group", change: func(c *gin.Context, _ *model.GeminiCachedContent) {
			common.SetContextKey(c, constant.ContextKeyUsingGroup, "other-group")
		}, status: http.StatusForbidden},
		{name: "token model restriction", change: func(c *gin.Context, _ *model.GeminiCachedContent) {
			common.SetContextKey(c, constant.ContextKeyTokenModelLimitEnabled, true)
			common.SetContextKey(c, constant.ContextKeyTokenModelLimit, map[string]bool{"other-model": true})
		}, status: http.StatusForbidden},
		{name: "conflicting pin", change: func(c *gin.Context, _ *model.GeminiCachedContent) {
			service.GetChannelConstraints(c).AddPin(hostdto.ChannelPin{ChannelId: channel.Id + 1})
		}, status: http.StatusForbidden},
		{name: "pending", change: func(_ *gin.Context, r *model.GeminiCachedContent) { r.State = model.GeminiCachedContentPending }, status: http.StatusForbidden},
		{name: "mutating", change: func(_ *gin.Context, r *model.GeminiCachedContent) { r.State = model.GeminiCachedContentMutating }, status: http.StatusForbidden},
		{name: "deleted", change: func(_ *gin.Context, r *model.GeminiCachedContent) { r.State = model.GeminiCachedContentDeleted }, status: http.StatusForbidden},
		{name: "expired", change: func(_ *gin.Context, r *model.GeminiCachedContent) { r.ExpiresAt = time.Now().Add(-time.Hour).Unix() }, status: http.StatusForbidden},
		{name: "unknown public ID", body: strings.Replace(body, record.PublicID, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", 1), status: http.StatusForbidden},
		{name: "external Vertex name", body: strings.Replace(body, "cachedContents/"+record.PublicID, "projects/p/locations/us-central1/cachedContents/external", 1), status: http.StatusForbidden},
		{name: "duplicate container cannot hide local ID", body: strings.TrimSuffix(body, "}") + `,"extra_body":{}}`, status: http.StatusBadRequest},
		{name: "system message", body: strings.Replace(body, `"role":"user"`, `"role":"system"`, 1), status: http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			requestBody := tc.body
			if requestBody == "" {
				requestBody = body
			}
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(requestBody))
			ctx.Set("id", 1)
			ctx.Set("token_id", 2)
			common.SetContextKey(ctx, constant.ContextKeyUsingGroup, "default")
			common.SetContextKey(ctx, constant.ContextKeyUserGroup, "default")
			candidate := record
			if tc.change != nil {
				tc.change(ctx, &candidate)
			}
			require.NoError(t, model.DB.Save(&candidate).Error)
			t.Cleanup(func() { common.CleanupBodyStorage(ctx) })
			if prepareGeminiCacheReference(ctx) {
				require.Nil(t, SetupContextForSelectedChannel(ctx, &channel, "gemini-alias"))
				assert.Equal(t, channel.Id, common.GetContextKeyInt(ctx, constant.ContextKeyChannelId))
				assert.Equal(t, "original-key", common.GetContextKeyString(ctx, constant.ContextKeyChannelKey))
				assert.Equal(t, 1, common.GetContextKeyInt(ctx, constant.ContextKeyChannelMultiKeyIndex))
				pins := service.GetChannelConstraints(ctx).Pins
				require.Len(t, pins, 1)
				assert.Equal(t, hostdto.PinRetrySingleAttempt, pins[0].RetryMode)
				assert.Equal(t, channel.Id, pins[0].ChannelId)
				ctx.Status(http.StatusNoContent)
				ctx.Writer.WriteHeaderNow()
			}
			assert.Equal(t, tc.status, recorder.Code, recorder.Body.String())
		})
	}
	// Enforce the managed-cache limit before materializing any disk body.
	file, err := os.Create(filepath.Join(t.TempDir(), "oversized-cache-request.json"))
	require.NoError(t, err)
	_, err = file.WriteString(body)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, file.Close()) })
	storage := &geminiCacheDiskStorageProbe{File: file, size: service.GeminiCacheMaxRequestBytes + 1}
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ctx.Set(common.KeyBodyStorage, storage)
	require.False(t, prepareGeminiCacheReference(ctx))
	assert.Equal(t, http.StatusRequestEntityTooLarge, recorder.Code)
	assert.Zero(t, storage.wholeReads)
}
