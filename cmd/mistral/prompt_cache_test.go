package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func decodePayload(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	return doc
}

func TestPromptCacheKeyKeepsClientKey(t *testing.T) {
	original := []byte(`{"model":"m","prompt_cache_key":"client-session","messages":[{"role":"user","content":"hi"}]}`)
	payload := []byte(`{"messages":[{"role":"user","content":"hi"}],"model":"upstream"}`)
	out, cache := applyPromptCacheRouting(payload, original, nil, nil, "upstream")
	if cache.KeySource != "client" {
		t.Fatalf("source = %q", cache.KeySource)
	}
	if cache.Key != "client-session" {
		t.Fatalf("key = %q", cache.Key)
	}
	if got := decodePayload(t, out)["prompt_cache_key"]; got != "client-session" {
		t.Fatalf("prompt_cache_key = %#v", got)
	}
}

func TestPromptCacheKeyRecoversKeyFromOriginal(t *testing.T) {
	// Responses and Claude bodies are converted into chat completions, which
	// drops unknown top-level fields; the raw client request still has the key.
	original := []byte(`{"prompt_cache_key":"client-session","input":"hi"}`)
	payload := []byte(`{"model":"upstream","messages":[{"role":"user","content":"hi"}]}`)
	out, cache := applyPromptCacheRouting(payload, original, nil, nil, "upstream")
	if cache.KeySource != "client" || cache.Key != "client-session" {
		t.Fatalf("source=%q key=%q", cache.KeySource, cache.Key)
	}
	if got := decodePayload(t, out)["prompt_cache_key"]; got != "client-session" {
		t.Fatalf("prompt_cache_key = %#v", got)
	}
}

func TestPromptCacheKeyIsIndependentOfClientAffinity(t *testing.T) {
	// The key and the affinity are separate signals. A client that already
	// carries the official affinity must not have it turned into the body key:
	// the key stays derived from the content root.
	headers := map[string][]string{"X-Affinity": {"root-session-uuid"}}
	payload := []byte(`{"model":"upstream","messages":[{"role":"user","content":"hi"}]}`)
	out, cache := applyPromptCacheRouting(payload, nil, nil, headers, "upstream")
	if cache.KeySource != "root" {
		t.Fatalf("key source = %q, want root", cache.KeySource)
	}
	if cache.Key == "" || cache.Key == "root-session-uuid" {
		t.Fatalf("unexpected key %q", cache.Key)
	}
	if got := decodePayload(t, out)["prompt_cache_key"]; got != cache.Key {
		t.Fatalf("prompt_cache_key = %#v, key = %q", got, cache.Key)
	}
}

func TestAffinityForwardsClientValueVerbatim(t *testing.T) {
	// The official Vibe client sends the conversation family's root session id
	// as x-affinity. Forwarding it unchanged is what keeps a fork on the pod its
	// parent already warmed; rehashing it would still route consistently, but a
	// client that later reconnects with the raw id would split from its own
	// earlier requests.
	headers := map[string][]string{"x-affinity": {"3f8c1d2e-root"}}
	payload := []byte(`{"model":"upstream","messages":[{"role":"user","content":"hi"}]}`)
	_, cache := applyPromptCacheRouting(payload, nil, nil, headers, "upstream")
	if cache.Affinity != "3f8c1d2e-root" {
		t.Fatalf("affinity = %q, want the client value verbatim", cache.Affinity)
	}
	if cache.AffinitySource != "client" {
		t.Fatalf("affinity source = %q", cache.AffinitySource)
	}
}

