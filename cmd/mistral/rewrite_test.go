package main

import (
	"encoding/json"
	"testing"
)

func TestRequestMovesReasoningAndDropsStore(t *testing.T) {
	in := []byte(`{"model":"deepseek-v4.1-flash","store":false,"reasoning_effort":"max","messages":[{"role":"assistant","content":"4","reasoning_content":"add"},{"role":"user","content":"ok","refusal":null}]}`)
	out, ok := rewriteRequestBody(in, nil, "deepseek-v4-1-flash-1017", "deepseek-v4.1-flash", false)
	if !ok {
		t.Fatal("expected rewrite")
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	if _, exists := doc["store"]; exists {
		t.Fatal("store remains")
	}
	if doc["reasoning_effort"] != "max" {
		t.Fatalf("effort = %#v", doc["reasoning_effort"])
	}
	messages := doc["messages"].([]any)
	assistant := messages[0].(map[string]any)
	if _, exists := assistant["reasoning_content"]; exists {
		t.Fatal("reasoning_content remains")
	}
	parts := assistant["content"].([]any)
	if parts[0].(map[string]any)["type"] != "thinking" {
		t.Fatalf("content = %#v", assistant["content"])
	}
	if parts[1].(map[string]any)["text"] != "4" {
		t.Fatalf("text part = %#v", parts[1])
	}
	user := messages[1].(map[string]any)
	if _, exists := user["refusal"]; exists {
		t.Fatal("refusal remains")
	}
	if user["content"] != "ok" {
		t.Fatalf("user content = %#v", user["content"])
	}
}

func TestRequestIgnoresOtherModels(t *testing.T) {
	in := []byte(`{"store":false,"messages":[{"role":"assistant","content":"4","reasoning_content":"add"}]}`)
	if _, ok := rewriteRequestBody(in, nil, "gpt-5.6", "gpt-5.6", false); ok {
		t.Fatal("rewrote unrelated model")
	}
}

func TestStreamSplitsThinking(t *testing.T) {
	in := []byte(`{"choices":[{"delta":{"content":[{"type":"thinking","thinking":[{"type":"text","text":"The"}],"closed":true},{"type":"text","text":"OK"}]}}]}`)
	out, ok := rewriteResponseBody(in)
	if !ok {
		t.Fatal("expected rewrite")
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	delta := doc["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
	if delta["reasoning_content"] != "The" || delta["content"] != "OK" {
		t.Fatalf("delta = %#v", delta)
	}
}

func TestStreamLeavesStringContent(t *testing.T) {
	in := []byte(`{"choices":[{"delta":{"content":"OK"}}]}`)
	if _, ok := rewriteResponseBody(in); ok {
		t.Fatal("rewrote string content")
	}
}

func TestNonStreamMessage(t *testing.T) {
	in := []byte(`{"choices":[{"message":{"role":"assistant","content":[{"type":"thinking","thinking":"because"},{"type":"text","text":"4"}]}}]}`)
	out, ok := rewriteResponseBody(in)
	if !ok {
		t.Fatal("expected rewrite")
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	msg := doc["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["reasoning_content"] != "because" || msg["content"] != "4" {
		t.Fatalf("message = %#v", msg)
	}
}

// dsh and other OpenAI-compatible clients send "developer" for the system
// message and a pile of fields Mistral does not model. Every one of those was
// a 422 before the sanitizer existed.
func TestRequestDropsFieldsMistralRejects(t *testing.T) {
	in := []byte(`{
		"model":"deepseek-v4.1-flash",
		"store":false,
		"stream_options":{"include_usage":true},
		"max_completion_tokens":4096,
		"reasoning":{"effort":"max"},
		"logit_bias":{},
		"verbosity":"low",
		"messages":[
			{"role":"developer","content":"be brief"},
			{"role":"user","content":"hi","name":"bob"},
			{"role":"assistant","content":"ok","reasoning_content":"hm","refusal":null,"name":"a"},
			{"role":"user","content":[{"type":"input_text","text":"again"}]}
		]}`)
	out, ok := rewriteRequestBody(in, []string{"*"}, "deepseek-v4-1-flash-1017", "deepseek-v4.1-flash", false)
	if !ok {
		t.Fatal("expected rewrite")
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"store", "logit_bias", "verbosity", "reasoning"} {
		if _, exists := doc[key]; exists {
			t.Fatalf("%s survived", key)
		}
	}
	if doc["reasoning_effort"] != "max" {
		t.Fatalf("reasoning.effort was not lifted: %#v", doc)
	}
	if _, exists := doc["stream_options"]; !exists {
		t.Fatal("stream_options is valid upstream and should survive")
	}
	messages := doc["messages"].([]any)
	system := messages[0].(map[string]any)
	if system["role"] != "system" {
		t.Fatalf("developer role survived: %#v", system)
	}
	if _, exists := messages[1].(map[string]any)["name"]; exists {
		t.Fatal("user name survived")
	}
	assistant := messages[2].(map[string]any)
	if _, exists := assistant["name"]; exists {
		t.Fatal("assistant name survived")
	}
	if _, exists := assistant["refusal"]; exists {
		t.Fatal("refusal survived")
	}
	parts := assistant["content"].([]any)
	if parts[0].(map[string]any)["type"] != "thinking" {
		t.Fatalf("reasoning was not folded into content: %#v", parts)
	}
	part := messages[3].(map[string]any)["content"].([]any)[0].(map[string]any)
	if part["type"] != "text" {
		t.Fatalf("input_text part survived: %#v", part)
	}
}

func TestEffortFollowsModelLadder(t *testing.T) {
	cases := []struct{ model, in, want string }{
		{"zai-glm-5-3", "xhigh", "max"},
		{"zai-glm-5-3", "low", "low"},
		{"mistral-vibe-cli-latest", "max", "high"},
		{"mistral-vibe-cli-latest", "high", "high"},
		{"deepseek-v4-1-flash-1017", "medium", "high"},
		{"deepseek-v4-1-flash-1017", "minimal", "low"},
		{"deepseek-v4-1-flash-1017", "xhigh", "xhigh"},
	}
	for _, tc := range cases {
		if got := normalizeEffort(tc.model, tc.in); got != tc.want {
			t.Fatalf("normalizeEffort(%q, %q) = %q, want %q", tc.model, tc.in, got, tc.want)
		}
	}
}

// Clients turn thinking off with "off" as often as with "none". It used to have
// no alias, so it fell through to the strongest level and the model thought
// exactly when the client asked it not to.
func TestEffortOffTurnsThinkingOff(t *testing.T) {
	for _, model := range []string{"mistral-vibe-cli-latest", "zai-glm-5-3", "mistral-large-latest"} {
		if got := normalizeEffort(model, "off"); got != "none" && got != "low" {
			t.Fatalf("normalizeEffort(%q, off) = %q, want a level that does not think", model, got)
		}
	}
	if got := normalizeEffort("mistral-vibe-cli-latest", "off"); got != "none" {
		t.Fatalf("off = %q, want none", got)
	}
}

// A typo or a value this plugin does not know must not silently buy the most
// expensive level. Leaving the field out keeps the model's own default.
func TestEffortUnknownValueKeepsModelDefault(t *testing.T) {
	for _, in := range []string{"bogus", "ultra", "thinking-harder"} {
		if got := normalizeEffort("mistral-large-latest", in); got != "" {
			t.Fatalf("normalizeEffort(%q) = %q, want empty so the field is dropped", in, got)
		}
	}
}

// An effort the model cannot express goes to the nearest level it has, and a
// tie resolves to the cheaper side rather than the strongest.
func TestEffortNearestLevelMatchesUpstream(t *testing.T) {
	cases := []struct{ model, in, want string }{
		{"zai-glm-5-3", "none", "low"},
		{"zai-glm-5-3", "xhigh", "max"},
		{"mistral-vibe-cli-latest", "low", "high"},
		{"mistral-vibe-cli-latest", "medium", "high"},
	}
	for _, tc := range cases {
		if got := normalizeEffort(tc.model, tc.in); got != tc.want {
			t.Fatalf("normalizeEffort(%q, %q) = %q, want %q", tc.model, tc.in, got, tc.want)
		}
	}
}

// The compatibility endpoint speaks the OpenAI schema: reasoning_content is
// legal and must survive, while the standard OpenAI fields the old sanitizer
// did not know (seed/user/metadata/max_completion_tokens) must survive too.
func TestCompatRequestKeepsOpenAIFields(t *testing.T) {
	in := []byte(`{"model":"deepseek-v4.1-flash","store":false,"reasoning_effort":"high","service_tier":"auto","prompt_cache_retention":"24h","top_k":5,"seed":42,"user":"u1","metadata":{"a":"b"},"max_completion_tokens":32,"prompt_cache_key":"cache-1","messages":[{"role":"developer","content":"be brief"},{"role":"user","content":"hi","name":"bob"},{"role":"assistant","content":"ok","reasoning_content":"hm","prefix":true,"refusal":null,"name":"a","annotations":[]},{"role":"tool","tool_call_id":"c1","content":"r","name":"g"}]}`)
	out, ok := rewriteRequestBody(in, []string{"*"}, "deepseek-v4-1-flash-1017", "deepseek-v4.1-flash", true)
	if !ok {
		t.Fatal("expected rewrite")
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"store", "service_tier", "prompt_cache_retention", "top_k"} {
		if _, exists := doc[key]; exists {
			t.Fatalf("%s survived", key)
		}
	}
	for _, key := range []string{"seed", "user", "metadata", "max_completion_tokens", "prompt_cache_key"} {
		if _, exists := doc[key]; !exists {
			t.Fatalf("%s was dropped", key)
		}
	}
	messages := doc["messages"].([]any)
	if messages[0].(map[string]any)["role"] != "system" {
		t.Fatalf("developer role = %#v", messages[0])
	}
	if _, exists := messages[1].(map[string]any)["name"]; exists {
		t.Fatal("user name survived")
	}
	assistant := messages[2].(map[string]any)
	if assistant["reasoning_content"] != "hm" {
		t.Fatalf("reasoning_content = %#v", assistant["reasoning_content"])
	}
	if assistant["content"] != "ok" || assistant["name"] != "a" {
		t.Fatalf("assistant = %#v", assistant)
	}
	for _, key := range []string{"prefix", "refusal", "annotations"} {
		if _, exists := assistant[key]; exists {
			t.Fatalf("assistant %s survived", key)
		}
	}
	toolMsg := messages[3].(map[string]any)
	if _, exists := toolMsg["name"]; exists {
		t.Fatal("tool name survived")
	}
}

// A client replaying a native Mistral response sends thinking blocks. The
// compatibility route rejects those, so they are flattened back into
// reasoning_content instead of being forwarded as content parts.
func TestCompatFlattensNativeThinkingParts(t *testing.T) {
	in := []byte(`{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":[{"type":"thinking","thinking":[{"type":"text","text":"because"}],"closed":true},{"type":"text","text":"4"}]},{"role":"user","content":"ok"}]}`)
	out, ok := rewriteRequestBody(in, []string{"*"}, "deepseek-v4-1-flash-1017", "deepseek-v4.1-flash", true)
	if !ok {
		t.Fatal("expected rewrite")
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	assistant := doc["messages"].([]any)[1].(map[string]any)
	if assistant["reasoning_content"] != "because" {
		t.Fatalf("reasoning_content = %#v", assistant["reasoning_content"])
	}
	parts := assistant["content"].([]any)
	if len(parts) != 1 || parts[0].(map[string]any)["text"] != "4" {
		t.Fatalf("content = %#v", assistant["content"])
	}
}

func TestCompatStripsCacheControlAndNormalizesParts(t *testing.T) {
	in := []byte(`{"messages":[{"role":"user","content":[{"type":"input_text","text":"hi","cache_control":{"type":"ephemeral"}}]}]}`)
	out, ok := rewriteRequestBody(in, []string{"*"}, "deepseek-v4-1-flash-1017", "deepseek-v4.1-flash", true)
	if !ok {
		t.Fatal("expected rewrite")
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	part := doc["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)
	if part["type"] != "text" {
		t.Fatalf("type = %#v", part["type"])
	}
	if _, exists := part["cache_control"]; exists {
		t.Fatal("cache_control survived")
	}
}
