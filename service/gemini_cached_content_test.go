package service

import (
	"context"
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
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGeminiCacheErrorLocalization(t *testing.T) {
	for _, tc := range []struct {
		language, message, want string
	}{
		{"en", "CachedContent not found (or permission denied)", "CachedContent not found (or permission denied)"},
		{"zh-CN", "CachedContent not found (or permission denied)", "缓存不存在或无权访问"},
		{"zh-TW", "CachedContent not found (or permission denied)", "快取不存在或無權存取"},
		{"zh-CN", "explicit cache pricing is not configured for gemini-2.5-flash-cache-create", "未配置 gemini-2.5-flash-cache-create 的显式缓存价格"},
		{"zh-CN", "invalid explicit cache pricing: unknown variable {{.Model}}", "显式缓存价格配置无效：unknown variable {{.Model}}"},
		{"zh-CN", "cached content token count must be between 1 and 2000000", "缓存 token 数量必须在 1 到 2000000 之间"},
		{"zh-TW", "explicit cache price evaluation failed: division by zero", "顯式快取價格計算失敗：division by zero"},
		{"zh-TW", "Cached content upstream request rejected (PERMISSION_DENIED)", "快取上游請求遭拒（PERMISSION_DENIED）"},
		{"en", "Cached content upstream request rejected (RESOURCE_EXHAUSTED)", "Cached content upstream request rejected (RESOURCE_EXHAUSTED)"},
		{"zh-CN", "Unrecognized upstream error: diagnostic", "Unrecognized upstream error: diagnostic"},
		{"zh-CN", "Cached content upstream request rejected (malformed", "Cached content upstream request rejected (malformed"},
	} {
		t.Run(tc.language+"/"+tc.message, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1beta/cachedContents", nil)
			c.Request.Header.Set("Accept-Language", tc.language)
			assert.Equal(t, tc.want, LocalizeGeminiCacheError(c, tc.message))
		})
	}
}

func TestGeminiCacheRequestValidation(t *testing.T) {
	now := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	valid := `{"model":"models/gemini-2.5-flash","contents":[{"role":"user","parts":[{"text":"hello"},{"inlineData":{"mimeType":"image/png","data":"aGVsbG8="}}]}],"tools":[{"functionDeclarations":[{"name":"lookup","parameters":{"type":"OBJECT","properties":{"snake_case":{"type":"STRING"}}}}]}]}`
	request, err := ParseGeminiCacheCreate([]byte(valid), now)
	require.NoError(t, err)
	assert.Equal(t, "gemini-2.5-flash", request.Model)
	assert.Equal(t, now.Add(time.Hour), request.ExpiresAt)
	assert.Contains(t, string(request.Body["contents"]), `"inlineData"`)
	assert.Contains(t, string(request.Body["tools"]), `"snake_case"`)
	assert.NotContains(t, request.Body, "ttl")
	assert.Equal(t, `"2026-10-14T01:00:00Z"`, string(request.Body["expireTime"]))

	for _, tc := range []struct{ name, body string }{
		{"duplicate model", `{"model":"gemini-2.5-flash","model":"other","contents":[]}`},
		{"escaped duplicate", `{"model":"gemini-2.5-flash","mo\u0064el":"other"}`},
		{"nested duplicate", `{"model":"gemini-2.5-flash","contents":[{"parts":[{"text":"one","text":"two"}]}]}`},
		{"server managed", `{"model":"gemini-2.5-flash","name":"cachedContents/a","contents":[{"parts":[{"text":"hi"}]}]}`},
		{"expiration alias", `{"model":"gemini-2.5-flash","expire_time":"2026-10-14T01:00:00Z","contents":[{"parts":[{"text":"hi"}]}]}`},
		{"exclusive expiration", `{"model":"gemini-2.5-flash","ttl":"1s","expireTime":"2026-10-14T01:00:00Z","contents":[{"parts":[{"text":"hi"}]}]}`},
		{"oversized ttl", `{"model":"gemini-2.5-flash","ttl":"86401s","contents":[{"parts":[{"text":"hi"}]}]}`},
		{"negative ttl", `{"model":"gemini-2.5-flash","ttl":"-1s","contents":[{"parts":[{"text":"hi"}]}]}`},
		{"overflow ttl", `{"model":"gemini-2.5-flash","ttl":"99999999999999999s","contents":[{"parts":[{"text":"hi"}]}]}`},
		{"null ttl", `{"model":"gemini-2.5-flash","ttl":null,"contents":[{"parts":[{"text":"hi"}]}]}`},
		{"path model", `{"model":"../gemini","contents":[{"parts":[{"text":"hi"}]}]}`},
		{"null contents", `{"model":"gemini-2.5-flash","contents":null}`},
		{"empty", `{}`},
		{"multiple objects", `{} {}`},
		{"protocol depth limit", `{"contents":` + strings.Repeat("[", 65) + "0" + strings.Repeat("]", 65) + "}"},
		{"decoder depth limit", `{"contents":` + strings.Repeat("[", 10001) + "0" + strings.Repeat("]", 10001) + "}"},
	} {
		t.Run(tc.name, func(t *testing.T) { _, err := ParseGeminiCacheCreate([]byte(tc.body), now); require.Error(t, err) })
	}
	patch, err := ParseGeminiCachePatch([]byte(`{"ttl":"3600.5s"}`), now)
	require.NoError(t, err)
	assert.Equal(t, now.Add(time.Hour+time.Second), patch.ExpiresAt)
	assert.Equal(t, "expireTime", patch.UpdateMask)
	for _, body := range []string{`{}`, `{"ttl":"0s"}`, `{"ttl":"1h"}`, `{"ttl":"1s","model":"gemini"}`, `{"expireTime":"2020-01-01T00:00:00Z"}`} {
		_, err := ParseGeminiCachePatch([]byte(body), now)
		require.Error(t, err)
	}
}

