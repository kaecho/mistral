package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// resetQuotaState clears the process-wide quota map and points persistence at a
// scratch file, so tests never touch a real plugin-data directory.
func resetQuotaState(t *testing.T) string {
	t.Helper()
	path := t.TempDir() + "/quota.json"
	quotaMu.Lock()
	previousPath := quotaCachePath
	quotaCachePath = path
	quotaSnapshots = map[string]quotaSnapshot{}
	quotaSuspects = map[string]time.Time{}
	quotaCacheLoaded = true // do not read the real cache from disk
	quotaMu.Unlock()
	t.Cleanup(func() {
		quotaMu.Lock()
		quotaSnapshots = map[string]quotaSnapshot{}
		quotaSuspects = map[string]time.Time{}
		quotaCachePath = previousPath
		quotaCacheLoaded = true
		quotaMu.Unlock()
	})
	return path
}

func budgetPointer(percentage, initial float64, reset time.Time, payg bool) *quotaBudget {
	return &quotaBudget{
		UsagePercentage: percentage,
		InitialBudget:   initial,
		Currency:        "USD",
		ResetAt:         reset,
		PaygEnabled:     payg,
	}
}

const testWhoamiCHAT = `{"plan_type":"CHAT","plan_name":"EDU","api_base":"https://api.mistral.ai","vibe_base":"https://chat.mistral.ai"}`

func TestParseQuotaSnapshotFromBillingPayload(t *testing.T) {
	subscription := []byte(`{"plan_name":"EDU","payg_enabled":false,"vibe_payg_enabled":false,"enterprise":false}`)
	budget := []byte(`{"vibe_budget":{"usage_percentage":100.0,"initial_budget":150.0,"currency":"USD","reset_at":"2026-12-01T00:00:00Z","payg_enabled":false},"api_budget":{"usage_percentage":15.7401036,"initial_budget":15.0,"currency":"USD","reset_at":"2026-12-01T00:00:00Z","payg_enabled":false},"usage_percentage":15.7401036,"initial_budget":15.0,"currency":"USD","reset_at":"2026-12-01T00:00:00Z"}`)
	snapshot := parseQuotaSnapshot(subscription, budget, []byte(testWhoamiCHAT), time.Now())
	if snapshot.PlanName != "EDU" || snapshot.PlanType != "CHAT" {
		t.Fatalf("plan = %q / %q", snapshot.PlanName, snapshot.PlanType)
	}
	if snapshot.API == nil || snapshot.Vibe == nil {
		t.Fatalf("budgets missing: %#v", snapshot)
	}
	if snapshot.API.UsagePercentage != 15.7401036 || snapshot.API.InitialBudget != 15 {
		t.Fatalf("api budget = %#v", snapshot.API)
	}
	if snapshot.Applicable != "api" {
		t.Fatalf("applicable = %q, want api for plugin traffic", snapshot.Applicable)
	}
	if snapshot.autoDisabled() {
		t.Fatalf("an API allowance at 15.7%% must not auto-disable")
	}
	if !snapshot.API.ResetAt.Equal(time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("api reset = %v", snapshot.API.ResetAt)
	}
	if snapshot.exhaustedFor(false) {
		t.Fatalf("api budget should not be exhausted")
	}
	view := quotaForTest(t, snapshot, "s1")
	api, _ := view["api"].(map[string]any)
	if api == nil || api["currency"] != "USD" {
		t.Fatalf("api view = %#v", view["api"])
	}
	used, _ := api["used"].(float64)
	if used < 2.3610 || used > 2.3611 {
		t.Fatalf("used = %v", used)
	}
	if view["auto_disabled"] != false || view["applicable"] != "api" {
		t.Fatalf("view = %#v", view)
	}
	vibe, _ := view["vibe"].(map[string]any)
	if vibe == nil || vibe["exhausted"] != true {
		t.Fatalf("vibe view = %#v", view["vibe"])
	}
	apiView, _ := view["api"].(map[string]any)
	if apiView == nil || apiView["exhausted"] != false {
		t.Fatalf("api view = %#v", view["api"])
	}
	if view["reset_at"] != formatTime(time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("applicable reset = %#v", view["reset_at"])
	}
}

