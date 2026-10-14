package service

import (
	"bufio"
	"io"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/tidwall/gjson"
)

// HasOpenAIGeminiCacheReference probes only protocol fields, including every
// duplicate container. Tool schemas and message text never count as references.
func HasOpenAIGeminiCacheReference(body []byte) bool {
	found := false
	root := gjson.ParseBytes(body)
	if !root.IsObject() {
		return false
	}
	root.ForEach(func(key, value gjson.Result) bool {
		if strings.EqualFold(key.String(), "cached_content") || strings.EqualFold(key.String(), "cachedContent") {
			found = true
		}
		if strings.EqualFold(key.String(), "extra_body") && value.IsObject() {
			value.ForEach(func(extraKey, extraValue gjson.Result) bool {
				if strings.EqualFold(extraKey.String(), "cached_content") || strings.EqualFold(extraKey.String(), "cachedContent") {
					found = true
				}
				if strings.EqualFold(extraKey.String(), "google") && extraValue.IsObject() {
					extraValue.ForEach(func(googleKey, _ gjson.Result) bool {
						if strings.EqualFold(googleKey.String(), "cached_content") || strings.EqualFold(googleKey.String(), "cachedContent") {
							found = true
						}
						return !found
					})
				}
				return !found
			})
		}
		return !found
	})
	return found
}

// OpenAIGeminiCacheReferenceNameInReader scans a disk-backed request without
// buffering message content. Only extra_body.google.cached_content is accepted;
// aliases and duplicate references fail closed before channel selection.
func OpenAIGeminiCacheReferenceNameInReader(input io.Reader) (string, error) {
	reader := bufio.NewReaderSize(input, 32*1024)
	depth := 0
	extraBody, googleBody := false, false
	pendingExtra, pendingGoogle := false, false
	var reference string
	for {
		next, err := reader.ReadByte()
		if err == io.EOF {
			return reference, nil
		}
		if err != nil {
			return "", err
		}
		switch next {
		case ' ', '\t', '\r', '\n':
			continue
		case '{', '[':
			if depth == 1 {
				extraBody = next == '{' && pendingExtra
			}
			if depth == 2 {
				googleBody = next == '{' && extraBody && pendingGoogle
			}
			pendingExtra, pendingGoogle = false, false
			depth++
		case '}', ']':
			if depth == 3 {
				googleBody = false
			}
			if depth == 2 {
				extraBody = false
			}
			pendingExtra, pendingGoogle = false, false
			depth--
		case '"':
			inspect := depth == 1 || (depth == 2 && extraBody) || (depth == 3 && googleBody)
			key, err := readGeminiCacheJSONKey(reader, inspect)
			if err != nil {
				return "", err
			}
			pendingExtra, pendingGoogle = false, false
			if !inspect {
				continue
			}
			for {
				next, err = reader.ReadByte()
				if err == io.EOF {
					return reference, nil
				}
				if err != nil {
					return "", err
				}
				if next != ' ' && next != '\t' && next != '\r' && next != '\n' {
					break
				}
			}
			if next != ':' {
				if err := reader.UnreadByte(); err != nil {
					return "", err
				}
				continue
			}
			switch strings.ToLower(key) {
			case "extra_body":
				pendingExtra = depth == 1
			case "google":
				pendingGoogle = depth == 2 && extraBody
			case "cachedcontent", "cached_content":
				if key != "cached_content" || depth != 3 || !googleBody {
					return "", geminiCacheInvalid("Use extra_body.google.cached_content for OpenAI cached content requests")
				}
				if reference != "" {
					return "", geminiCacheInvalid("Duplicate JSON fields are not supported")
				}
				for {
					next, err = reader.ReadByte()
					if err != nil {
						return "", io.ErrUnexpectedEOF
					}
					if next != ' ' && next != '\t' && next != '\r' && next != '\n' {
						break
					}
				}
				if next != '"' {
					return "", geminiCacheInvalid("cachedContent must be a resource name")
				}
				name, nameErr := readGeminiCacheJSONKey(reader, true)
				if nameErr != nil || name == "" {
					return "", geminiCacheInvalid("cachedContent must be a resource name")
				}
				reference = name
			}
		default:
			pendingExtra, pendingGoogle = false, false
		}
	}
}

// ValidateOpenAIGeminiCacheReference protects a managed cache before request
// conversion, which otherwise drops unknown fields and merges system messages.
func ValidateOpenAIGeminiCacheReference(body []byte) (name, modelName string, err error) {
	fields, err := decodeGeminiCacheObject(body)
	if err != nil {
		return "", "", err
	}
	objects := []map[string]common.RawMessage{fields}
	current := fields
	for _, field := range []string{"extra_body", "google"} {
		var nested map[string]common.RawMessage
		if common.Unmarshal(current[field], &nested) != nil || nested == nil {
			return "", "", geminiCacheInvalid("Use extra_body.google.cached_content for OpenAI cached content requests")
		}
		objects = append(objects, nested)
		current = nested
	}
	for i, object := range objects {
		for field := range object {
			// Struct decoders accept case-insensitive field names, whereas maps
			// preserve them. Reject aliases before the two parsers can disagree.
			if strings.EqualFold(field, "cachedContent") {
				return "", "", geminiCacheInvalid("Use extra_body.google.cached_content for OpenAI cached content requests")
			}
			if strings.EqualFold(field, "cached_content") && (field != "cached_content" || i != len(objects)-1) {
				return "", "", geminiCacheInvalid("Use extra_body.google.cached_content for OpenAI cached content requests")
			}
			for _, canonical := range []string{"extra_body", "google", "model", "messages"} {
				if strings.EqualFold(field, canonical) && field != canonical {
					return "", "", geminiCacheInvalid("Invalid cached content request")
				}
			}
			for _, immutable := range []string{"systemInstruction", "system_instruction", "tools", "functions", "tool_choice", "function_call", "parallel_tool_calls", "toolConfig", "tool_config"} {
				if strings.EqualFold(field, immutable) {
					return "", "", geminiCacheInvalid("OpenAI cached content cannot be combined with system or developer messages, tools or tool configuration")
				}
			}
		}
	}
	if common.Unmarshal(current["cached_content"], &name) != nil || name == "" {
		return "", "", geminiCacheInvalid("cachedContent must be a resource name")
	}
	if common.Unmarshal(fields["model"], &modelName) != nil || modelName == "" {
		return "", "", geminiCacheInvalid("model is required")
	}
	if !geminiCacheModelPattern.MatchString(modelName) {
		return "", "", geminiCacheInvalid("Invalid model name")
	}
	var messages []struct {
		Role string `json:"role"`
	}
	if common.Unmarshal(fields["messages"], &messages) != nil || len(messages) == 0 {
		return "", "", geminiCacheInvalid("OpenAI cached content requires a non-empty messages array")
	}
	for _, message := range messages {
		if message.Role == "system" || message.Role == "developer" {
			return "", "", geminiCacheInvalid("OpenAI cached content cannot be combined with system or developer messages, tools or tool configuration")
		}
	}
	return name, modelName, nil
}
