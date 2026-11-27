package main

import (
	"strings"
	"testing"
)

func TestParseModelAlias(t *testing.T) {
	alias, ok := parseModelAlias("glm-5.3=zai-glm-5-3")
	if !ok || alias.Client != "glm-5.3" || alias.Upstream != "zai-glm-5-3" {
		t.Fatalf("alias = %#v ok=%v", alias, ok)
	}
	alias, ok = parseModelAlias("mistral-vibe-cli-latest")
	if !ok || alias.Upstream != alias.Client {
		t.Fatalf("plain alias = %#v", alias)
	}
}

func TestReplacePluginBlock(t *testing.T) {
	config := "plugins:\n  configs:\n    other:\n      enabled: false\n    mistral:\n      enabled: true\n      api-key: \"old\"\ncredential-concurrency:\n  cleanup-interval: 5s\n"
	updated, err := replacePluginBlock(config, "    mistral:\n      enabled: true\n      api-key: \"new\"\n")
	if err != nil {
		t.Fatal(err)
	}
	if !stringsContains(updated, "api-key: \"new\"") || !stringsContains(updated, "credential-concurrency:") || stringsContains(updated, "old") {
		t.Fatalf("updated:\n%s", updated)
	}
}

func stringsContains(s, part string) bool {
	return len(s) >= len(part) && (s == part || len(part) == 0 || (func() bool { return stringIndex(s, part) >= 0 })())
}

func stringIndex(s, part string) int {
	for i := 0; i+len(part) <= len(s); i++ {
		if s[i:i+len(part)] == part {
			return i
		}
	}
	return -1
}

func TestOldAPIKeyMigrates(t *testing.T) {
	raw := "enabled: true\nbase-url: \"https://api.mistral.ai/v1\"\napi-key: \"mstrl_test\"\nmodels:\n  - \"deepseek-v4.1-flash=deepseek-v4-1-flash-1017\"\n"
	got := parseSettings([]byte(raw))
	cred, ok := got.pick()
	if !ok || cred.APIKey != "mstrl_test" {
		t.Fatalf("cred %#v ok=%v", cred, ok)
	}
	upstream, _ := got.upstream("deepseek-v4.1-flash")
	if upstream != "deepseek-v4-1-flash-1017" {
		t.Fatalf("upstream %s", upstream)
	}
}

func TestParseIgnoreProxySetting(t *testing.T) {
	defaults := parseSettings(nil)
	if defaults.IgnoreProxy {
		t.Fatal("ignore-proxy must default off")
	}
	got := parseSettings([]byte("ignore-proxy: true\n"))
	if !got.IgnoreProxy {
		t.Fatal("ignore-proxy should parse as on")
	}
	off := parseSettings([]byte("ignore-proxy: false\n"))
	if off.IgnoreProxy {
		t.Fatal("ignore-proxy should parse as off")
	}
}

func TestRenderPluginBlockIncludesIgnoreProxy(t *testing.T) {
	block := renderPluginBlock(settings{
		BaseURL: "https://api.mistral.ai/v1", LogRetention: 200, IgnoreProxy: true,
		Failover: true, CooldownSeconds: 60,
	})
	if !strings.Contains(block, "ignore-proxy: true") {
		t.Fatalf("block missing ignore-proxy:\n%s", block)
	}
}

func TestParseNormalizedYAMLKeepsCredentialKey(t *testing.T) {
	// CPA re-serializes the plugin block with yaml.v3 (4-space indent) before
	// calling plugin.register, so the nested api-key arrives indented. The
	// parser must not mistake it for the legacy top-level api-key.
	raw := `enabled: true
priority: 100
base-url: "https://x.example/v1"
log-retention: 200
stream-idle-seconds: 0
failover: true
ignore-proxy: true
cooldown-seconds: 60
credentials:
    - id: "key-1"
      label: "primary"
      api-key: "mstrl_abc"
      disabled: false
      priority: 10
    - id: "key-2"
      label: "backup"
      api-key: "mstrl_def"
      disabled: true
      priority: 5
models:
    - id: "m"
      upstream-id: "upstream-m"
      reasoning: ""
`
	got := parseSettings([]byte(raw))
	if len(got.Credentials) != 2 {
		t.Fatalf("credentials = %#v", got.Credentials)
	}
	if got.Credentials[0].APIKey != "mstrl_abc" || got.Credentials[1].APIKey != "mstrl_def" {
		t.Fatalf("keys were not parsed: %#v", got.Credentials)
	}
	if !got.Credentials[1].Disabled || got.Credentials[1].Priority != 5 {
		t.Fatalf("second credential = %#v", got.Credentials[1])
	}
	if !got.IgnoreProxy || !got.Failover {
		t.Fatalf("settings = %#v", got)
	}
}

func TestParseOldTopLevelAPIKeyStillWorks(t *testing.T) {
	raw := "enabled: true\nbase-url: \"https://x.example/v1\"\napi-key: \"mstrl_legacy\"\nmodels:\n  - \"client=upstream\"\n"
	got := parseSettings([]byte(raw))
	if len(got.Credentials) != 1 || got.Credentials[0].APIKey != "mstrl_legacy" {
		t.Fatalf("credentials = %#v", got.Credentials)
	}
}