func quotaForTest(t *testing.T, snapshot quotaSnapshot, id string) map[string]any {
	t.Helper()
	resetQuotaState(t)
	setQuotaSnapshot(id, snapshot)
	return quotaView(id, true)
}

func TestParseQuotaSnapshotAggregateFallback(t *testing.T) {
	budget := []byte(`{"usage_percentage":42.5,"initial_budget":20.0,"currency":"EUR","reset_at":"2026-12-01","payg_enabled":false}`)
	snapshot := parseQuotaSnapshot(nil, budget, nil, time.Now())
	if snapshot.API == nil {
		t.Fatalf("aggregate budget was not used as the API allowance: %#v", snapshot)
	}
	if snapshot.API.UsagePercentage != 42.5 || snapshot.API.Currency != "EUR" || snapshot.API.InitialBudget != 20 {
		t.Fatalf("api budget = %#v", snapshot.API)
	}
	if !snapshot.API.ResetAt.Equal(time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("date-only reset = %v", snapshot.API.ResetAt)
	}
	if snapshot.Vibe != nil {
		t.Fatalf("vibe budget should be absent: %#v", snapshot.Vibe)
	}
	if snapshot.Applicable != "api" {
		t.Fatalf("a single API allowance should be applicable, got %q", snapshot.Applicable)
	}
	if snapshot.exhaustedFor(true) {
		t.Fatalf("missing vibe budget must not read as exhausted")
	}
}

func TestQuotaScopeResolution(t *testing.T) {
	future := time.Now().Add(48 * time.Hour)
	exhausted := func(currency string) *quotaBudget { return budgetPointer(100, 15, future, false) }
	fresh := func() *quotaBudget { return budgetPointer(1, 15, future, false) }
	cases := []struct {
		name      string
		api, vibe *quotaBudget
		want      string
		disabled  bool
	}{
		{"api allowance is always the plugin allowance", fresh(), exhausted("USD"), "api", false},
		{"exhausted api allowance disables the key", exhausted("USD"), fresh(), "api", true},
		{"exhausted vibe allowance never disables", fresh(), exhausted("USD"), "api", false},
		{"only a vibe allowance leaves the destination unknown", nil, exhausted("USD"), "", false},
		{"only an api allowance is enough", exhausted("USD"), nil, "api", true},
		{"no budgets never disables", nil, nil, "", false},
		{"payg api keeps the key usable", budgetPointer(140, 15, future, true), exhausted("USD"), "api", false},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			snapshot := quotaSnapshot{
				API: item.api, Vibe: item.vibe,
				FetchedAt: time.Now(),
			}
			snapshot.resolveApplicable()
			if snapshot.Applicable != item.want {
				t.Fatalf("applicable = %q, want %q", snapshot.Applicable, item.want)
			}
			if snapshot.autoDisabled() != item.disabled {
				t.Fatalf("autoDisabled = %t, want %t", snapshot.autoDisabled(), item.disabled)
			}
		})
	}
}

func TestQuotaExhaustedRules(t *testing.T) {
	future := time.Now().Add(48 * time.Hour)
	past := time.Now().Add(-48 * time.Hour)
	cases := []struct {
		name   string
		budget *quotaBudget
		want   bool
	}{
		{"nil", nil, false},
		{"below ceiling", budgetPointer(99.99, 15, future, false), false},
		{"at ceiling", budgetPointer(100, 15, future, false), true},
		{"over ceiling", budgetPointer(140, 15, future, false), true},
		{"payg keeps it usable", budgetPointer(140, 15, future, true), false},
		{"cycle already reset", budgetPointer(100, 15, past, false), false},
		{"no reset time", budgetPointer(100, 15, time.Time{}, false), true},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			if got := item.budget.quotaExhausted(); got != item.want {
				t.Fatalf("quotaExhausted() = %t, want %t", got, item.want)
			}
		})
	}
}

