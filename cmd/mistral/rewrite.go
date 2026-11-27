package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
)

var defaultModels = []string{
	"deepseek-v4-1-flash-1017",
	"deepseek-v4.1-flash",
	"zai-glm-5-3",
	"zai-glm-5-3-flash",
	"glm-5.3",
	"glm-5.3-flash",
	"mistral-vibe-cli-latest",
	"mistral-vibe-cli-with-tools",
	"mistral-vibe-cli-fast",
	"mistral-vibe-cli-*",
}

func modelMatches(patterns []string, names ...string) bool {
	if len(patterns) == 0 {
		patterns = defaultModels
	}
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		for _, pattern := range patterns {
			pattern = strings.TrimSpace(pattern)
			if pattern == "" {
				continue
			}
			if pattern == "*" || pattern == name {
				return true
			}
			ok, err := filepath.Match(pattern, name)
			if err == nil && ok {
				return true
			}
		}
	}
	return false
}

func parseModelPatterns(configYAML []byte) []string {
	text := strings.TrimSpace(string(configYAML))
	if text == "" {
		return append([]string(nil), defaultModels...)
	}
	if strings.HasPrefix(text, "{") {
		var doc struct {
			Models []string `json:"models"`
		}
		if json.Unmarshal(configYAML, &doc) == nil && len(doc.Models) > 0 {
			return doc.Models
		}
		return append([]string(nil), defaultModels...)
	}
	lines := strings.Split(text, "\n")
	var patterns []string
	inModels := false
	for _, line := range lines {
		trim := strings.TrimSpace(line)
		if trim == "" || strings.HasPrefix(trim, "#") {
			continue
		}
		if !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
			inModels = strings.HasPrefix(trim, "models:")
			continue
		}
		if !inModels || !strings.HasPrefix(trim, "-") {
			continue
		}
		item := strings.TrimSpace(strings.TrimPrefix(trim, "-"))
		item = strings.Trim(item, `"'`)
		if item != "" {
			patterns = append(patterns, item)
		}
	}
	if len(patterns) == 0 {
		return append([]string(nil), defaultModels...)
	}
	return patterns
}

// Mistral validates the chat body against a strict schema and answers 422 with
// "Extra inputs are not permitted" for anything it does not know. OpenAI and
// Responses clients send plenty of such fields, so drop them before forwarding.
var unsupportedTopLevelFields = []string{
	"store", "logit_bias", "web_search", "web_search_options",
	"continue_final_message", "prefix", "verbosity", "reasoning",
	"text", "instructions", "input", "top_k", "prompt_mode",
}

// The OpenAI-compatible facade accepts the standard OpenAI fields the native
// route 422s on (developer, seed, user, metadata, max_completion_tokens,
// prompt_cache_key, store=false, reasoning_content), but it still rejects a
// handful of real OpenAI parameters outright. Strip those, and always strip
// store: the facade only accepts false, and this API is stateless either way.
var compatUnsupportedTopLevelFields = []string{
	"store", "logit_bias", "web_search", "web_search_options",
	"continue_final_message", "prefix", "verbosity", "reasoning",
	"text", "instructions", "input", "top_k", "prompt_mode",
	"service_tier", "prompt_cache_retention", "cache_control",
}

// Each Mistral message role accepts its own field set; a field that belongs to
// another role is a 422 rather than being ignored.
var allowedMessageFields = map[string]map[string]bool{
	"system":    {"role": true, "content": true},
	"user":      {"role": true, "content": true},
	"assistant": {"role": true, "content": true, "tool_calls": true, "prefix": true},
	"tool":      {"role": true, "content": true, "tool_call_id": true},
}

// The facade takes the OpenAI message shape. reasoning_content is legal here
// and stays in place; native Mistral thinking blocks are not, so they are
// flattened into reasoning_content before the request is sent.
var compatMessageFields = map[string]map[string]bool{
	"system":    {"role": true, "content": true},
	"user":      {"role": true, "content": true},
	"assistant": {"role": true, "content": true, "tool_calls": true, "reasoning_content": true, "name": true},
	"tool":      {"role": true, "content": true, "tool_call_id": true},
}

