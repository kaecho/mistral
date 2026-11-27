package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// The host re-serializes the auth document, so matching it by byte pattern
// missed harmless spacing differences and left the auth unhandled.
func TestAuthResponseMatchesAnySpacing(t *testing.T) {
	for _, body := range []string{
		`{"type":"mistral"}`,
		`{"type": "mistral"}`,
		`{"type" :  "mistral"}`,
		"{\n  \"type\": \"mistral\"\n}",
	} {
		var env envelope
		if err := json.Unmarshal(authResponse([]byte(body)), &env); err != nil {
			t.Fatalf("%s: %v", body, err)
		}
		if !env.OK {
			t.Fatalf("%s: auth response was not OK", body)
		}
		if !strings.Contains(string(authResponse([]byte(body))), `"Handled":true`) {
			t.Fatalf("%s: auth document not handled", body)
		}
	}
	if strings.Contains(string(authResponse([]byte(`{"type":"openai"}`))), `"Handled":true`) {
		t.Fatal("foreign auth document was claimed")
	}
}

// The limit is in bytes because callers bound log lines; measuring bytes and
// cutting runes let a Chinese message run well past the bound.
func TestTruncateTextRespectsByteLimit(t *testing.T) {
	text := strings.Repeat("错误信息", 200)
	for _, limit := range []int{40, 400, 800} {
		got := truncateText(text, limit)
		if len(got) > limit {
			t.Fatalf("truncateText(limit %d) returned %d bytes", limit, len(got))
		}
	}
	if got := truncateText("short", 100); got != "short" {
		t.Fatalf("short text changed: %q", got)
	}
	// A cut must land on a rune boundary, never mid-character.
	if got := truncateText(text, 41); !strings.HasSuffix(got, "...") && len(got) > 41 {
		t.Fatalf("odd cut: %q", got)
	}
}

// A label ending in a backslash used to consume the closing quote and produce
// a document the host could not parse.
func TestQuoteYAMLEscapesBackslash(t *testing.T) {
	cases := map[string]string{
		`C:\keys\`: `"C:\\keys\\"`,
		`say "hi"`: `"say \"hi\""`,
		"line":     `"line"`,
	}
	for in, want := range cases {
		if got := quoteYAML(in); got != want {
			t.Fatalf("quoteYAML(%q) = %s, want %s", in, got, want)
		}
	}
}

// An unrecognized role used to keep every unknown field, each of which is a 422
// from the upstream.
func TestUnknownRoleKeepsOnlyRoleAndContent(t *testing.T) {
	in := []byte(`{"model":"m","messages":[{"role":"weird","content":"hi","parallel_tool_calls":true,"cache_control":{"type":"ephemeral"}}]}`)
	out, ok := rewriteRequestBody(in, []string{"*"}, "upstream-model", "upstream-model", false)
	if !ok {
		t.Fatal("expected rewrite")
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	msg := doc["messages"].([]any)[0].(map[string]any)
	if len(msg) != 2 || msg["role"] != "weird" || msg["content"] != "hi" {
		t.Fatalf("unknown role message = %#v, want only role and content", msg)
	}
}

func TestExecuteUsesOpenAICompatEndpointByDefault(t *testing.T) {
	storeSettings(defaultSettings())
	clearAllCredentialHealth()
	t.Cleanup(func() {
		storeSettings(defaultSettings())
		clearAllCredentialHealth()
	})
	storeSettings(settings{
		BaseURL: defaultBaseURL, Endpoint: endpointAuto, Failover: true, CooldownSeconds: 60,
		Credentials: []credential{{ID: "a", Label: "A", APIKey: "ka", Priority: 10}},
		Models:      []modelAlias{{Client: "m", Upstream: "upstream-model"}},
	})
	original := upstreamRequest
	t.Cleanup(func() { upstreamRequest = original })
	var seenURL string
	var seenBody []byte
	upstreamRequest = func(callbackID string, ignoreProxy bool, method, url string, headers map[string][]string, body []byte) (int, []byte, error) {
		seenURL = url
		seenBody = append([]byte(nil), body...)
		return 200, []byte(`{"choices":[{"message":{"content":"4","reasoning_content":"because"}}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`), nil
	}
	request, _ := json.Marshal(map[string]any{
		"Model": "m", "SourceFormat": "openai",
		"OriginalRequest": []byte(`{"store":false,"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"4","reasoning_content":"because"},{"role":"user","content":"again"}]}`),
	})
	raw := execute(request, false)
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
		t.Fatalf("execute failed: %s", raw)
	}
	if seenURL != openAICompatURL {
		t.Fatalf("upstream URL = %q, want %q", seenURL, openAICompatURL)
	}
	body := decodePayload(t, seenBody)
	if _, exists := body["store"]; exists {
		t.Fatal("store was forwarded")
	}
	assistant := body["messages"].([]any)[1].(map[string]any)
	if assistant["reasoning_content"] != "because" {
		t.Fatalf("reasoning_content = %#v", assistant["reasoning_content"])
	}
	if _, exists := assistant["content"].([]any); exists {
		t.Fatalf("native thinking content was built: %#v", assistant["content"])
	}
}