func TestAvailableCandidatesSkipSpentKeys(t *testing.T) {
	resetQuotaState(t)
	reset := time.Now().Add(24 * time.Hour)
	current := settings{
		BaseURL:             "https://api.mistral.ai/v1",
		ConsoleURL:          defaultConsoleURL,
		QuotaMonitoring:     true,
		QuotaRefreshSeconds: 60,
		Credentials: []credential{
			{ID: "api-spent", Label: "api-spent", APIKey: "k1", Priority: 30},
			{ID: "vibe-spent", Label: "vibe-spent", APIKey: "k2", Priority: 20},
			{ID: "fresh", Label: "fresh", APIKey: "k3", Priority: 10},
		},
		Models: []modelAlias{{Client: "m", Upstream: "mistral-large-latest"}},
	}
	setQuotaSnapshot("api-spent", quotaSnapshot{FetchedAt: time.Now(), Applicable: "api", API: budgetPointer(100, 15, reset, false)})
	setQuotaSnapshot("vibe-spent", quotaSnapshot{FetchedAt: time.Now(), Applicable: "vibe", Vibe: budgetPointer(100, 150, reset, false)})
	ids := credentialIDs(current.availableCandidates())
	if strings.Contains(ids, "api-spent") || strings.Contains(ids, "vibe-spent") || !strings.Contains(ids, "fresh") {
		t.Fatalf("candidates = %s", ids)
	}
	current.QuotaMonitoring = false
	ids = credentialIDs(current.availableCandidates())
	if !strings.Contains(ids, "api-spent") || !strings.Contains(ids, "vibe-spent") {
		t.Fatalf("quota monitoring off should not filter: %s", ids)
	}
	current.QuotaMonitoring = true
	// An errored read must never remove a credential from rotation.
	setQuotaSnapshot("vibe-spent", quotaSnapshot{Error: "billing budget unavailable (HTTP 403)", FetchedAt: time.Now()})
	if ids = credentialIDs(current.availableCandidates()); !strings.Contains(ids, "vibe-spent") {
		t.Fatalf("errored snapshot filtered a credential: %s", ids)
	}
	// Unknown scope with both allowances computed must not disable.
	setQuotaSnapshot("vibe-spent", quotaSnapshot{FetchedAt: time.Now(), PlanType: "", API: budgetPointer(100, 15, reset, false), Vibe: budgetPointer(100, 150, reset, false)})
	if ids = credentialIDs(current.availableCandidates()); !strings.Contains(ids, "vibe-spent") {
		t.Fatalf("unknown scope filtered a credential: %s", ids)
	}
}

func credentialIDs(items []credential) string {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	return strings.Join(ids, ",")
}