func TestGeminiCacheReferenceValidation(t *testing.T) {
	for _, tc := range []struct {
		name, body, reference string
		fails                 bool
	}{
		{"incremental content", `{"cachedContent":"cachedContents/aaaaaaaaaaaaaaaa","contents":[{"parts":[{"text":"next"}]}]}`, "cachedContents/aaaaaaaaaaaaaaaa", false},
		{"count tokens", `{"generateContentRequest":{"cachedContent":"cachedContents/aaaaaaaaaaaaaaaa","contents":[{"parts":[{"text":"next"}]}]}}`, "cachedContents/aaaaaaaaaaaaaaaa", false},
		{"alias", `{"cached_content":"cachedContents/aaaaaaaaaaaaaaaa"}`, "", true},
		{"nested alias", `{"generate_content_request":{"cachedContent":"cachedContents/aaaaaaaaaaaaaaaa"}}`, "", true},
		{"duplicate reference", `{"cachedContent":"cachedContents/aaaaaaaaaaaaaaaa","cachedContent":"cachedContents/bbbbbbbbbbbbbbbb"}`, "", true},
		{"conflicting references", `{"cachedContent":"cachedContents/aaaaaaaaaaaaaaaa","generateContentRequest":{"cachedContent":"cachedContents/bbbbbbbbbbbbbbbb"}}`, "", true},
		{"duplicate immutable system", `{"cachedContent":"cachedContents/aaaaaaaaaaaaaaaa","systemInstruction":{"parts":[{"text":"override"}]}}`, "", true},
		{"duplicate immutable tools", `{"cachedContent":"cachedContents/aaaaaaaaaaaaaaaa","tools":[]}`, "", true},
		{"ordinary request", `{"contents":[{"parts":[{"text":"hello"}]}]}`, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reference, err := ValidateGeminiCacheReference([]byte(tc.body))
			if tc.fails {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.reference, reference)
		})
	}
}