func TestAffinityFromRootSessionMetadata(t *testing.T) {
	// Without a client header the host's root session id is the next best
	// routing identity: a fork carries the root's id while keeping its own
	// session id as telemetry.
	payload := []byte(`{"model":"upstream","messages":[{"role":"system","content":"be brief"},{"role":"user","content":"hi"}]}`)
	metadata := map[string]any{"root_session_id": "root-7", "canonical_session_id": "child-9"}
	_, cache := applyPromptCacheRouting(payload, nil, metadata, nil, "upstream")
	if cache.Affinity != "root-7" {
		t.Fatalf("affinity = %q, want the root session id", cache.Affinity)
	}
	if cache.AffinitySource != "session" {
		t.Fatalf("affinity source = %q", cache.AffinitySource)
	}
	// The key still comes from the content root, not the session metadata.
	if cache.KeySource != "root" {
		t.Fatalf("key source = %q", cache.KeySource)
	}
}

func TestAffinityFallsBackToCacheKey(t *testing.T) {
	// No client header and no root metadata: the content root is the only
	// stable identity left, so both signals share it.
	payload := []byte(`{"model":"upstream","messages":[{"role":"system","content":"be brief"},{"role":"user","content":"hi"}]}`)
	_, cache := applyPromptCacheRouting(payload, nil, nil, nil, "upstream")
	if cache.Affinity == "" || cache.Affinity != cache.Key {
		t.Fatalf("affinity = %q, key = %q", cache.Affinity, cache.Key)
	}
	if cache.AffinitySource != "root" {
		t.Fatalf("affinity source = %q", cache.AffinitySource)
	}
}

func TestAffinityKeepsForkAndParentTogether(t *testing.T) {
	// A fork diverges in its content (a new first user message) but the host
	// still tags it with the family root, so both requests route to one pod.
	parent := []byte(`{"model":"upstream","messages":[{"role":"user","content":"original task"}]}`)
	fork := []byte(`{"model":"upstream","messages":[{"role":"user","content":"try another approach"}]}`)
	metadata := map[string]any{"root_session_id": "root-7"}
	_, parentCache := applyPromptCacheRouting(parent, nil, metadata, nil, "upstream")
	_, forkCache := applyPromptCacheRouting(fork, nil, metadata, nil, "upstream")
	if parentCache.Key == forkCache.Key {
		t.Fatal("diverged conversations should not share a content key")
	}
	if parentCache.Affinity != forkCache.Affinity {
		t.Fatalf("fork split the affinity: %q vs %q", parentCache.Affinity, forkCache.Affinity)
	}
}

func TestPromptCacheKeyFromContentRootIsStable(t *testing.T) {
	payload := []byte(`{"model":"upstream","messages":[{"role":"system","content":"be brief"},{"role":"user","content":"one"}]}`)
	first, cache := applyPromptCacheRouting(payload, nil, nil, nil, "upstream")
	if cache.KeySource != "root" {
		t.Fatalf("source = %q", cache.KeySource)
	}
	second, again := applyPromptCacheRouting(
		[]byte(`{"model":"upstream","messages":[{"role":"system","content":"be brief"},{"role":"user","content":"one"},{"role":"assistant","content":"a"},{"role":"user","content":"two"}]}`),
		nil, nil, nil, "upstream")
	if decodePayload(t, first)["prompt_cache_key"] != decodePayload(t, second)["prompt_cache_key"] {
		t.Fatal("the content root should keep the key stable across turns")
	}
	if cache.Key != again.Key {
		t.Fatalf("returned key changed: %q -> %q", cache.Key, again.Key)
	}
}

func TestPromptCacheKeyContentRootDiffersPerConversation(t *testing.T) {
	system := "You are a document translator. Keep every placeholder intact."
	first, _ := applyPromptCacheRouting(
		[]byte(`{"model":"upstream","messages":[{"role":"system","content":"`+system+`"},{"role":"user","content":"chunk one"}]}`),
		nil, nil, nil, "upstream")
	other, _ := applyPromptCacheRouting(
		[]byte(`{"model":"upstream","messages":[{"role":"system","content":"`+system+`"},{"role":"user","content":"chunk two"}]}`),
		nil, nil, nil, "upstream")
	if decodePayload(t, first)["prompt_cache_key"] == decodePayload(t, other)["prompt_cache_key"] {
		t.Fatal("different conversations should not share a key")
	}
}