func TestRefreshQuotaCredentialAgainstBillingServer(t *testing.T) {
	resetQuotaState(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/v1/billing/subscription":
			_, _ = w.Write([]byte(`{"plan_name":"INDIVIDUAL","vibe_payg_enabled":false}`))
		case "/v1/billing/budget":
			_, _ = w.Write([]byte(`{"vibe_budget":{"usage_percentage":0.5,"initial_budget":255.0,"currency":"EUR","reset_at":"2026-12-01T00:00:00Z"},"api_budget":{"usage_percentage":0.0,"initial_budget":25.5,"currency":"EUR","reset_at":"2026-12-01T00:00:00Z"}}`))
		case "/api/vibe/whoami":
			_, _ = w.Write([]byte(`{"plan_type":"CHAT","plan_name":"INDIVIDUAL"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	current := settings{
		BaseURL:         server.URL + "/v1",
		ConsoleURL:      server.URL,
		QuotaMonitoring: true,
		Credentials:     []credential{{ID: "key", Label: "key", APIKey: "test-key"}},
	}
	snapshot := refreshQuotaCredential(current, current.Credentials[0], "")
	if snapshot.Error != "" {
		t.Fatalf("refresh failed: %s", snapshot.Error)
	}
	if snapshot.PlanName != "INDIVIDUAL" || snapshot.PlanType != "CHAT" || snapshot.Applicable != "api" {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	if snapshot.Vibe == nil || snapshot.API == nil || snapshot.Vibe.UsagePercentage != 0.5 || snapshot.Vibe.Currency != "EUR" {
		t.Fatalf("budgets = %#v / %#v", snapshot.API, snapshot.Vibe)
	}
	stored, ok := quotaSnapshotFor("key")
	if !ok || stored.PlanName != "INDIVIDUAL" || stored.Applicable != "api" {
		t.Fatalf("stored snapshot = %#v / %t", stored, ok)
	}
}

func TestRefreshQuotaWhoamiFailureLeavesScopeUnknown(t *testing.T) {
	resetQuotaState(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/vibe/whoami" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"vibe_budget":{"usage_percentage":100.0,"initial_budget":150.0,"currency":"USD","reset_at":"2026-12-01T00:00:00Z"},"api_budget":{"usage_percentage":100.0,"initial_budget":15.0,"currency":"USD","reset_at":"2026-12-01T00:00:00Z"}}`))
	}))
	defer server.Close()
	current := settings{BaseURL: server.URL + "/v1", ConsoleURL: server.URL, QuotaMonitoring: true}
	cred := credential{ID: "key", APIKey: "test-key"}
	snapshot := refreshQuotaCredential(current, cred, "")
	if snapshot.Error != "" {
		t.Fatalf("whoami failure must not be a billing error: %s", snapshot.Error)
	}
	if snapshot.Applicable != "api" {
		t.Fatalf("applicable = %q, want api without a scope", snapshot.Applicable)
	}
	if !quotaExhaustedFor(current, "key") {
		t.Fatalf("both API and Vibe allowances at 100%% must disable the credential")
	}
	view := quotaView("key", true)
	if view["auto_disabled"] != true || view["applicable"] != "api" {
		t.Fatalf("view = %#v", view)
	}
	for _, key := range []string{"api", "vibe"} {
		budget, _ := view[key].(map[string]any)
		if budget == nil || budget["exhausted"] != true {
			t.Fatalf("%s view = %#v", key, view[key])
		}
	}
}

func TestRefreshQuotaCredentialSurfacesBillingPermissionError(t *testing.T) {
	resetQuotaState(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()
	current := settings{BaseURL: server.URL + "/v1", ConsoleURL: server.URL, QuotaMonitoring: true}
	cred := credential{ID: "member", APIKey: "member-key"}
	snapshot := refreshQuotaCredential(current, cred, "")
	if snapshot.Error == "" || !strings.Contains(snapshot.Error, "403") {
		t.Fatalf("error not surfaced: %q", snapshot.Error)
	}
	if snapshot.haveBudgets() {
		t.Fatalf("403 must not produce fake budgets")
	}
	if quotaExhaustedFor(current, "member") {
		t.Fatalf("a billing permission error disabled the credential")
	}
}

func TestQuotaCacheSurvivesRestart(t *testing.T) {
	path := resetQuotaState(t)
	reset := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	setQuotaSnapshot("key", quotaSnapshot{
		PlanName: "EDU", PlanType: "CHAT", Applicable: "vibe", Currency: "USD", FetchedAt: time.Now(),
		API: budgetPointer(10, 15, reset, false), Vibe: budgetPointer(100, 150, reset, false),
	})
	if _, err := json.Marshal(quotaSnapshots); err != nil {
		t.Fatalf("snapshot is not serializable: %v", err)
	}
	quotaMu.Lock()
	quotaSnapshots = map[string]quotaSnapshot{}
	quotaCacheLoaded = false
	quotaMu.Unlock()
	loaded, ok := quotaSnapshotFor("key")
	if !ok || loaded.PlanName != "EDU" || loaded.Applicable != "vibe" || loaded.Vibe == nil {
		t.Fatalf("cache did not reload: %#v / %t (path %s)", loaded, ok, path)
	}
	if !quotaExhaustedFor(settings{QuotaMonitoring: true}, "key") {
		t.Fatalf("reloaded cache should keep the key exhausted")
	}
}

func TestParseResetTime(t *testing.T) {
	cases := map[string]time.Time{
		"2026-12-01T00:00:00Z":      time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC),
		"2026-12-01T08:00:00+08:00": time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC),
		"2026-12-01":                time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC),
		"":                          time.Time{},
		"not-a-date":                time.Time{},
	}
	for input, want := range cases {
		if got := parseResetTime(input); !got.Equal(want) {
			t.Fatalf("parseResetTime(%q) = %v, want %v", input, got, want)
		}
	}
	if got := parseResetTime("1796112000"); !got.Equal(time.Unix(1796112000, 0)) {
		t.Fatalf("unix reset = %v", got)
	}
}