func TestGeminiCacheBindingPinsAccountAndConfiguration(t *testing.T) {
	channel := &model.Channel{Id: 7, Type: constant.ChannelTypeGemini, Status: common.ChannelStatusEnabled, Models: "gemini-alias", Group: "default", Key: "second\noriginal", ModelMapping: common.GetPointer(`{"gemini-alias":"gemini-2.5-flash"}`), ChannelInfo: model.ChannelInfo{IsMultiKey: true}}
	record := &model.GeminiCachedContent{ChannelID: 7, Group: "default", Model: "gemini-alias", UpstreamModel: "gemini-2.5-flash", BaseURL: "https://generativelanguage.googleapis.com", KeyFingerprint: GeminiCacheKeyFingerprint("original")}
	key, index, err := GeminiCacheBinding(channel, record)
	require.NoError(t, err)
	assert.Equal(t, "original", key)
	assert.Equal(t, 1, index)
	for _, tc := range []struct {
		name   string
		change func(*model.Channel)
	}{
		{"disabled key", func(c *model.Channel) { c.ChannelInfo.MultiKeyStatusList = map[int]int{1: 2} }},
		{"rotated key", func(c *model.Channel) { c.Key = "second\nreplacement" }},
		{"disabled channel", func(c *model.Channel) { c.Status = 2 }},
		{"mapping drift", func(c *model.Channel) { c.ModelMapping = common.GetPointer(`{"gemini-alias":"gemini-2.5-pro"}`) }},
		{"mapping cycle", func(c *model.Channel) {
			c.ModelMapping = common.GetPointer(`{"gemini-alias":"other","other":"gemini-alias"}`)
		}},
		{"model removed", func(c *model.Channel) { c.Models = "other" }},
		{"group removed", func(c *model.Channel) { c.Group = "other" }},
		{"base drift", func(c *model.Channel) { c.BaseURL = common.GetPointer("https://other.example") }},
		{"header override", func(c *model.Channel) { c.HeaderOverride = common.GetPointer(`{"x-goog-api-key":"other"}`) }},
		{"parameter override", func(c *model.Channel) { c.ParamOverride = common.GetPointer(`{"model":"other"}`) }},
		{"system prompt", func(c *model.Channel) { c.Setting = common.GetPointer(`{"system_prompt":"override"}`) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := *channel
			tc.change(&changed)
			_, _, err := GeminiCacheBinding(&changed, record)
			require.Error(t, err)
		})
	}
	thirdParty := *channel
	thirdParty.BaseURL = common.GetPointer("https://proxy.example")
	_, err = GeminiCacheChannelBaseURL(&thirdParty)
	require.Error(t, err)
	thirdParty.Setting = common.GetPointer(`{"gemini_explicit_cache_enabled":true}`)
	base, err := GeminiCacheChannelBaseURL(&thirdParty)
	require.NoError(t, err)
	assert.Equal(t, "https://proxy.example", base)
	for _, address := range []string{"https://proxy.example?key=secret", "https://user:secret@proxy.example", "https://proxy.example/#fragment", "file:///tmp/cache"} {
		thirdParty.BaseURL = &address
		_, err := GeminiCacheChannelBaseURL(&thirdParty)
		require.Error(t, err)
	}
}

func TestGeminiCacheUpstreamProtocolAndCleanup(t *testing.T) {
	expiry := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	var calls []string
	var invalidCreation bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.RequestURI())
		assert.Equal(t, "account-secret", r.Header.Get("x-goog-api-key"))
		assert.Empty(t, r.URL.Query().Get("key"))
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodDelete {
			_, _ = w.Write([]byte(`{}`))
			return
		}
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		if strings.HasSuffix(r.URL.Path, ":countTokens") {
			var request map[string]map[string]common.RawMessage
			require.NoError(t, common.Unmarshal(body, &request))
			assert.Equal(t, `"models/gemini-2.5-flash"`, string(request["generateContentRequest"]["model"]))
			assert.NotContains(t, request["generateContentRequest"], "expireTime")
			_, _ = w.Write([]byte(`{"totalTokens":2048}`))
			return
		}
		if r.Method == http.MethodPost {
			assert.Contains(t, string(body), `"model":"models/gemini-2.5-flash"`)
		}
		if r.Method == http.MethodPatch {
			assert.Equal(t, "expireTime", r.URL.Query().Get("updateMask"))
		}
		tokens := 2048
		if invalidCreation {
			tokens = -1
		}
		response, err := common.Marshal(map[string]any{"name": "cachedContents/provider-resource", "model": "models/gemini-2.5-flash", "expireTime": expiry.Format(time.RFC3339), "usageMetadata": map[string]any{"totalTokenCount": tokens}})
		require.NoError(t, err)
		_, _ = w.Write(response)
	}))
	defer server.Close()
	channel := &model.Channel{Type: constant.ChannelTypeGemini, Status: common.ChannelStatusEnabled, BaseURL: &server.URL, Setting: common.GetPointer(`{"gemini_explicit_cache_enabled":true}`)}
	request, err := ParseGeminiCacheCreate([]byte(`{"model":"local-alias","contents":[{"parts":[{"text":"hello"}]}]}`), expiry.Add(-time.Hour))
	require.NoError(t, err)
	count, err := CountGeminiCacheTokens(context.Background(), channel, "account-secret", request, "gemini-2.5-flash")
	require.NoError(t, err)
	assert.Equal(t, 2048, count)
	resource, err := CreateGeminiCachedContent(context.Background(), channel, "account-secret", request, "gemini-2.5-flash")
	require.NoError(t, err)
	assert.Equal(t, "cachedContents/provider-resource", resource.Name)
	patch, err := ParseGeminiCachePatch([]byte(`{"expireTime":"`+expiry.Format(time.RFC3339)+`"}`), expiry.Add(-time.Hour))
	require.NoError(t, err)
	_, err = PatchGeminiCachedContent(context.Background(), channel, "account-secret", resource.Name, patch, "gemini-2.5-flash")
	require.NoError(t, err)
	_, err = GetGeminiCachedContentUpstream(context.Background(), channel, "account-secret", resource.Name, "gemini-2.5-flash")
	require.NoError(t, err)
	require.NoError(t, DeleteGeminiCachedContent(context.Background(), channel, "account-secret", resource.Name))
	invalidCreation = true
	_, err = CreateGeminiCachedContent(context.Background(), channel, "account-secret", request, "gemini-2.5-flash")
	require.Error(t, err)
	assert.Equal(t, "DELETE /v1beta/cachedContents/provider-resource", calls[len(calls)-1])
	assert.Len(t, calls, 7)
}