func TestParseRenderedPluginBlockRoundTripsKeys(t *testing.T) {
	// The console writes the block with its own indentation; reading it back
	// must return the same credentials.
	original := settings{
		BaseURL: "https://api.mistral.ai/v1", LogRetention: 200, Failover: true,
		IgnoreProxy: true, CooldownSeconds: 60,
		Credentials: []credential{
			{ID: "a", Label: "A", APIKey: "mstrl_aaa", Priority: 10},
			{ID: "b", Label: "B", APIKey: "mstrl_bbb", Priority: 5},
		},
		Models: []modelAlias{{Client: "m", Upstream: "upstream-m"}},
	}
	block := renderPluginBlock(original)
	got := parseSettings([]byte(block))
	if len(got.Credentials) != 2 {
		t.Fatalf("credentials = %#v\nblock:\n%s", got.Credentials, block)
	}
	if got.Credentials[0].APIKey != "mstrl_aaa" || got.Credentials[1].APIKey != "mstrl_bbb" {
		t.Fatalf("keys = %#v", got.Credentials)
	}
	if !got.IgnoreProxy {
		t.Fatalf("settings = %#v", got)
	}
}

// A config written before stream-idle-seconds existed must keep the default
// guard. Reading the missing key as zero used to disable it silently.
func TestIdleDefaultSurvivesConfigWithoutKey(t *testing.T) {
	cfg := []byte("    mistral:\n      enabled: true\n      base-url: \"https://api.mistral.ai/v1\"\n      models:\n        - id: m\n          upstream-id: um\n")
	got := parseSettings(cfg)
	if got.StreamIdleSeconds != defaultStreamIdleSeconds {
		t.Fatalf("StreamIdleSeconds = %d, want the default %d", got.StreamIdleSeconds, defaultStreamIdleSeconds)
	}
	// An explicit 0 still means off, so the setting can be disabled on purpose.
	explicit := parseSettings([]byte("    mistral:\n      stream-idle-seconds: 0\n"))
	if explicit.StreamIdleSeconds != 0 {
		t.Fatalf("explicit 0 = %d, want 0", explicit.StreamIdleSeconds)
	}
	// And a custom value is kept.
	custom := parseSettings([]byte("    mistral:\n      stream-idle-seconds: 45\n"))
	if custom.StreamIdleSeconds != 45 {
		t.Fatalf("custom = %d, want 45", custom.StreamIdleSeconds)
	}
}

func TestChatEndpointSelection(t *testing.T) {
	cases := []struct {
		name   string
		base   string
		mode   string
		want   string
		compat bool
	}{
		{"official auto", defaultBaseURL, endpointAuto, openAICompatURL, true},
		{"official openai", defaultBaseURL, endpointOpenAI, openAICompatURL, true},
		{"official native", defaultBaseURL, endpointNative, defaultBaseURL + "/chat/completions", false},
		{"custom auto", "https://proxy.example/v1", endpointAuto, "https://proxy.example/v1/chat/completions", false},
		{"custom openai", "https://proxy.example/v1", endpointOpenAI, "https://proxy.example/openai/v1/chat/completions", true},
		{"custom native", "https://proxy.example/v1", endpointNative, "https://proxy.example/v1/chat/completions", false},
	}
	for _, tc := range cases {
		got, compat := (settings{BaseURL: tc.base, Endpoint: tc.mode}).chatEndpoint()
		if got != tc.want || compat != tc.compat {
			t.Fatalf("%s: endpoint=%q compat=%v, want %q %v", tc.name, got, compat, tc.want, tc.compat)
		}
	}
}

func TestParseEndpointSetting(t *testing.T) {
	if got := parseSettings([]byte("endpoint: native\n")).Endpoint; got != endpointNative {
		t.Fatalf("endpoint = %q", got)
	}
	if got := parseSettings([]byte("endpoint: openai\n")).Endpoint; got != endpointOpenAI {
		t.Fatalf("endpoint = %q", got)
	}
	if got := parseSettings([]byte("endpoint: something-else\n")).Endpoint; got != endpointAuto {
		t.Fatalf("unknown endpoint = %q, want auto", got)
	}
	if got := parseSettings(nil).Endpoint; got != endpointAuto {
		t.Fatalf("default endpoint = %q, want auto", got)
	}
	// The wrapper YAML the host sends puts the plugin block under an indented
	// key; the field still has to be found.
	got := parseSettings([]byte("    mistral:\n      endpoint: native\n      base-url: \"https://api.mistral.ai/v1\"\n"))
	if got.Endpoint != endpointNative {
		t.Fatalf("nested endpoint = %q", got.Endpoint)
	}
}

func TestRenderPluginBlockIncludesEndpoint(t *testing.T) {
	block := renderPluginBlock(settings{BaseURL: defaultBaseURL})
	if !strings.Contains(block, `endpoint: "auto"`) {
		t.Fatalf("block missing endpoint:\n%s", block)
	}
}