func TestQuotaExhaustedEnvelope(t *testing.T) {
	resetQuotaState(t)
	reset := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	current := settings{
		QuotaMonitoring: true,
		Credentials:     []credential{{ID: "key", APIKey: "k", Label: "key"}},
	}
	setQuotaSnapshot("key", quotaSnapshot{PlanName: "EDU", PlanType: "CHAT", Applicable: "vibe", FetchedAt: time.Now(), Vibe: budgetPointer(100, 150, reset, false)})
	raw := string(quotaExhaustedEnvelope(current))
	if !strings.Contains(raw, "quota_exhausted") || !strings.Contains(raw, "EDU") || !strings.Contains(raw, "2026-12-01") {
		t.Fatalf("envelope = %s", raw)
	}
}

func TestQuotaSettingsRoundTrip(t *testing.T) {
	raw := "base-url: \"https://api.mistral.ai/v1\"\nconsole-url: \"https://console.mistral.ai\"\nquota-monitoring: false\nquota-refresh-seconds: 120\n"
	parsed := parseSettings([]byte(raw))
	if parsed.ConsoleURL != "https://console.mistral.ai" {
		t.Fatalf("console url = %q", parsed.ConsoleURL)
	}
	if parsed.QuotaMonitoring {
		t.Fatalf("quota monitoring should be off")
	}
	if parsed.QuotaRefreshSeconds != 120 {
		t.Fatalf("refresh seconds = %d", parsed.QuotaRefreshSeconds)
	}
	defaults := parseSettings(nil)
	if !defaults.QuotaMonitoring || defaults.QuotaRefreshSeconds != 60 || defaults.ConsoleURL != defaultConsoleURL {
		t.Fatalf("defaults = %t / %d / %q", defaults.QuotaMonitoring, defaults.QuotaRefreshSeconds, defaults.ConsoleURL)
	}
	rendered := renderPluginBlock(parsed)
	if !strings.Contains(rendered, "console-url:") || !strings.Contains(rendered, "quota-monitoring: false") || !strings.Contains(rendered, "quota-refresh-seconds: 120") {
		t.Fatalf("render missing quota keys:\n%s", rendered)
	}
	// An explicitly empty console URL must survive a parse/render round trip:
	// the console uses it to switch scope detection off.
	empty := parseSettings([]byte("console-url: \"\"\n"))
	if empty.ConsoleURL != "" {
		t.Fatalf("empty console url was replaced by %q", empty.ConsoleURL)
	}
	if emptyRendered := renderPluginBlock(empty); !strings.Contains(emptyRendered, "console-url: \"\"") {
		t.Fatalf("empty console url not rendered:\n%s", emptyRendered)
	}
}