func TestPromptCacheKeyCompactionReplayKeepsRoot(t *testing.T) {
	// The compaction request replays the whole history and appends its own
	// instruction as the last message; it must keep the conversation's key so
	// the replay lands on the pod that already holds the prefix.
	base := `{"model":"upstream","messages":[{"role":"system","content":"be brief"},{"role":"user","content":"one"},{"role":"assistant","content":"a"}]}`
	_, conversation := applyPromptCacheRouting([]byte(base), nil, nil, nil, "upstream")
	replay := `{"model":"upstream","messages":[{"role":"system","content":"be brief"},{"role":"user","content":"one"},{"role":"assistant","content":"a"},{"role":"user","content":"summarize the conversation above"}]}`
	_, replayCache := applyPromptCacheRouting([]byte(replay), nil, nil, nil, "upstream")
	if replayCache.KeySource != "root" || replayCache.Key != conversation.Key {
		t.Fatalf("compaction replay changed the key: %q -> %q (source %q)", conversation.Key, replayCache.Key, replayCache.KeySource)
	}
}

func TestPromptCacheKeyFallsBackToSessionMetadata(t *testing.T) {
	metadata := map[string]any{"canonical_session_id": "session-42"}
	first, cache := applyPromptCacheRouting([]byte(`{"model":"upstream"}`), nil, metadata, nil, "upstream")
	if cache.KeySource != "session" || cache.Key == "" {
		t.Fatalf("source=%q key=%q", cache.KeySource, cache.Key)
	}
	second, again := applyPromptCacheRouting([]byte(`{"model":"upstream"}`), nil, metadata, nil, "upstream")
	if decodePayload(t, first)["prompt_cache_key"] != decodePayload(t, second)["prompt_cache_key"] || cache.Key != again.Key {
		t.Fatal("session fallback should be stable")
	}
}

func TestPromptCacheKeySkipsEmptyRequests(t *testing.T) {
	out, cache := applyPromptCacheRouting([]byte(`{"model":"upstream"}`), nil, nil, nil, "upstream")
	if cache.Key != "" || cache.Affinity != "" {
		t.Fatalf("key=%q affinity=%q", cache.Key, cache.Affinity)
	}
	if _, exists := decodePayload(t, out)["prompt_cache_key"]; exists {
		t.Fatal("empty request should not get a cache key")
	}
}

func TestPromptCacheKeySurvivesProtocolConversion(t *testing.T) {
	// A Responses client that sent prompt_cache_key keeps it after conversion.
	body := []byte(`{"prompt_cache_key":"conv-1","input":"hi","reasoning":{"effort":"low"}}`)
	out, ok := toMistralChat(body, body, "openai-response", "mistral-vibe-cli-latest", false, false)
	if !ok {
		t.Fatal("expected conversion")
	}
	out, cache := applyPromptCacheRouting(out, body, nil, nil, "mistral-vibe-cli-latest")
	if cache.KeySource != "client" || cache.Key != "conv-1" {
		t.Fatalf("source=%q key=%q", cache.KeySource, cache.Key)
	}
	if got := decodePayload(t, out)["prompt_cache_key"]; got != "conv-1" {
		t.Fatalf("prompt_cache_key = %#v", got)
	}
}

func TestAffinityHeaderValue(t *testing.T) {
	if got := affinityHeaderValue("mstrl-abc123"); got != "mstrl-abc123" {
		t.Fatalf("token should pass through, got %q", got)
	}
	if got := affinityHeaderValue(""); got != "" {
		t.Fatalf("empty should stay empty, got %q", got)
	}
	hashed := affinityHeaderValue("bad\nvalue")
	if hashed == "" || strings.ContainsAny(hashed, "\n") {
		t.Fatalf("invalid value should be hashed, got %q", hashed)
	}
	if long := affinityHeaderValue(strings.Repeat("x", 200)); len(long) > 128 {
		t.Fatalf("long value should be hashed, got %q", long)
	}
}