func TestGeminiCacheUpstreamRejectsRedirectAndOversizedResponse(t *testing.T) {
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"redirect", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/must-not-follow", http.StatusTemporaryRedirect)
		}},
		{"oversized", func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(strings.Repeat(" ", geminiCacheMaxResponseBytes+1)))
		}},
		{"credential in provider error", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"message":"bad account-secret on https://example/?key=account-secret"}}`))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; tc.handler(w, r) }))
			defer server.Close()
			channel := &model.Channel{Type: constant.ChannelTypeGemini, Status: common.ChannelStatusEnabled, BaseURL: &server.URL, Setting: common.GetPointer(`{"gemini_explicit_cache_enabled":true}`)}
			_, err := GetGeminiCachedContentUpstream(context.Background(), channel, "account-secret", "cachedContents/example", "gemini-2.5-flash")
			require.Error(t, err)
			assert.NotContains(t, err.Error(), "account-secret")
			assert.Equal(t, 1, calls)
		})
	}
}

func TestGeminiCacheClientResponseHidesAccountResource(t *testing.T) {
	record := &model.GeminiCachedContent{PublicID: "aaaaaaaaaaaaaaaa", Model: "local-alias", UpstreamName: "cachedContents/private-upstream", UpstreamModel: "provider-model", KeyFingerprint: "secret-fingerprint", ChannelID: 22, TotalTokens: 2048}
	body, err := common.Marshal(GeminiCacheClientResponse(record))
	require.NoError(t, err)
	assert.Contains(t, string(body), `"name":"cachedContents/aaaaaaaaaaaaaaaa"`)
	assert.Contains(t, string(body), `"model":"models/local-alias"`)
	assert.NotContains(t, string(body), "private-upstream")
	assert.NotContains(t, string(body), "provider-model")
	assert.NotContains(t, string(body), "secret-fingerprint")
}

func TestGeminiCacheReferenceDetectionDoesNotMissDuplicateOrEscapedKeys(t *testing.T) {
	for _, body := range []string{
		`{"cachedContent":"","cachedContent":"cachedContents/aaaaaaaaaaaaaaaa"}`,
		`{"cached\u0043ontent":"cachedContents/aaaaaaaaaaaaaaaa"}`,
		`{"generate_content_request":{"cachedContent":"cachedContents/aaaaaaaaaaaaaaaa"}}`,
		`{"generateContentRequest":{"cached_content":"cachedContents/aaaaaaaaaaaaaaaa"}}`,
		`{"generateContentRequest":{},"generateContentRequest":{"cachedContent":"cachedContents/aaaaaaaaaaaaaaaa"}}`,
	} {
		assert.True(t, HasGeminiCacheReference([]byte(body)), body)
	}
	assert.False(t, HasGeminiCacheReference([]byte(`{"generateContentRequest":{"contents":[]}}`)))
	assert.False(t, HasGeminiCacheReference([]byte(`{"tools":[{"parameters":{"properties":{"cachedContent":{"type":"STRING"}}}}]}`)))
}

func TestGeminiCacheExpirationWholeSecondBillingBoundary(t *testing.T) {
	now := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		body    string
		offset  time.Duration
		seconds int64
		fails   bool
	}{
		{`{"ttl":"1.000000001s"}`, 0, 2, false},
		{`{"ttl":"86399.999999999s"}`, 0, 86400, false},
		{`{"ttl":"86400s"}`, 0, 86400, false},
		{`{"ttl":"86400s"}`, time.Nanosecond, 0, true},
		{`{"ttl":"86400.000000001s"}`, 0, 0, true},
		{`{"ttl":"-0.5s"}`, 0, 0, true},
		{`{"ttl":"0.999s"}`, 0, 0, true},
		{`{"expireTime":"2026-10-14T00:00:01.001Z"}`, 0, 2, false},
	} {
		patch, err := ParseGeminiCachePatch([]byte(tc.body), now.Add(tc.offset))
		if tc.fails {
			require.Error(t, err, tc.body)
			continue
		}
		require.NoError(t, err, tc.body)
		assert.Equal(t, now.Add(time.Duration(tc.seconds)*time.Second), patch.ExpiresAt)
		assert.Zero(t, patch.ExpiresAt.Nanosecond())
	}
}

type geminiCacheBoundedReadProbe struct {
	io.Reader
	largestRead int
	bytesRead   int
}

func (r *geminiCacheBoundedReadProbe) Read(p []byte) (int, error) {
	r.largestRead = max(r.largestRead, len(p))
	n, err := r.Reader.Read(p)
	r.bytesRead += n
	return n, err
}

func TestGeminiCacheReferenceReader(t *testing.T) {
	for _, tc := range []struct {
		body  string
		found bool
	}{
		{`{"cachedContent":"cachedContents/a"}`, true},
		{`{"cached\u0043ontent":"cachedContents/a"}`, true},
		{`{"cachedContent":"","cachedContent":"cachedContents/a"}`, true},
		{`{"generateContentRequest":{},"generateContentRequest":{"cachedContent":"cachedContents/a"}}`, true},
		{`{"generate_content_request":{"cached_content":"cachedContents/a"}}`, true},
		{`{"generate\u0043ontentRequest":{"cached\u0043ontent":"cachedContents/a"}}`, true},
		{`{"contents":[{"parts":[{"text":"\"cachedContent\":\"not-a-reference\""}]}]}`, false},
		{`{"tools":[{"parameters":{"properties":{"cachedContent":{"type":"STRING"}}}}]}`, false},
		{`{"generateContentRequest":{"contents":[{"parts":[{"text":"ordinary"}]}]}}`, false},
	} {
		found, err := GeminiCacheReferenceInReader(strings.NewReader(tc.body))
		require.NoError(t, err, tc.body)
		assert.Equal(t, tc.found, found, tc.body)
	}
	file, err := os.Create(filepath.Join(t.TempDir(), "large-request.json"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, file.Close()) })
	_, err = file.WriteString(`{"contents":[{"parts":[{"text":"`)
	require.NoError(t, err)
	// A normal disk-backed generation payload larger than the explicit-cache
	// limit must remain streamable; its inline text is not cache metadata.
	chunk := strings.Repeat("a", 1<<20)
	for range 33 {
		_, err = file.WriteString(chunk)
		require.NoError(t, err)
	}
	_, err = file.WriteString(`"}]}]}`)
	require.NoError(t, err)
	_, err = file.Seek(0, io.SeekStart)
	require.NoError(t, err)
	probe := &geminiCacheBoundedReadProbe{Reader: file}
	found, err := GeminiCacheReferenceInReader(probe)
	require.NoError(t, err)
	assert.False(t, found)
	assert.LessOrEqual(t, probe.largestRead, 64<<10, "the probe must not request buffers proportional to the body")
	// A cache key near the beginning must be detected without consuming a large
	// remaining request before its stricter cache size check runs.
	_, err = file.Seek(0, io.SeekStart)
	require.NoError(t, err)
	early := &geminiCacheBoundedReadProbe{Reader: io.MultiReader(strings.NewReader(`{"cachedContent":"cachedContents/a","contents":`), file)}
	found, err = GeminiCacheReferenceInReader(early)
	require.NoError(t, err)
	assert.True(t, found)
	assert.LessOrEqual(t, early.bytesRead, 64<<10, "an early reference must not scan or buffer the remaining large body")
}

func TestGeminiCacheReferenceReaderRejectsAmbiguousAccountRouting(t *testing.T) {
	for _, body := range []string{
		`{"cachedContent":"cachedContents/external","cachedContent":"cachedContents/aaaaaaaaaaaaaaaa"}`,
		`{"cachedContent":"cachedContents/external","generate_content_request":{"cached_content":"cachedContents/aaaaaaaaaaaaaaaa"}}`,
		`{"cachedContent":null}`,
	} {
		_, err := GeminiCacheReferenceNameInReader(strings.NewReader(body))
		require.Error(t, err, body)
	}
	name, err := GeminiCacheReferenceNameInReader(strings.NewReader(`{"generateContentRequest":{},"generateContentRequest":{"cached\u0043ontent":"projects/p/locations/l/cachedContents/id"}}`))
	require.NoError(t, err)
	assert.Equal(t, "projects/p/locations/l/cachedContents/id", name)
}