func TestSubscriptionPaygFallback(t *testing.T) {
	reset := time.Now().Add(48 * time.Hour).Format(time.RFC3339)
	// The budget document omits the per-allowance payg flags; the
	// subscription-level flag is the account-wide fallback.
	subscription := []byte(`{"plan_name":"INDIVIDUAL","payg_enabled":true}`)
	budget := []byte(`{"vibe_budget":{"usage_percentage":100.0,"initial_budget":150.0,"currency":"USD","reset_at":"` + reset + `"},"api_budget":{"usage_percentage":100.0,"initial_budget":15.0,"currency":"USD","reset_at":"` + reset + `"}}`)
	snapshot := parseQuotaSnapshot(subscription, budget, []byte(testWhoamiCHAT), time.Now())
	if snapshot.API == nil || !snapshot.API.PaygEnabled {
		t.Fatalf("api payg should fall back to the subscription: %#v", snapshot.API)
	}
	if snapshot.autoDisabled() {
		t.Fatalf("pay-as-you-go must keep the key usable past 100%%")
	}
	// The Vibe flag does not govern API traffic.
	explicit := []byte(`{"plan_name":"INDIVIDUAL","payg_enabled":true,"vibe_payg_enabled":false}`)
	snapshot = parseQuotaSnapshot(explicit, budget, []byte(testWhoamiCHAT), time.Now())
	if snapshot.autoDisabled() {
		t.Fatalf("vibe_payg_enabled=false must not pause API traffic")
	}
	// A per-allowance API flag still wins over the account-wide one.
	noAPIPayg := []byte(`{"vibe_budget":{"usage_percentage":100.0,"initial_budget":150.0,"currency":"USD","reset_at":"` + reset + `"},"api_budget":{"usage_percentage":100.0,"initial_budget":15.0,"currency":"USD","reset_at":"` + reset + `","payg_enabled":false}}`)
	snapshot = parseQuotaSnapshot(subscription, noAPIPayg, []byte(testWhoamiCHAT), time.Now())
	if !snapshot.autoDisabled() {
		t.Fatalf("api payg=false should pause plugin traffic")
	}
}

func TestNumericResetAtIsAccepted(t *testing.T) {
	budget := []byte(`{"vibe_budget":{"usage_percentage":100.0,"initial_budget":150.0,"currency":"USD","reset_at":1796112000,"payg_enabled":false}}`)
	snapshot := parseQuotaSnapshot(nil, budget, nil, time.Now())
	if snapshot.Vibe == nil {
		t.Fatalf("numeric reset_at hid the allowance: %#v", snapshot)
	}
	if !snapshot.Vibe.ResetAt.Equal(time.Unix(1796112000, 0)) {
		t.Fatalf("reset = %v", snapshot.Vibe.ResetAt)
	}
}

func TestQuotaSuspectHoldsUntilFreshRead(t *testing.T) {
	resetQuotaState(t)
	reset := time.Now().Add(24 * time.Hour)
	current := settings{
		QuotaMonitoring: true,
		Credentials:     []credential{{ID: "key", APIKey: "k", Priority: 10}},
		Models:          []modelAlias{{Client: "m", Upstream: "mistral-large-latest"}},
	}
	markQuotaSuspect("key")
	if ids := credentialIDs(current.availableCandidates()); strings.Contains(ids, "key") {
		t.Fatalf("a 402 mark did not remove the credential: %s", ids)
	}
	// A read that started before the mark carries the pre-402 picture and
	// must not clear it.
	stale := time.Now().Add(-time.Minute)
	setQuotaSnapshot("key", quotaSnapshot{FetchedAt: stale, Applicable: "vibe"})
	if ids := credentialIDs(current.availableCandidates()); strings.Contains(ids, "key") {
		t.Fatalf("stale read cleared the 402 mark: %s", ids)
	}
	// A read started after the mark with usable budgets clears it.
	setQuotaSnapshot("key", quotaSnapshot{FetchedAt: time.Now(), Applicable: "vibe", Vibe: budgetPointer(10, 150, reset, false)})
	if ids := credentialIDs(current.availableCandidates()); !strings.Contains(ids, "key") {
		t.Fatalf("fresh read did not clear the 402 mark: %s", ids)
	}
	// A fresh exhausted read keeps the credential out through the normal rule.
	setQuotaSnapshot("key", quotaSnapshot{FetchedAt: time.Now(), Applicable: "vibe", Vibe: budgetPointer(100, 150, reset, false)})
	if ids := credentialIDs(current.availableCandidates()); strings.Contains(ids, "key") {
		t.Fatalf("exhausted read did not remove the credential: %s", ids)
	}
}