func TestHeaderValueIsCaseInsensitive(t *testing.T) {
	headers := map[string][]string{"X-AFFINITY": {"root"}}
	if got := headerValue(headers, "x-affinity"); got != "root" {
		t.Fatalf("headerValue = %q", got)
	}
	if got := headerValue(headers, "x-session-affinity"); got != "" {
		t.Fatalf("unexpected match: %q", got)
	}
}

func TestExecuteSendsSeparateKeyAndAffinity(t *testing.T) {
	storeSettings(defaultSettings())
	clearAllCredentialHealth()
	t.Cleanup(func() {
		storeSettings(defaultSettings())
		clearAllCredentialHealth()
	})
	storeSettings(settings{
		BaseURL: "https://upstream.invalid/v1", Failover: true, CooldownSeconds: 60,
		Credentials: []credential{{ID: "a", Label: "A", APIKey: "ka", Priority: 10}},
		Models:      []modelAlias{{Client: "m", Upstream: "upstream-model"}},
	})
	original := upstreamRequest
	t.Cleanup(func() { upstreamRequest = original })
	var captured []byte
	var capturedHeaders map[string][]string
	upstreamRequest = func(callbackID string, ignoreProxy bool, method, url string, headers map[string][]string, body []byte) (int, []byte, error) {
		captured = append([]byte(nil), body...)
		capturedHeaders = headers
		return 200, []byte(`{"choices":[{"message":{"content":"OK"}}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`), nil
	}
	request, _ := json.Marshal(map[string]any{
		"Model":        "m",
		"SourceFormat": "openai",
		"Payload":      []byte(`{"messages":[{"role":"system","content":"be brief"},{"role":"user","content":"hi"}]}`),
		"Metadata":     map[string]any{"root_session_id": "root-abc", "canonical_session_id": "session-xyz"},
	})
	raw := execute(request, false)
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
		t.Fatalf("execute failed: %s", raw)
	}
	var doc map[string]any
	if err := json.Unmarshal(captured, &doc); err != nil {
		t.Fatalf("upstream body: %v", err)
	}
	key, _ := doc["prompt_cache_key"].(string)
	if key == "" {
		t.Fatal("prompt_cache_key was not injected")
	}
	if got := headerValue(capturedHeaders, promptCacheAffinityHeader); got != "root-abc" {
		t.Fatalf("x-affinity = %q, want the root session id", got)
	}
	if key == "root-abc" {
		t.Fatal("the cache key must stay the content root, not the affinity identity")
	}
}

func TestExecuteForwardsVibeAffinity(t *testing.T) {
	storeSettings(defaultSettings())
	clearAllCredentialHealth()
	t.Cleanup(func() {
		storeSettings(defaultSettings())
		clearAllCredentialHealth()
	})
	storeSettings(settings{
		BaseURL: "https://upstream.invalid/v1", Failover: true, CooldownSeconds: 60,
		Credentials: []credential{{ID: "a", Label: "A", APIKey: "ka", Priority: 10}},
		Models:      []modelAlias{{Client: "m", Upstream: "upstream-model"}},
	})
	original := upstreamRequest
	t.Cleanup(func() { upstreamRequest = original })
	var capturedHeaders map[string][]string
	upstreamRequest = func(callbackID string, ignoreProxy bool, method, url string, headers map[string][]string, body []byte) (int, []byte, error) {
		capturedHeaders = headers
		return 200, []byte(`{"choices":[{"message":{"content":"OK"}}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`), nil
	}
	request, _ := json.Marshal(map[string]any{
		"Model":        "m",
		"SourceFormat": "openai",
		"Payload":      []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
		"Headers":      map[string][]string{"x-affinity": {"vibe-root-session"}},
	})
	raw := execute(request, false)
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
		t.Fatalf("execute failed: %s", raw)
	}
	if got := headerValue(capturedHeaders, promptCacheAffinityHeader); got != "vibe-root-session" {
		t.Fatalf("x-affinity = %q, want the client's own value", got)
	}
}