func rewriteRequestBody(body []byte, patterns []string, model, requested string, compat bool) ([]byte, bool) {
	if !modelMatches(patterns, model, requested) {
		return nil, false
	}
	var doc map[string]any
	if err := decodeJSON(body, &doc); err != nil {
		return nil, false
	}
	if !rewriteRequestDoc(doc, model, compat) {
		return nil, false
	}
	out, err := encodeJSON(doc)
	if err != nil {
		return nil, false
	}
	return out, true
}

// rewriteRequestDoc applies the request sanitizer to an already decoded body and
// reports whether anything changed. Callers that converted another protocol
// into a chat body hold that map already; running the sanitizer on it directly
// saves decoding and re-encoding a payload that can be hundreds of kilobytes.
func rewriteRequestDoc(doc map[string]any, model string, compat bool) bool {
	changed := false
	if reasoning, ok := doc["reasoning"].(map[string]any); ok {
		if effort, ok := reasoning["effort"].(string); ok && effort != "" {
			if _, exists := doc["reasoning_effort"]; !exists {
				doc["reasoning_effort"] = effort
			}
		}
	}
	fields := unsupportedTopLevelFields
	if compat {
		fields = compatUnsupportedTopLevelFields
	}
	for _, key := range fields {
		if _, ok := doc[key]; ok {
			delete(doc, key)
			changed = true
		}
	}
	if effort, ok := doc["reasoning_effort"].(string); ok {
		normalized := normalizeEffort(model, effort)
		if normalized != effort {
			if normalized == "" {
				delete(doc, "reasoning_effort")
			} else {
				doc["reasoning_effort"] = normalized
			}
			changed = true
		}
	}
	messages, ok := doc["messages"].([]any)
	if !ok {
		return changed
	}
	for _, item := range messages {
		msg, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if rewriteMessage(msg, compat) {
			changed = true
		}
	}
	return changed
}

func rewriteMessage(msg map[string]any, compat bool) bool {
	changed := false
	role, _ := msg["role"].(string)
	// OpenAI replaced the system role with "developer"; Mistral only knows the
	// original tag and rejects the alias outright.
	if role == "developer" {
		role = "system"
		msg["role"] = role
		changed = true
	}
	if compat {
		if flattenThinkingParts(msg) {
			changed = true
		}
		if reasoning := firstString(msg["reasoning"]); reasoning != "" && role == "assistant" {
			if firstString(msg["reasoning_content"]) == "" {
				msg["reasoning_content"] = reasoning
			}
			delete(msg, "reasoning")
			changed = true
		}
		if allowed, known := compatMessageFields[role]; known {
			for key := range msg {
				if allowed[key] {
					continue
				}
				delete(msg, key)
				changed = true
			}
		} else {
			for key := range msg {
				if key == "role" || key == "content" {
					continue
				}
				delete(msg, key)
				changed = true
			}
		}
	} else {
		if reasoning := firstString(msg["reasoning_content"], msg["reasoning"]); reasoning != "" && role == "assistant" {
			msg["content"] = appendThinking(msg["content"], reasoning)
			changed = true
		}
		if allowed, known := allowedMessageFields[role]; known {
			for key := range msg {
				if allowed[key] {
					continue
				}
				delete(msg, key)
				changed = true
			}
		} else {
			// An unrecognized role still has to be sanitized. Dropping only the
			// three known OpenAI extras left every other unknown field to reach the
			// upstream, where it is a 422. Keep the role and its content and let
			// everything else go: the request stays valid even if the upstream
			// rejects the role itself.
			for key := range msg {
				if key == "role" || key == "content" {
					continue
				}
				delete(msg, key)
				changed = true
			}
		}
	}
	if parts, ok := msg["content"].([]any); ok {
		if normalizeContentParts(parts) {
			changed = true
		}
	}
	return changed
}