func TestAdminQuotaActionReadsBilling(t *testing.T) {
	resetQuotaState(t)
	reset := time.Now().Add(48 * time.Hour).Format(time.RFC3339)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/billing/subscription":
			_, _ = w.Write([]byte(`{"plan_name":"EDU","vibe_payg_enabled":false}`))
		case "/v1/billing/budget":
			_, _ = w.Write([]byte(`{"vibe_budget":{"usage_percentage":100.0,"initial_budget":150.0,"currency":"USD","reset_at":"` + reset + `"},"api_budget":{"usage_percentage":2.0,"initial_budget":15.0,"currency":"USD","reset_at":"` + reset + `"}}`))
		case "/api/vibe/whoami":
			_, _ = w.Write([]byte(testWhoamiCHAT))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	raw := "base-url: \"" + server.URL + "/v1\"\nconsole-url: \"" + server.URL + "\"\n" +
		"quota-monitoring: true\nquota-refresh-seconds: 60\n" +
		"credentials:\n    - id: \"s1\"\n      label: \"s1\"\n      api-key: \"mstrl_live\"\n      disabled: true\n      priority: 0\n" +
		"models:\n    - id: \"m\"\n      upstream-id: \"m1\"\n      reasoning: \"\"\n"
	storeSettings(parseSettings([]byte(raw)))
	t.Cleanup(func() { storeSettings(defaultSettings()) })
	status, body := handleAdmin([]byte(`{"action":"quota"}`), "")
	if status != 200 {
		t.Fatalf("quota action status = %d body = %s", status, body)
	}
	var state map[string]any
	if err := json.Unmarshal(body, &state); err != nil {
		t.Fatalf("decode: %v body=%s", err, body)
	}
	credentials, _ := state["credentials"].([]any)
	if len(credentials) != 1 {
		t.Fatalf("credentials = %#v", state["credentials"])
	}
	credential, _ := credentials[0].(map[string]any)
	quota, _ := credential["quota"].(map[string]any)
	if quota == nil || quota["plan"] != "EDU" || quota["applicable"] != "api" || quota["auto_disabled"] != false {
		t.Fatalf("quota view = %#v", credential["quota"])
	}
	if state["active"] != "" {
		// The only credential is manual-disabled, so nothing is active.
		if quota["auto_disabled"] == true {
			return
		}
		t.Fatalf("active = %#v", state["active"])
	}
}

