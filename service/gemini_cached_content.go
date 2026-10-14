package service

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	hostreasoning "github.com/QuantumNous/new-api/setting/reasoning"
	"github.com/tidwall/gjson"
)

const (
	GeminiCacheMaxTokens        = 2_000_000
	GeminiCacheMaxTTL           = 24 * time.Hour
	geminiCacheMaxResponseBytes = 4 << 20
	GeminiCacheMaxRequestBytes  = 32 << 20
)

var (
	geminiCacheModelPattern        = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,199}$`)
	geminiCachePublicIDPattern     = regexp.MustCompile(`^[a-z0-9]{16,64}$`)
	geminiCacheUpstreamNamePattern = regexp.MustCompile(`^cachedContents/[A-Za-z0-9_-]{1,256}$`)
	geminiCacheDurationPattern     = regexp.MustCompile(`^[0-9]+(?:\.[0-9]{1,9})?s$`)
)

// GeminiCacheError contains a safe Google-style error. Upstream bodies and URLs
// are deliberately excluded because they can contain account credentials.
type GeminiCacheError struct {
	HTTPStatus int
	Status     string
	Message    string
	// Internal recovery metadata, never included in client errors.
	OutcomeUncertain bool
	UpstreamName     string
}

func (e *GeminiCacheError) Error() string { return e.Message }

func geminiCacheInvalid(message string) error {
	return &GeminiCacheError{HTTPStatus: http.StatusBadRequest, Status: "INVALID_ARGUMENT", Message: message}
}

func geminiCacheUnavailable() error {
	return &GeminiCacheError{HTTPStatus: http.StatusConflict, Status: "FAILED_PRECONDITION", Message: "Cached content channel configuration is unavailable or has changed"}
}

func geminiCacheBadUpstream() error {
	return &GeminiCacheError{HTTPStatus: http.StatusBadGateway, Status: "INTERNAL", Message: "Invalid cached content response from upstream"}
}

// GeminiCacheCreateRequest keeps content as raw JSON so multimodal parts and
// function schemas survive validation without passing through a lossy DTO.
type GeminiCacheCreateRequest struct {
	Body        map[string]common.RawMessage
	Model       string
	DisplayName string
	ExpiresAt   time.Time
}

type GeminiCachePatchRequest struct {
	Body       map[string]common.RawMessage
	ExpiresAt  time.Time
	UpdateMask string
}

type GeminiCacheUpstreamResource struct {
	Name        string
	Model       string
	DisplayName string
	TotalTokens int
	ExpireTime  time.Time
}

// validateGeminiCacheJSON rejects duplicate keys before any decoder can choose
// a different value. Schema/property names are data and are not renamed.
func validateGeminiCacheJSON(value gjson.Result, depth int) error {
	if depth > 64 {
		return geminiCacheInvalid("JSON nesting exceeds the supported limit")
	}
	if !value.IsObject() && !value.IsArray() {
		return nil
	}
	seen := make(map[string]struct{})
	var validationErr error
	value.ForEach(func(key, child gjson.Result) bool {
		if value.IsObject() {
			if _, exists := seen[key.String()]; exists {
				validationErr = geminiCacheInvalid("Duplicate JSON fields are not supported")
				return false
			}
			seen[key.String()] = struct{}{}
		}
		validationErr = validateGeminiCacheJSON(child, depth+1)
		return validationErr == nil
	})
	return validationErr
}

func decodeGeminiCacheObject(body []byte) (map[string]common.RawMessage, error) {
	if len(body) == 0 || len(body) > GeminiCacheMaxRequestBytes {
		return nil, geminiCacheInvalid("Expected a valid JSON object within the request size limit")
	}
	// The shared decoder bounds syntax nesting before gjson traversal. Calling
	// gjson.ValidBytes first would recurse without the protocol's depth limit.
	var fields map[string]common.RawMessage
	if err := common.Unmarshal(body, &fields); err != nil {
		return nil, geminiCacheInvalid("Invalid JSON object")
	}
	root := gjson.ParseBytes(body)
	if !root.IsObject() {
		return nil, geminiCacheInvalid("Expected a JSON object")
	}
	if err := validateGeminiCacheJSON(root, 0); err != nil {
		return nil, err
	}
	return fields, nil
}

func geminiCacheExpiration(fields map[string]common.RawMessage, now time.Time, required bool) (time.Time, error) {
	ttl, hasTTL := fields["ttl"]
	expires, hasExpiry := fields["expireTime"]
	if hasTTL && hasExpiry {
		return time.Time{}, geminiCacheInvalid("Specify exactly one of ttl or expireTime")
	}
	expiry := now.Add(time.Hour)
	var text string
	switch {
	case !hasTTL && !hasExpiry:
		if required {
			return time.Time{}, geminiCacheInvalid("Specify ttl or expireTime")
		}
	case hasTTL:
		if common.Unmarshal(ttl, &text) != nil || !geminiCacheDurationPattern.MatchString(text) {
			return time.Time{}, geminiCacheInvalid("ttl must be a positive duration in seconds ending in s")
		}
		duration, err := time.ParseDuration(text)
		if err != nil || duration < time.Second || duration > GeminiCacheMaxTTL {
			return time.Time{}, geminiCacheInvalid("ttl must be between 1 and 86400 seconds")
		}
		expiry = now.Add(duration)
	case hasExpiry:
		if common.Unmarshal(expires, &text) != nil {
			return time.Time{}, geminiCacheInvalid("expireTime must be an RFC3339 timestamp")
		}
		var err error
		expiry, err = time.Parse(time.RFC3339Nano, text)
		if err != nil || expiry.Sub(now) < time.Second || expiry.Sub(now) > GeminiCacheMaxTTL {
			return time.Time{}, geminiCacheInvalid("expireTime must be between 1 and 86400 seconds in the future")
		}
	}
	// Persisted deadlines and storage billing both use whole seconds. Round once
	// here, before quoting or contacting upstream, so a fractional duration cannot
	// escape charging or lead to an unusable resource after metadata truncation.
	if expiry.Nanosecond() != 0 {
		expiry = expiry.Truncate(time.Second).Add(time.Second)
	}
	if expiry.After(now.Truncate(time.Second).Add(GeminiCacheMaxTTL)) {
		return time.Time{}, geminiCacheInvalid("Rounded expiration must not exceed 86400 seconds")
	}
	return expiry, nil
}

func ParseGeminiCacheCreate(body []byte, now time.Time) (*GeminiCacheCreateRequest, error) {
	fields, err := decodeGeminiCacheObject(body)
	if err != nil {
		return nil, err
	}
	for key := range fields {
		if !slices.Contains([]string{"model", "contents", "systemInstruction", "tools", "toolConfig", "displayName", "ttl", "expireTime"}, key) {
			return nil, geminiCacheInvalid("Unsupported or server-managed cached content field")
		}
	}
	var localModel string
	if common.Unmarshal(fields["model"], &localModel) != nil {
		return nil, geminiCacheInvalid("model is required")
	}
	localModel = strings.TrimPrefix(localModel, "models/")
	if !geminiCacheModelPattern.MatchString(localModel) {
		return nil, geminiCacheInvalid("Invalid model name")
	}
	if contents, present := fields["contents"]; present {
		value := gjson.ParseBytes(contents)
		if !value.IsArray() || len(value.Array()) == 0 {
			return nil, geminiCacheInvalid("contents must be a non-empty array")
		}
		for _, item := range value.Array() {
			if !item.IsObject() || !item.Get("parts").IsArray() || len(item.Get("parts").Array()) == 0 {
				return nil, geminiCacheInvalid("Each content must contain a non-empty parts array")
			}
		}
	}
	for _, name := range []string{"systemInstruction", "toolConfig"} {
		if raw, present := fields[name]; present && !gjson.ParseBytes(raw).IsObject() {
			return nil, geminiCacheInvalid(name + " must be an object")
		}
	}
	if raw, present := fields["tools"]; present && !gjson.ParseBytes(raw).IsArray() {
		return nil, geminiCacheInvalid("tools must be an array")
	}
	if _, hasContents := fields["contents"]; !hasContents {
		if _, hasSystem := fields["systemInstruction"]; !hasSystem {
			return nil, geminiCacheInvalid("contents or systemInstruction is required")
		}
	}
	var displayName string
	if raw, present := fields["displayName"]; present {
		if common.Unmarshal(raw, &displayName) != nil || string(raw) == "null" || utf8.RuneCountInString(displayName) > 128 {
			return nil, geminiCacheInvalid("displayName must be a string of at most 128 characters")
		}
	}
	expiresAt, err := geminiCacheExpiration(fields, now, false)
	if err != nil {
		return nil, err
	}
	// Send an absolute deadline: countTokens latency cannot silently extend the
	// paid storage period, and retries do not restart a relative TTL.
	delete(fields, "ttl")
	fields["expireTime"], _ = common.Marshal(expiresAt.UTC().Format(time.RFC3339Nano))
	return &GeminiCacheCreateRequest{Body: fields, Model: localModel, DisplayName: displayName, ExpiresAt: expiresAt}, nil
}

func ParseGeminiCachePatch(body []byte, now time.Time) (*GeminiCachePatchRequest, error) {
	fields, err := decodeGeminiCacheObject(body)
	if err != nil {
		return nil, err
	}
	for key := range fields {
		if key != "ttl" && key != "expireTime" {
			return nil, geminiCacheInvalid("Only ttl or expireTime may be updated")
		}
	}
	expiry, err := geminiCacheExpiration(fields, now, true)
	if err != nil {
		return nil, err
	}
	encoded, _ := common.Marshal(expiry.UTC().Format(time.RFC3339Nano))
	return &GeminiCachePatchRequest{Body: map[string]common.RawMessage{"expireTime": encoded}, ExpiresAt: expiry, UpdateMask: "expireTime"}, nil
}

// ValidateGeminiCacheReference validates both generateContent and countTokens.
// New conversation contents are allowed with cachedContent; cached immutable
// system instructions and tool definitions cannot be supplied a second time.
func ValidateGeminiCacheReference(body []byte) (string, error) {
	fields, err := decodeGeminiCacheObject(body)
	if err != nil {
		return "", err
	}
	objects := []map[string]common.RawMessage{fields}
	if _, exists := fields["generate_content_request"]; exists {
		return "", geminiCacheInvalid("Use generateContentRequest instead of generate_content_request")
	}
	if nested, present := fields["generateContentRequest"]; present {
		var nestedFields map[string]common.RawMessage
		if !gjson.ParseBytes(nested).IsObject() || common.Unmarshal(nested, &nestedFields) != nil {
			return "", geminiCacheInvalid("generateContentRequest must be an object")
		}
		objects = append(objects, nestedFields)
	}
	name := ""
	for _, object := range objects {
		if _, exists := object["cached_content"]; exists {
			return "", geminiCacheInvalid("Use cachedContent instead of cached_content")
		}
		if raw, exists := object["cachedContent"]; exists {
			var current string
			if common.Unmarshal(raw, &current) != nil || current == "" {
				return "", geminiCacheInvalid("cachedContent must be a resource name")
			}
			if name != "" && name != current {
				return "", geminiCacheInvalid("Conflicting cachedContent references")
			}
			name = current
		}
	}
	if name != "" {
		for _, object := range objects {
			for _, field := range []string{"systemInstruction", "system_instruction", "tools", "toolConfig", "tool_config"} {
				if _, exists := object[field]; exists {
					return "", geminiCacheInvalid("cachedContent cannot be combined with systemInstruction, tools or toolConfig")
				}
			}
		}
	}
	return name, nil
}

// HasGeminiCacheReference is a cheap, duplicate-safe preflight used by the
// relay middleware. It avoids imposing strict cache validation limits on every
// ordinary Gemini request while still recognizing escaped and nested aliases.
func HasGeminiCacheReference(body []byte) bool {
	root := gjson.ParseBytes(body)
	if !root.IsObject() {
		return false
	}
	found := false
	root.ForEach(func(key, child gjson.Result) bool {
		switch key.String() {
		case "cachedContent", "cached_content":
			found = true
		case "generateContentRequest", "generate_content_request":
			if child.IsObject() {
				child.ForEach(func(nestedKey, _ gjson.Result) bool {
					if nestedKey.String() == "cachedContent" || nestedKey.String() == "cached_content" {
						found = true
					}
					return !found
				})
			}
		}
		return !found
	})
	return found
}

// GeminiCacheReferenceInReader probes the two protocol positions without
// materializing a request. Large inlineData/text strings are consumed through
// a fixed-size buffer; only short candidate field names are retained. The
// ordinary request decoder remains responsible for full JSON validation.
func GeminiCacheReferenceInReader(input io.Reader) (bool, error) {
	found, _, err := scanGeminiCacheReference(input, false)
	return found, err
}

// GeminiCacheReferenceNameInReader reads only bounded protocol resource names.
// It checks every occurrence to prevent duplicate or nested fields selecting a
// different account policy than the eventual request decoder.
func GeminiCacheReferenceNameInReader(input io.Reader) (string, error) {
	_, name, err := scanGeminiCacheReference(input, true)
	return name, err
}

func scanGeminiCacheReference(input io.Reader, captureName bool) (bool, string, error) {
	var reference string
	reader := bufio.NewReaderSize(input, 32*1024)
	depth := 0
	nestedRequest := false
	pendingRequest := false
	for {
		next, err := reader.ReadByte()
		if err == io.EOF {
			return reference != "", reference, nil
		}
		if err != nil {
			return false, "", err
		}
		switch next {
		case ' ', '\t', '\r', '\n':
			continue
		case '{', '[':
			if depth == 1 {
				nestedRequest = next == '{' && pendingRequest
			}
			pendingRequest = false
			depth++
		case '}', ']':
			if depth == 2 {
				nestedRequest = false
			}
			pendingRequest = false
			depth--
		case '"':
			inspect := depth == 1 || (depth == 2 && nestedRequest)
			key, err := readGeminiCacheJSONKey(reader, inspect)
			if err != nil {
				return false, "", err
			}
			pendingRequest = false
			if !inspect {
				continue
			}
			for {
				next, err = reader.ReadByte()
				if err == io.EOF {
					return reference != "", reference, nil
				}
				if err != nil {
					return false, "", err
				}
				if next != ' ' && next != '\t' && next != '\r' && next != '\n' {
					break
				}
			}
			if next != ':' {
				if err := reader.UnreadByte(); err != nil {
					return false, "", err
				}
				continue
			}
			switch key {
			case "cachedContent", "cached_content":
				if !captureName {
					return true, "", nil
				}
				for {
					next, err = reader.ReadByte()
					if err != nil {
						return false, "", io.ErrUnexpectedEOF
					}
					if next != ' ' && next != '\t' && next != '\r' && next != '\n' {
						break
					}
				}
				if next != '"' {
					return false, "", geminiCacheInvalid("cachedContent must be a resource name")
				}
				name, nameErr := readGeminiCacheJSONKey(reader, true)
				if nameErr != nil || name == "" {
					return false, "", geminiCacheInvalid("cachedContent must be a resource name")
				}
				if reference != "" && reference != name {
					return false, "", geminiCacheInvalid("Conflicting cachedContent references")
				}
				reference = name
			case "generateContentRequest", "generate_content_request":
				pendingRequest = depth == 1
			}
		default:
			pendingRequest = false
		}
	}
}

// readGeminiCacheJSONKey consumes a quoted JSON token after its opening quote.
// The longest reference alias fits even when every character uses a Unicode
// escape; oversized names and all non-protocol string values are skipped.
func readGeminiCacheJSONKey(reader *bufio.Reader, capture bool) (string, error) {
	var encoded [1024]byte
	length := 1
	encoded[0] = '"'
	escaped := false
	for {
		next, err := reader.ReadByte()
		if err == io.EOF {
			return "", io.ErrUnexpectedEOF
		}
		if err != nil {
			return "", err
		}
		if capture {
			if length == len(encoded) {
				capture = false
			} else {
				encoded[length] = next
				length++
			}
		}
		if escaped {
			escaped = false
			continue
		}
		if next == '\\' {
			escaped = true
			continue
		}
		if next != '"' {
			continue
		}
		if !capture {
			return "", nil
		}
		var key string
		if err := common.Unmarshal(encoded[:length], &key); err != nil {
			return "", err
		}
		return key, nil
	}
}

func ValidGeminiCachePublicID(id string) bool { return geminiCachePublicIDPattern.MatchString(id) }

func GeminiCacheKeyFingerprint(key string) string {
	digest := sha256.Sum256([]byte(key))
	return hex.EncodeToString(digest[:])
}

func GeminiCacheChannelBaseURL(channel *model.Channel) (string, error) {
	base, supported := model.GeminiCacheChannelBaseURL(channel)
	if !supported {
		return "", geminiCacheUnavailable()
	}
	return base, nil
}

func GeminiCacheUpstreamModel(channel *model.Channel, localModel string) (string, error) {
	if channel == nil || !geminiCacheModelPattern.MatchString(localModel) || !slices.Contains(strings.Split(channel.Models, ","), localModel) {
		return "", geminiCacheUnavailable()
	}
	mapping := make(map[string]string)
	if raw := channel.GetModelMapping(); raw != "" && common.UnmarshalJsonStr(raw, &mapping) != nil {
		return "", geminiCacheUnavailable()
	}
	current := localModel
	seen := make(map[string]bool)
	for {
		if seen[current] {
			return "", geminiCacheUnavailable()
		}
		seen[current] = true
		next, exists := mapping[current]
		baseModel := hostreasoning.BaseModelName(current)
		if (!exists || next == "") && baseModel != current {
			next = mapping[baseModel]
		}
		if next == "" || next == current {
			break
		}
		current = next
	}
	current = strings.TrimPrefix(current, "models/")
	if !geminiCacheModelPattern.MatchString(current) {
		return "", geminiCacheUnavailable()
	}
	return current, nil
}

// GeminiCacheBinding pins every later operation to the original upstream
// account, while permitting a multi-key channel to reorder unchanged keys.
func GeminiCacheBinding(channel *model.Channel, record *model.GeminiCachedContent) (string, int, error) {
	if record == nil || channel == nil || channel.Id != record.ChannelID {
		return "", 0, geminiCacheUnavailable()
	}
	base, err := GeminiCacheChannelBaseURL(channel)
	if err != nil || base != record.BaseURL || !slices.Contains(strings.Split(channel.Group, ","), record.Group) {
		return "", 0, geminiCacheUnavailable()
	}
	upstreamModel, err := GeminiCacheUpstreamModel(channel, record.Model)
	if err != nil || upstreamModel != record.UpstreamModel {
		return "", 0, geminiCacheUnavailable()
	}
	keys := []string{channel.Key}
	if channel.ChannelInfo.IsMultiKey {
		keys = channel.GetKeys()
	}
	for index, key := range keys {
		if key == "" || GeminiCacheKeyFingerprint(key) != record.KeyFingerprint {
			continue
		}
		if status, exists := channel.ChannelInfo.MultiKeyStatusList[index]; channel.ChannelInfo.IsMultiKey && exists && status != common.ChannelStatusEnabled {
			continue
		}
		return key, index, nil
	}
	return "", 0, geminiCacheUnavailable()
}

func geminiCacheUpstreamRequest(ctx context.Context, channel *model.Channel, key, method, resource string, payload any, mutation bool) ([]byte, error) {
	base, err := GeminiCacheChannelBaseURL(channel)
	if err != nil {
		return nil, err
	}
	if key == "" || strings.ContainsAny(key, "\r\n") {
		return nil, geminiCacheUnavailable()
	}
	body, err := common.Marshal(payload)
	if err != nil {
		return nil, geminiCacheInvalid("Unable to encode cached content request")
	}
	if payload == nil {
		body = nil
	}
	timeout := 5 * time.Minute
	if mutation {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, method, base+"/v1beta/"+resource, bytes.NewReader(body))
	if err != nil {
		return nil, geminiCacheUnavailable()
	}
	request.Header.Set("x-goog-api-key", key)
	request.Header.Set("Content-Type", "application/json")
	settings := channel.GetSetting()
	client, err := GetHttpClientWithProxySettings(settings.Proxy, settings)
	if err != nil {
		return nil, geminiCacheUnavailable()
	}
	boundedClient := *client
	boundedClient.Timeout = timeout
	boundedClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := boundedClient.Do(request)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, &GeminiCacheError{HTTPStatus: http.StatusGatewayTimeout, Status: "DEADLINE_EXCEEDED", Message: "Cached content upstream request timed out", OutcomeUncertain: true}
		}
		return nil, &GeminiCacheError{HTTPStatus: http.StatusBadGateway, Status: "UNAVAILABLE", Message: "Cached content upstream request failed", OutcomeUncertain: true}
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, geminiCacheMaxResponseBytes+1))
	if err != nil || len(responseBody) > geminiCacheMaxResponseBytes {
		return nil, &GeminiCacheError{HTTPStatus: http.StatusBadGateway, Status: "INTERNAL", Message: "Invalid cached content response from upstream", OutcomeUncertain: true}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		status := "INTERNAL"
		code := response.StatusCode
		switch code {
		case 400:
			status = "INVALID_ARGUMENT"
		case 401, 403:
			status = "PERMISSION_DENIED"
			code = http.StatusForbidden
		case 404:
			status = "NOT_FOUND"
		case 409:
			status = "FAILED_PRECONDITION"
		case 429:
			status = "RESOURCE_EXHAUSTED"
		case 504:
			status = "DEADLINE_EXCEEDED"
		default:
			code = http.StatusBadGateway
		}
		return nil, &GeminiCacheError{HTTPStatus: code, Status: status, Message: "Cached content upstream request rejected (" + status + ")", OutcomeUncertain: response.StatusCode >= 500 || response.StatusCode == http.StatusRequestTimeout}
	}
	return responseBody, nil
}

func CountGeminiCacheTokens(ctx context.Context, channel *model.Channel, key string, request *GeminiCacheCreateRequest, upstreamModel string) (int, error) {
	if request == nil || !geminiCacheModelPattern.MatchString(upstreamModel) {
		return 0, geminiCacheInvalid("Invalid cached content request")
	}
	generate := make(map[string]common.RawMessage)
	for _, field := range []string{"contents", "systemInstruction", "tools", "toolConfig"} {
		if value, exists := request.Body[field]; exists {
			generate[field] = value
		}
	}
	generate["model"], _ = common.Marshal("models/" + upstreamModel)
	response, err := geminiCacheUpstreamRequest(ctx, channel, key, http.MethodPost, "models/"+upstreamModel+":countTokens", map[string]any{"generateContentRequest": generate}, false)
	if err != nil {
		return 0, err
	}
	var result struct {
		TotalTokens *int `json:"totalTokens"`
	}
	if common.Unmarshal(response, &result) != nil || result.TotalTokens == nil || *result.TotalTokens < 1 || *result.TotalTokens > GeminiCacheMaxTokens {
		return 0, geminiCacheBadUpstream()
	}
	return *result.TotalTokens, nil
}

func parseGeminiCacheUpstreamResource(body []byte, upstreamModel string) (*GeminiCacheUpstreamResource, error) {
	var response struct {
		Name          string `json:"name"`
		Model         string `json:"model"`
		DisplayName   string `json:"displayName"`
		ExpireTime    string `json:"expireTime"`
		UsageMetadata struct {
			TotalTokenCount *int `json:"totalTokenCount"`
		} `json:"usageMetadata"`
	}
	if common.Unmarshal(body, &response) != nil || !geminiCacheUpstreamNamePattern.MatchString(response.Name) || strings.TrimPrefix(response.Model, "models/") != upstreamModel || response.UsageMetadata.TotalTokenCount == nil || *response.UsageMetadata.TotalTokenCount < 1 || *response.UsageMetadata.TotalTokenCount > GeminiCacheMaxTokens {
		return nil, geminiCacheBadUpstream()
	}
	expiry, err := time.Parse(time.RFC3339Nano, response.ExpireTime)
	if err != nil || expiry.Nanosecond() != 0 || !expiry.After(time.Now()) || expiry.After(time.Now().Add(GeminiCacheMaxTTL)) {
		return nil, geminiCacheBadUpstream()
	}
	return &GeminiCacheUpstreamResource{Name: response.Name, Model: upstreamModel, DisplayName: response.DisplayName, TotalTokens: *response.UsageMetadata.TotalTokenCount, ExpireTime: expiry}, nil
}

func CreateGeminiCachedContent(ctx context.Context, channel *model.Channel, key string, request *GeminiCacheCreateRequest, upstreamModel string) (*GeminiCacheUpstreamResource, error) {
	if request == nil || !geminiCacheModelPattern.MatchString(upstreamModel) {
		return nil, geminiCacheInvalid("Invalid cached content request")
	}
	payload := maps.Clone(request.Body)
	payload["model"], _ = common.Marshal("models/" + upstreamModel)
	body, err := geminiCacheUpstreamRequest(ctx, channel, key, http.MethodPost, "cachedContents", payload, false)
	if err != nil {
		return nil, err
	}
	resource, validationErr := parseGeminiCacheUpstreamResource(body, upstreamModel)
	if validationErr == nil && resource.ExpireTime.After(request.ExpiresAt) {
		validationErr = geminiCacheBadUpstream()
	}
	if validationErr != nil {
		// A malformed resource must not leave a billable cache behind if its
		// resource name is still recoverable. Never let client cancellation
		// prevent this bounded compensating delete.
		var partial struct {
			Name string `json:"name"`
		}
		cacheErr := &GeminiCacheError{HTTPStatus: http.StatusBadGateway, Status: "INTERNAL", Message: "Invalid cached content response from upstream", OutcomeUncertain: true}
		if common.Unmarshal(body, &partial) == nil && geminiCacheUpstreamNamePattern.MatchString(partial.Name) {
			cacheErr.UpstreamName = partial.Name
			cacheErr.OutcomeUncertain = DeleteGeminiCachedContent(context.WithoutCancel(ctx), channel, key, partial.Name) != nil
		}
		return nil, cacheErr
	}
	return resource, nil
}

func PatchGeminiCachedContent(ctx context.Context, channel *model.Channel, key, upstreamName string, patch *GeminiCachePatchRequest, upstreamModel string) (*GeminiCacheUpstreamResource, error) {
	if patch == nil || !geminiCacheUpstreamNamePattern.MatchString(upstreamName) {
		return nil, geminiCacheInvalid("Invalid cached content update")
	}
	body, err := geminiCacheUpstreamRequest(ctx, channel, key, http.MethodPatch, upstreamName+"?updateMask=expireTime", patch.Body, true)
	if err != nil {
		return nil, err
	}
	resource, err := parseGeminiCacheUpstreamResource(body, upstreamModel)
	if err == nil && (resource.Name != upstreamName || resource.ExpireTime.After(patch.ExpiresAt)) {
		err = geminiCacheBadUpstream()
	}
	if err != nil {
		return nil, &GeminiCacheError{HTTPStatus: http.StatusBadGateway, Status: "INTERNAL", Message: "Invalid cached content response from upstream", OutcomeUncertain: true, UpstreamName: upstreamName}
	}
	return resource, err
}

func GetGeminiCachedContentUpstream(ctx context.Context, channel *model.Channel, key, upstreamName, upstreamModel string) (*GeminiCacheUpstreamResource, error) {
	if !geminiCacheUpstreamNamePattern.MatchString(upstreamName) {
		return nil, geminiCacheUnavailable()
	}
	body, err := geminiCacheUpstreamRequest(ctx, channel, key, http.MethodGet, upstreamName, nil, true)
	if err != nil {
		return nil, err
	}
	resource, err := parseGeminiCacheUpstreamResource(body, upstreamModel)
	if err == nil && resource.Name != upstreamName {
		return nil, geminiCacheBadUpstream()
	}
	return resource, err
}

func DeleteGeminiCachedContent(ctx context.Context, channel *model.Channel, key, upstreamName string) error {
	if !geminiCacheUpstreamNamePattern.MatchString(upstreamName) {
		return geminiCacheUnavailable()
	}
	_, err := geminiCacheUpstreamRequest(ctx, channel, key, http.MethodDelete, upstreamName, nil, true)
	var apiErr *GeminiCacheError
	if errors.As(err, &apiErr) && apiErr.HTTPStatus == http.StatusNotFound {
		return nil
	}
	return err
}

func GeminiCacheClientResponse(record *model.GeminiCachedContent) map[string]any {
	response := map[string]any{
		"name":          "cachedContents/" + record.PublicID,
		"model":         "models/" + record.Model,
		"createTime":    time.Unix(record.CreatedAt, 0).UTC().Format(time.RFC3339),
		"updateTime":    time.Unix(record.UpdatedAt, 0).UTC().Format(time.RFC3339),
		"expireTime":    time.Unix(record.ExpiresAt, 0).UTC().Format(time.RFC3339),
		"usageMetadata": map[string]any{"totalTokenCount": record.TotalTokens},
	}
	if record.DisplayName != "" {
		response["displayName"] = record.DisplayName
	}
	return response
}