// flattenThinkingParts turns native Mistral content blocks into the
// reasoning_content string the OpenAI facade accepts. A client replaying a
// response it got from the native route would otherwise send thinking blocks
// the facade cannot parse.
func flattenThinkingParts(msg map[string]any) bool {
	parts, ok := msg["content"].([]any)
	if !ok {
		return false
	}
	kept := make([]any, 0, len(parts))
	var thought strings.Builder
	sawThinking := false
	for _, item := range parts {
		part, isMap := item.(map[string]any)
		if isMap {
			if kind, _ := part["type"].(string); kind == "thinking" {
				sawThinking = true
				thought.WriteString(thinkingText(part["thinking"]))
				continue
			}
		}
		kept = append(kept, item)
	}
	if !sawThinking {
		return false
	}
	if len(kept) == 0 {
		msg["content"] = ""
	} else {
		msg["content"] = kept
	}
	if thought.Len() > 0 {
		msg["reasoning_content"] = joinExisting(msg["reasoning_content"], thought.String())
	}
	return true
}

// Responses-style text parts are not part of Mistral's content unions.
func normalizeContentParts(parts []any) bool {
	changed := false
	for _, item := range parts {
		part, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if _, ok := part["cache_control"]; ok {
			// Anthropic clients attach cache hints to content blocks. Neither
			// Mistral route knows the field, and it is a hard validation error.
			delete(part, "cache_control")
			changed = true
		}
		kind, _ := part["type"].(string)
		if kind != "input_text" && kind != "output_text" {
			continue
		}
		part["type"] = "text"
		changed = true
	}
	return changed
}

func appendThinking(content any, reasoning string) []any {
	thinking := map[string]any{
		"type":     "thinking",
		"thinking": []any{map[string]any{"type": "text", "text": reasoning}},
		"closed":   true,
	}
	switch typed := content.(type) {
	case []any:
		return append([]any{thinking}, typed...)
	case string:
		if typed == "" {
			return []any{thinking}
		}
		return []any{thinking, map[string]any{"type": "text", "text": typed}}
	default:
		if content == nil {
			return []any{thinking}
		}
		return []any{thinking, map[string]any{"type": "text", "text": content}}
	}
}

func rewriteResponseBody(body []byte) ([]byte, bool) {
	var doc map[string]any
	if err := decodeJSON(body, &doc); err != nil {
		return nil, false
	}
	choices, ok := doc["choices"].([]any)
	if !ok {
		return nil, false
	}
	changed := false
	for _, item := range choices {
		choice, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if msg, ok := choice["message"].(map[string]any); ok {
			if splitContentField(msg, "content", "reasoning_content") {
				changed = true
			}
		}
		if delta, ok := choice["delta"].(map[string]any); ok {
			if splitContentField(delta, "content", "reasoning_content") {
				changed = true
			}
		}
	}
	if !changed {
		return nil, false
	}
	out, err := encodeJSON(doc)
	if err != nil {
		return nil, false
	}
	return out, true
}

func splitContentField(obj map[string]any, contentKey, reasoningKey string) bool {
	parts, ok := obj[contentKey].([]any)
	if !ok {
		return false
	}
	reasoning, text, ok := splitParts(parts)
	if !ok {
		return false
	}
	if reasoning == "" && text == "" {
		return false
	}
	if text == "" {
		delete(obj, contentKey)
	} else {
		obj[contentKey] = text
	}
	if reasoning != "" {
		obj[reasoningKey] = joinExisting(obj[reasoningKey], reasoning)
	}
	return true
}

func splitParts(parts []any) (reasoning, text string, ok bool) {
	var reasoningBuf, textBuf strings.Builder
	sawThinking := false
	for _, item := range parts {
		part, isMap := item.(map[string]any)
		if !isMap {
			return "", "", false
		}
		kind, _ := part["type"].(string)
		switch kind {
		case "thinking":
			sawThinking = true
			reasoningBuf.WriteString(thinkingText(part["thinking"]))
		case "text":
			value, isString := part["text"].(string)
			if !isString {
				return "", "", false
			}
			textBuf.WriteString(value)
		default:
			return "", "", false
		}
	}
	if !sawThinking {
		return "", "", false
	}
	return reasoningBuf.String(), textBuf.String(), true
}

func thinkingText(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case []any:
		var buf strings.Builder
		for _, item := range typed {
			switch inner := item.(type) {
			case string:
				buf.WriteString(inner)
			case map[string]any:
				if text, ok := inner["text"].(string); ok {
					buf.WriteString(text)
				}
			}
		}
		return buf.String()
	default:
		return ""
	}
}