func TestExecuteQuotaExhaustedEnvelope(t *testing.T) {
	resetQuotaState(t)
	reset := time.Now().Add(48 * time.Hour)
	storeSettings(settings{
		BaseURL:             "http://127.0.0.1:1/v1",
		QuotaMonitoring:     true,
		QuotaRefreshSeconds: 60,
		Credentials:         []credential{{ID: "key", Label: "key", APIKey: "k", Priority: 10}},
		Models:              []modelAlias{{Client: "m", Upstream: "mistral-large-latest"}},
	})
	t.Cleanup(func() { storeSettings(defaultSettings()) })
	setQuotaSnapshot("key", quotaSnapshot{
		PlanName: "EDU", PlanType: "CHAT", Applicable: "vibe", FetchedAt: time.Now(),
		Vibe: budgetPointer(100, 150, reset, false),
	})
	raw, _ := json.Marshal(map[string]any{
		"Model": "m", "SourceFormat": "openai",
		"OriginalRequest": []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`),
	})
	body := string(execute(raw, false))
	if !strings.Contains(body, "quota_exhausted") || !strings.Contains(body, "EDU") {
		t.Fatalf("execute envelope = %s", body)
	}
}

func TestWhoamiCachedBetweenRefreshes(t *testing.T) {
	resetQuotaState(t)
	var whoamiCalls, budgetCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/billing/subscription":
			_, _ = w.Write([]byte(`{"plan_name":"INDIVIDUAL"}`))
		case "/v1/billing/budget":
			budgetCalls.Add(1)
			_, _ = w.Write([]byte(`{"vibe_budget":{"usage_percentage":1.0,"initial_budget":255.0,"currency":"EUR","reset_at":"2026-12-01T00:00:00Z"},"api_budget":{"usage_percentage":0.0,"initial_budget":25.5,"currency":"EUR","reset_at":"2026-12-01T00:00:00Z"}}`))
		case "/api/vibe/whoami":
			whoamiCalls.Add(1)
			_, _ = w.Write([]byte(testWhoamiCHAT))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	current := settings{BaseURL: server.URL + "/v1", ConsoleURL: server.URL, QuotaMonitoring: true}
	cred := credential{ID: "key", APIKey: "test-key"}
	refreshQuotaCredential(current, cred, "")
	refreshQuotaCredential(current, cred, "")
	if got := whoamiCalls.Load(); got != 1 {
		t.Fatalf("whoami calls = %d, want 1 (cached for %s)", got, whoamiCacheTTL)
	}
	if got := budgetCalls.Load(); got != 2 {
		t.Fatalf("budget calls = %d, want 2", got)
	}
	snapshot, ok := quotaSnapshotFor("key")
	if !ok || snapshot.PlanType != "CHAT" || snapshot.Applicable != "api" {
		t.Fatalf("scope was not retained on the cached cycle: %#v", snapshot)
	}
}

func TestWhoamiFailureKeepsKnownScope(t *testing.T) {
	resetQuotaState(t)
	reset := time.Now().Add(48 * time.Hour)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/vibe/whoami" {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(`{"vibe_budget":{"usage_percentage":100.0,"initial_budget":150.0,"currency":"USD","reset_at":"` + reset.Format(time.RFC3339) + `"},"api_budget":{"usage_percentage":100.0,"initial_budget":15.0,"currency":"USD","reset_at":"` + reset.Format(time.RFC3339) + `"}}`))
	}))
	defer server.Close()
	current := settings{BaseURL: server.URL + "/v1", ConsoleURL: server.URL, QuotaMonitoring: true}
	cred := credential{ID: "key", APIKey: "test-key"}
	setQuotaSnapshot("key", quotaSnapshot{
		PlanType: "CHAT", Applicable: "api",
		WhoamiFetchedAt: time.Now().Add(-2 * whoamiCacheTTL), FetchedAt: time.Now().Add(-2 * whoamiCacheTTL),
		API: budgetPointer(100, 15, reset, false), Vibe: budgetPointer(100, 150, reset, false),
	})
	refreshQuotaCredential(current, cred, "")
	snapshot, ok := quotaSnapshotFor("key")
	if !ok || snapshot.PlanType != "CHAT" || snapshot.Applicable != "api" {
		t.Fatalf("a failed scope refresh erased the known scope: %#v", snapshot)
	}
	if !snapshot.autoDisabled() || !quotaExhaustedFor(current, "key") {
		t.Fatalf("a known exhausted scope must survive the whoami outage")
	}
}