func joinExisting(current any, extra string) string {
	if text, ok := current.(string); ok && text != "" {
		return text + extra
	}
	return extra
}

func firstString(values ...any) string {
	for _, value := range values {
		text, ok := value.(string)
		if ok && text != "" {
			return text
		}
	}
	return ""
}

func decodeJSON(raw []byte, dest any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	return dec.Decode(dest)
}

func encodeJSON(value any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func rewriteStreamPayload(payload []byte) ([]byte, bool) {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("[DONE]")) {
		return nil, false
	}
	if bytes.HasPrefix(trimmed, []byte("data:")) {
		rest := bytes.TrimSpace(bytes.TrimPrefix(trimmed, []byte("data:")))
		rewritten, ok := rewriteResponseBody(rest)
		if !ok {
			return nil, false
		}
		return append([]byte("data: "), rewritten...), true
	}
	return rewriteResponseBody(trimmed)
}

// Mistral rejects any effort value outside the per-model list, and the lists
// differ: the Vibe CLI models only take high, the deepseek/GLM routes take a
// wider set. Values outside the list are a 400 on every path forward.
var effortByModel = map[string][]string{
	"mistral-vibe-cli-latest":     {"none", "high"},
	"mistral-vibe-cli-with-tools": {"none", "high"},
	"mistral-vibe-cli-fast":       {"none", "high"},
	"zai-glm-5-3":                 {"low", "high", "max"},
	"zai-glm-5-3-flash":           {"low", "high", "max"},
	// Verified against both the native and the OpenAI-compatible route: the
	// deepseek route accepts none/low/high/xhigh/max and rejects medium and
	// minimal with a 400. Leaving it on the generic ladder passed "medium"
	// through untouched, so a client that asked for medium always failed.
	"deepseek-v4-1-flash-1017": {"none", "low", "high", "xhigh", "max"},
}

var defaultEffortLadder = []string{"none", "low", "medium", "high", "xhigh", "max"}

// effortRank orders the standard levels from least to most thinking. It is what
// lets an out-of-ladder value land on the nearest level a model actually
// advertises instead of jumping to an extreme.
var effortRank = map[string]int{
	"none": 0, "low": 1, "medium": 2, "high": 3, "xhigh": 4, "max": 5,
}

// unsupported client values are remapped onto a standard level. "off" is how
// OpenAI-compatible clients turn thinking off, so it has to mean none; without
// this alias it fell through to the strongest level and forced the thinking the
// client asked to skip.
var effortAliases = map[string]string{
	"off":     "none",
	"minimal": "low",
	"default": "medium",
	"auto":    "medium",
}

// normalizeThinkingEffort maps a client effort onto a level the model accepts.
// An unknown value goes to the nearest advertised level, never to the extreme:
// a typo used to become the most expensive setting on the model silently, and
// it is not worth paying for a guess.
func normalizeEffort(model, effort string) string {
	effort = strings.ToLower(strings.TrimSpace(effort))
	if effort == "" {
		return ""
	}
	levels := effortByModel[model]
	if len(levels) == 0 {
		levels = defaultEffortLadder
	}
	for _, level := range levels {
		if level == effort {
			return effort
		}
	}
	target := effort
	if alias, ok := effortAliases[target]; ok {
		target = alias
	}
	if rank, ok := effortRank[target]; ok {
		// A model lists its levels cheapest first. Moving down is only allowed
		// when the client asked for less than the model can do at all, so a
		// client asking for low on a none/high model gets that model's lowest
		// level rather than silence: "low" means thinking, just not much.
		if rank <= effortRank[levels[0]] {
			return levels[0]
		}
		// Otherwise round up to the nearest level the model has. Rounding down
		// would silently do less work than the client asked for, and a typo used
		// to jump straight to the most expensive setting; going one step up is
		// neither.
		best := ""
		for _, level := range levels {
			if effortRank[level] >= rank {
				if best == "" || effortRank[level] < effortRank[best] {
					best = level
				}
			}
		}
		if best != "" {
			return best
		}
		// Nothing at or above the request: the model's strongest level is the
		// closest it can get.
		return levels[len(levels)-1]
	}
	// Not a level this plugin knows: keep the model's default rather than
	// guessing a value the API may reject or bill for.
	return ""
}
