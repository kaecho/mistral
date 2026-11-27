package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Quota monitoring reads the same billing API the official Vibe client uses:
//
//	GET {base-url}/billing/subscription  -> plan name and pay-as-you-go flags
//	GET {base-url}/billing/budget        -> the included monthly allowances
//	GET {console-url}/api/vibe/whoami    -> the key scope
//
// The budget document separates the included API spend from the included Vibe
// Code spend, and the key scope decides which one the key consumes: Vibe-scope
// keys (plan_type CHAT or MISTRAL_CODE) bill everything to the Vibe allowance,
// Studio API-scope keys bill everything to the API allowance. Once the
// applicable allowance reaches 100% with pay-as-you-go off, the whole
// credential is routed around until reset_at; nothing is written back to the
// CPA config, so a manual disable stays manual and the quota state is purely
// derived.
const (
	defaultQuotaCachePath = "/CLIProxyAPI/plugin-data/mistral/quota.json"
	// defaultConsoleURL serves the whoami endpoint that names the key scope.
	// Billing itself lives on the API base, not here.
	defaultConsoleURL = "https://console.mistral.ai"
	// whoamiPath answers with plan_type ("CHAT", "API", "MISTRAL_CODE"), the
	// only signal of which monthly allowance a key draws from.
	whoamiPath = "/api/vibe/whoami"
	// quotaRequestTimeout bounds one billing read. Billing answers in well
	// under a second when it works; a quota refresh must never hold up a
	// request or a monitor tick for the 30s the shared direct client allows.
	quotaRequestTimeout = 8 * time.Second
	// quotaExhaustedPercent is the usage percentage at which an allowance is
	// considered spent. The API already rounds to a float, so >= 100 is the
	// only safe threshold.
	quotaExhaustedPercent = 100.0
	// whoamiCacheTTL is how long the key scope is trusted. The Vibe client
	// caches whoami for six hours; the scope only changes when the key is
	// re-created as another kind.
	whoamiCacheTTL = 6 * time.Hour
	// quotaSuspectTTL bounds how long a 402 keeps a credential out of rotation
	// when billing cannot be re-read (e.g. the billing host is unreachable).
	quotaSuspectTTL = 10 * time.Minute
)

// quotaBudget is one included monthly allowance.
type quotaBudget struct {
	UsagePercentage float64   `json:"usage_percentage"`
	InitialBudget   float64   `json:"initial_budget"`
	Currency        string    `json:"currency,omitempty"`
	ResetAt         time.Time `json:"reset_at,omitempty"`
	PaygEnabled     bool      `json:"payg_enabled,omitempty"`
}

// quotaSnapshot is the last billing read for one credential. It is also the
// on-disk shape of quota.json, so the console has numbers before the first
// refresh after a CPA restart.
type quotaSnapshot struct {
	PlanName string `json:"plan_name,omitempty"`
	PlanType string `json:"plan_type,omitempty"`
	// Applicable is "api" whenever the billing read returned an API
	// allowance. Plugin traffic is third-party API usage, and Mistral counts
	// calls made through non-Mistral tools against the API monthly allowance
	// even when the key belongs to a Vibe-scope account. Empty means the API
	// allowance is unknown; the credential is never auto-disabled on a guess.
	Applicable string `json:"applicable,omitempty"`
	// WhoamiFetchedAt is the last scope-lookup attempt (successful or not),
	// so an unreachable console is retried hourly instead of every cycle.
	WhoamiFetchedAt time.Time    `json:"whoami_fetched_at,omitempty"`
	Enterprise      bool         `json:"enterprise,omitempty"`
	Currency        string       `json:"currency,omitempty"`
	API             *quotaBudget `json:"api,omitempty"`
	Vibe            *quotaBudget `json:"vibe,omitempty"`
	FetchedAt       time.Time    `json:"fetched_at"`
	Error           string       `json:"error,omitempty"`
}

var (
	quotaMu          sync.Mutex
	quotaSnapshots   = map[string]quotaSnapshot{}
	quotaCacheLoaded bool
	quotaCachePath   = defaultQuotaCachePath
	quotaMonitorOnce sync.Once
	quotaWake        = make(chan struct{}, 1)
	// quotaSuspects holds credentials that just answered 402. They stay out of
	// rotation until billing is re-read successfully after the mark, or the
	// TTL expires, so a slow billing host cannot stall the request that saw
	// the 402.
	quotaSuspects = map[string]time.Time{}
)

// budgetJSON mirrors the billing payload. Pointers distinguish "absent" from
// "zero", and the top-level budget fields are decoded as a fallback for
// platforms that only return the aggregate allowance.
type budgetJSON struct {
	UsagePercentage *float64        `json:"usage_percentage"`
	InitialBudget   *float64        `json:"initial_budget"`
	Currency        string          `json:"currency"`
	ResetAt         json.RawMessage `json:"reset_at"`
	PaygEnabled     *bool           `json:"payg_enabled"`
}

type budgetEnvelopeJSON struct {
	PlanName        string          `json:"plan_name"`
	PaygEnabled     *bool           `json:"payg_enabled"`
	VibePaygEnabled *bool           `json:"vibe_payg_enabled"`
	Enterprise      bool            `json:"enterprise"`
	VibeBudget      *budgetJSON     `json:"vibe_budget"`
	APIBudget       *budgetJSON     `json:"api_budget"`
	UsagePercentage *float64        `json:"usage_percentage"`
	InitialBudget   *float64        `json:"initial_budget"`
	Currency        string          `json:"currency"`
	ResetAt         json.RawMessage `json:"reset_at"`
}

type whoamiJSON struct {
	PlanType string `json:"plan_type"`
	PlanName string `json:"plan_name"`
}

type subscriptionJSON struct {
	PlanName        string `json:"plan_name"`
	PaygEnabled     *bool  `json:"payg_enabled"`
	VibePaygEnabled *bool  `json:"vibe_payg_enabled"`
	Enterprise      bool   `json:"enterprise"`
}

// resolveApplicable picks the allowance this plugin's traffic is billed
// against. Every request this plugin forwards is third-party API usage, and
// Mistral counts that against the API monthly allowance: a Vibe-scope key still
// spends API budget when it is called by a non-Mistral tool. The Vibe allowance
// only funds the official Vibe surfaces (web, CLI, editor extensions), so it
// must never pause a key for plugin traffic.
//
// Without an API allowance the destination is unknown: leave Applicable empty
// rather than falling back to Vibe, because disabling on the Vibe number is
// exactly the mistake this separation avoids.
func (snapshot *quotaSnapshot) resolveApplicable() {
	if snapshot == nil {
		return
	}
	if snapshot.API != nil {
		snapshot.Applicable = "api"
		return
	}
	snapshot.Applicable = ""
}

func (snapshot quotaSnapshot) applicableBudget() *quotaBudget {
	switch snapshot.Applicable {
	case "api":
		return snapshot.API
	case "vibe":
		return snapshot.Vibe
	}
	return nil
}

// autoDisabled reports whether the API allowance is spent. The credential is
// then taken out of rotation as a whole: a key cannot move plugin traffic to
// its Vibe allowance.
func (snapshot quotaSnapshot) autoDisabled() bool {
	return snapshot.applicableBudget().quotaExhausted()
}

func (snapshot quotaSnapshot) applicableReset() time.Time {
	if budget := snapshot.applicableBudget(); budget != nil {
		return budget.ResetAt
	}
	return time.Time{}
}

// quotaExhausted reports whether this allowance cannot fund more usage until
// reset_at. Pay-as-you-go means usage past 100% is billed instead of paused, so
// it never disables the credential.
func (budget *quotaBudget) quotaExhausted() bool {
	if budget == nil || budget.PaygEnabled {
		return false
	}
	if budget.UsagePercentage < quotaExhaustedPercent {
		return false
	}
	if !budget.ResetAt.IsZero() && !time.Now().Before(budget.ResetAt) {
		// The cycle has rolled over. The numbers on disk are stale by
		// definition; treat the allowance as renewed until the next read
		// proves otherwise. A request that still fails will refresh it.
		return false
	}
	return true
}

func (snapshot quotaSnapshot) budgetFor(vibe bool) *quotaBudget {
	if vibe {
		return snapshot.Vibe
	}
	return snapshot.API
}

func (snapshot quotaSnapshot) exhaustedFor(vibe bool) bool {
	return snapshot.budgetFor(vibe).quotaExhausted()
}

func (snapshot quotaSnapshot) resetFor(vibe bool) time.Time {
	if budget := snapshot.budgetFor(vibe); budget != nil {
		return budget.ResetAt
	}
	return time.Time{}
}

func (snapshot quotaSnapshot) haveBudgets() bool {
	return snapshot.API != nil || snapshot.Vibe != nil
}

// parseQuotaSnapshot merges the subscription and budget documents into one
// snapshot. Both inputs are optional: a missing or malformed document leaves
// that half empty instead of failing the whole read.
func parseQuotaSnapshot(subscriptionBody, budgetBody, whoamiBody []byte, fetchedAt time.Time) quotaSnapshot {
	snapshot := quotaSnapshot{FetchedAt: fetchedAt}
	var subscription subscriptionJSON
	if len(subscriptionBody) > 0 && json.Unmarshal(subscriptionBody, &subscription) == nil {
		snapshot.PlanName = strings.TrimSpace(subscription.PlanName)
		snapshot.Enterprise = subscription.Enterprise
	}
	var whoami whoamiJSON
	if len(whoamiBody) > 0 && json.Unmarshal(whoamiBody, &whoami) == nil {
		snapshot.PlanType = strings.TrimSpace(whoami.PlanType)
		if snapshot.PlanName == "" {
			snapshot.PlanName = strings.TrimSpace(whoami.PlanName)
		}
	}
	var envelope budgetEnvelopeJSON
	if len(budgetBody) == 0 || json.Unmarshal(budgetBody, &envelope) != nil {
		return snapshot
	}
	snapshot.Currency = envelope.Currency
	payg := false
	// Per-allowance payg flags win; the subscription-level flags are the
	// account-wide fallback when the budget document omits them.
	if subscription.PaygEnabled != nil {
		payg = *subscription.PaygEnabled
	} else if envelope.PaygEnabled != nil {
		payg = *envelope.PaygEnabled
	}
	if envelope.APIBudget != nil {
		snapshot.API = makeBudget(envelope.APIBudget, envelope.Currency, payg)
	} else if envelope.UsagePercentage != nil || envelope.InitialBudget != nil {
		// Older/platform payloads only carry the aggregate (API) budget.
		snapshot.API = makeBudget(&budgetJSON{
			UsagePercentage: envelope.UsagePercentage,
			InitialBudget:   envelope.InitialBudget,
			Currency:        envelope.Currency,
			ResetAt:         envelope.ResetAt,
			PaygEnabled:     envelope.PaygEnabled,
		}, envelope.Currency, payg)
	}
	vibePayg := payg
	if subscription.VibePaygEnabled != nil {
		vibePayg = *subscription.VibePaygEnabled
	}
	if envelope.VibeBudget != nil {
		snapshot.Vibe = makeBudget(envelope.VibeBudget, envelope.Currency, vibePayg)
	}
	snapshot.resolveApplicable()
	return snapshot
}

func makeBudget(raw *budgetJSON, fallbackCurrency string, fallbackPayg bool) *quotaBudget {
	if raw == nil {
		return nil
	}
	budget := &quotaBudget{Currency: raw.Currency, PaygEnabled: fallbackPayg}
	if budget.Currency == "" {
		budget.Currency = fallbackCurrency
	}
	if raw.UsagePercentage != nil {
		budget.UsagePercentage = *raw.UsagePercentage
	}
	if raw.InitialBudget != nil {
		budget.InitialBudget = *raw.InitialBudget
	}
	if raw.PaygEnabled != nil {
		budget.PaygEnabled = *raw.PaygEnabled
	}
	budget.ResetAt = parseResetRaw(raw.ResetAt)
	return budget
}

// parseResetTime accepts the RFC3339 instant the API returns and is tolerant
// of a bare date or a unix timestamp, so a field change cannot turn a future
// reset into a permanent disable.
func parseResetTime(value string) time.Time {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05", "2006-01-02"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed
		}
	}
	if unix, err := json.Number(value).Float64(); err == nil && unix > 0 {
		return time.Unix(int64(unix), 0)
	}
	return time.Time{}
}

// parseResetRaw accepts an RFC3339 string or a unix timestamp. A numeric
// reset_at in a whole document used to fail the JSON decode and hide every
// allowance; keeping the field raw keeps the rest usable.
func parseResetRaw(raw json.RawMessage) time.Time {
	if len(raw) == 0 {
		return time.Time{}
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return parseResetTime(text)
	}
	var number float64
	if json.Unmarshal(raw, &number) == nil && number > 0 {
		return time.Unix(int64(number), 0)
	}
	return time.Time{}
}

// fetchQuotaSnapshot performs the billing reads for one credential, plus the
// whoami read that names the key scope. It never keeps a partial plan: the
// snapshot records whether the read worked and an error message for the
// console, and selection treats an errored snapshot as "no quota data" (never
// as exhausted).
func fetchQuotaSnapshot(current settings, cred credential, callbackID string, previous quotaSnapshot) quotaSnapshot {
	started := time.Now()
	base := strings.TrimRight(current.BaseURL, "/")
	var subscriptionBody, budgetBody, whoamiBody []byte
	var subscriptionErr, budgetErr error
	// Sequential is fine: the calls share one credential and the whole refresh
	// happens off the request path.
	status, body, err := quotaHTTPGet(current, cred, callbackID, base+"/billing/subscription")
	switch {
	case err != nil:
		subscriptionErr = err
	case status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusNotFound:
		// No permission to read billing (member token) or the platform does
		// not serve it. Not an error worth reddening the console.
		subscriptionErr = fmt.Errorf("billing subscription unavailable (HTTP %d)", status)
	case !upstreamStatusOK(status):
		subscriptionErr = fmt.Errorf("billing subscription HTTP %d: %s", status, truncateText(string(body), 200))
	default:
		subscriptionBody = body
	}
	status, body, err = quotaHTTPGet(current, cred, callbackID, base+"/billing/budget")
	switch {
	case err != nil:
		budgetErr = err
	case status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusNotFound:
		budgetErr = fmt.Errorf("billing budget unavailable (HTTP %d)", status)
	case !upstreamStatusOK(status):
		budgetErr = fmt.Errorf("billing budget HTTP %d: %s", status, truncateText(string(body), 200))
	default:
		budgetBody = body
	}
	// The scope read lives on the console origin and is optional. It is cached
	// for hours like the official client does, and a failed refresh keeps the
	// previously known scope: a console outage must not silently un-pause a
	// key whose allowances are spent.
	console := strings.TrimRight(strings.TrimSpace(current.ConsoleURL), "/")
	whoamiAttempted := time.Now()
	needWhoami := previous.WhoamiFetchedAt.IsZero() || time.Since(previous.WhoamiFetchedAt) >= whoamiCacheTTL
	if console != "" && needWhoami {
		if status, body, err = quotaHTTPGet(current, cred, callbackID, console+whoamiPath); err == nil && upstreamStatusOK(status) {
			whoamiBody = body
		}
	}
	snapshot := parseQuotaSnapshot(subscriptionBody, budgetBody, whoamiBody, started)
	if snapshot.PlanType == "" {
		snapshot.PlanType = previous.PlanType
	}
	if needWhoami {
		snapshot.WhoamiFetchedAt = whoamiAttempted
	} else {
		snapshot.WhoamiFetchedAt = previous.WhoamiFetchedAt
	}
	snapshot.resolveApplicable()
	// The allowance read is what selection needs; the plan read is cosmetic.
	// A failed subscription call must not mask a good budget response, and a
	// status other than 2xx must never leave stale-looking numbers behind.
	switch {
	case snapshot.haveBudgets():
	case budgetErr != nil:
		snapshot.Error = budgetErr.Error()
	case subscriptionErr != nil:
		snapshot.Error = subscriptionErr.Error()
	default:
		snapshot.Error = "billing returned no allowance data"
	}
	return snapshot
}

func quotaHTTPGet(current settings, cred credential, callbackID, url string) (int, []byte, error) {
	headers := map[string][]string{
		"Authorization": {"Bearer " + cred.APIKey},
		"Accept":        {"application/json"},
		"User-Agent":    {"kaecho-mistral/" + pluginVersion},
	}
	if callbackID != "" {
		return httpDo(callbackID, current.IgnoreProxy, http.MethodGet, url, headers, nil)
	}
	// Background ticks have no host callback, so make the call directly. The
	// shared direct client uses a 30s timeout, which is too long for a
	// monitor; a local client keeps the read bounded.
	request, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return 0, nil, err
	}
	for name, values := range headers {
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}
	client := &http.Client{Timeout: quotaRequestTimeout}
	if current.IgnoreProxy {
		client.Transport = directTransport()
	}
	response, err := client.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if readErr != nil {
		return response.StatusCode, nil, readErr
	}
	return response.StatusCode, body, nil
}

// quotaExhaustedFor reports whether a credential has spent the allowance its
// key scope bills against. Unknown scopes, unknown credentials and errored
// snapshots are never treated as exhausted, so a billing outage cannot take
// the provider down.
func quotaExhaustedFor(current settings, credentialID string) bool {
	if !current.QuotaMonitoring {
		return false
	}
	if quotaSuspectBlocked(credentialID) {
		return true
	}
	snapshot, ok := quotaSnapshotFor(credentialID)
	if !ok || snapshot.Error != "" || !snapshot.haveBudgets() {
		return false
	}
	return snapshot.autoDisabled()
}

// markQuotaSuspect takes a credential out of rotation until billing is read
// again, after the upstream answered 402. It does no network I/O itself.
func markQuotaSuspect(credentialID string) {
	if strings.TrimSpace(credentialID) == "" {
		return
	}
	quotaMu.Lock()
	defer quotaMu.Unlock()
	loadQuotaCacheLocked()
	quotaSuspects[credentialID] = time.Now()
}

// quotaSuspectBlocked reports whether a 402 mark still applies. A successful
// billing read after the mark clears it, because the snapshot then reflects
// the post-402 state and autoDisabled() decides on real numbers.
func quotaSuspectBlocked(credentialID string) bool {
	if strings.TrimSpace(credentialID) == "" {
		return false
	}
	quotaMu.Lock()
	defer quotaMu.Unlock()
	loadQuotaCacheLocked()
	markedAt, ok := quotaSuspects[credentialID]
	if !ok {
		return false
	}
	if time.Since(markedAt) > quotaSuspectTTL {
		delete(quotaSuspects, credentialID)
		return false
	}
	if snapshot, found := quotaSnapshots[credentialID]; found && snapshot.Error == "" && !snapshot.FetchedAt.Before(markedAt) {
		delete(quotaSuspects, credentialID)
		return false
	}
	return true
}

// quotaSnapshotFor returns the cached snapshot for one credential, loading the
// on-disk cache once per process.
func quotaSnapshotFor(credentialID string) (quotaSnapshot, bool) {
	quotaMu.Lock()
	defer quotaMu.Unlock()
	loadQuotaCacheLocked()
	snapshot, ok := quotaSnapshots[credentialID]
	return snapshot, ok
}

func setQuotaSnapshot(credentialID string, snapshot quotaSnapshot) {
	quotaMu.Lock()
	defer quotaMu.Unlock()
	loadQuotaCacheLocked()
	quotaSnapshots[credentialID] = snapshot
	if snapshot.Error == "" {
		// Only a read that started after the 402 can clear the mark; one that
		// was already in flight carries the pre-402 picture.
		if markedAt, marked := quotaSuspects[credentialID]; !marked || !snapshot.FetchedAt.Before(markedAt) {
			delete(quotaSuspects, credentialID)
		}
	}
	saveQuotaCacheLocked()
}

func dropQuotaSnapshot(credentialID string) {
	quotaMu.Lock()
	defer quotaMu.Unlock()
	loadQuotaCacheLocked()
	delete(quotaSnapshots, credentialID)
	delete(quotaSuspects, credentialID)
	saveQuotaCacheLocked()
}

func loadQuotaCacheLocked() {
	if quotaCacheLoaded {
		return
	}
	quotaCacheLoaded = true
	raw, err := os.ReadFile(quotaCachePath)
	if err != nil {
		return
	}
	var document struct {
		Snapshots map[string]quotaSnapshot `json:"snapshots"`
	}
	if json.Unmarshal(raw, &document) != nil || len(document.Snapshots) == 0 {
		return
	}
	quotaSnapshots = document.Snapshots
}

func saveQuotaCacheLocked() {
	if quotaCachePath == "" {
		return
	}
	raw, err := json.Marshal(map[string]any{"snapshots": quotaSnapshots, "saved_at": time.Now().UTC()})
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(quotaCachePath), 0o755); err != nil {
		return
	}
	temporary := quotaCachePath + ".tmp"
	if err := os.WriteFile(temporary, raw, 0o600); err != nil {
		return
	}
	_ = os.Rename(temporary, quotaCachePath)
}

// refreshQuotaCredential reads billing for one credential and stores the
// result. It returns the fresh snapshot even when the read failed.
func refreshQuotaCredential(current settings, cred credential, callbackID string) quotaSnapshot {
	if strings.TrimSpace(cred.APIKey) == "" {
		return quotaSnapshot{Error: "credential has no API key", FetchedAt: time.Now()}
	}
	previous, _ := quotaSnapshotFor(cred.ID)
	snapshot := fetchQuotaSnapshot(current, cred, callbackID, previous)
	setQuotaSnapshot(cred.ID, snapshot)
	return snapshot
}

// refreshQuotas reads billing for every credential (or one, when id is set)
// and returns the number of reads attempted. Entries are refreshed in
// sequence: credentials are few and the calls are cheap.
func refreshQuotas(current settings, callbackID, id string) int {
	reads := 0
	for _, cred := range current.Credentials {
		if id != "" && cred.ID != id {
			continue
		}
		if strings.TrimSpace(cred.APIKey) == "" {
			continue
		}
		refreshQuotaCredential(current, cred, callbackID)
		reads++
	}
	return reads
}

// startQuotaMonitor starts the background refresh loop. It is idempotent and
// is called once the plugin has a credential configuration (plugin.register).
// The loop is what re-enables a quota-disabled credential at reset time
// without waiting for traffic or for the console to be open.
func startQuotaMonitor() {
	quotaMonitorOnce.Do(func() {
		go quotaMonitorLoop()
	})
}

func quotaMonitorLoop() {
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-timer.C:
		case <-quotaWake:
		}
		refreshQuotasForMonitor()
		interval := time.Duration(clampInt(loadSettings().QuotaRefreshSeconds, 10, 3600)) * time.Second
		timer.Reset(interval)
	}
}

func refreshQuotasForMonitor() {
	current := loadSettings()
	if !current.QuotaMonitoring {
		return
	}
	interval := time.Duration(clampInt(current.QuotaRefreshSeconds, 10, 3600)) * time.Second
	for _, cred := range current.Credentials {
		if strings.TrimSpace(cred.APIKey) == "" {
			continue
		}
		// Manually disabled credentials still get a read: the console shows
		// the plan and allowances for every key, and a disabled key whose
		// allowance ran out is exactly what the user wants to see.
		if snapshot, ok := quotaSnapshotFor(cred.ID); ok && snapshot.Error == "" && time.Since(snapshot.FetchedAt) < interval && !quotaSuspectBlocked(cred.ID) {
			continue
		}
		refreshQuotaCredential(current, cred, "")
	}
}

// wakeQuotaMonitor asks the monitor for an immediate refresh, e.g. after a new
// credential is saved or quota monitoring is turned back on.
func wakeQuotaMonitor() {
	select {
	case quotaWake <- struct{}{}:
	default:
	}
}

// quotaView renders one snapshot for the management API. Numbers stay in their
// billing units (percentage, included amount, currency); the console formats
// them so it can follow the page language.
func quotaView(credentialID string, monitoring bool) map[string]any {
	snapshot, ok := quotaSnapshotFor(credentialID)
	if !ok {
		return nil
	}
	view := map[string]any{
		"plan":          snapshot.PlanName,
		"scope":         snapshot.PlanType,
		"applicable":    snapshot.Applicable,
		"enterprise":    snapshot.Enterprise,
		"currency":      snapshot.Currency,
		"monitoring":    monitoring,
		"fetched_at":    formatTime(snapshot.FetchedAt),
		"age_seconds":   int(time.Since(snapshot.FetchedAt).Seconds()),
		"api":           quotaBudgetView(snapshot.API),
		"vibe":          quotaBudgetView(snapshot.Vibe),
		"auto_disabled": monitoring && snapshot.autoDisabled(),
		"reset_at":      formatTime(snapshot.applicableReset()),
	}
	if view["age_seconds"].(int) < 0 {
		view["age_seconds"] = 0
	}
	if snapshot.Error != "" {
		view["error"] = snapshot.Error
	}
	return view
}

func quotaBudgetView(budget *quotaBudget) map[string]any {
	if budget == nil {
		return nil
	}
	used := budget.UsagePercentage / 100 * budget.InitialBudget
	view := map[string]any{
		"usage_percentage": budget.UsagePercentage,
		"initial_budget":   budget.InitialBudget,
		"currency":         budget.Currency,
		"used":             used,
		"remaining":        budget.InitialBudget - used,
		"payg_enabled":     budget.PaygEnabled,
		"reset_at":         formatTime(budget.ResetAt),
		"exhausted":        budget.quotaExhausted(),
	}
	if view["remaining"].(float64) < 0 {
		view["remaining"] = 0.0
	}
	return view
}

// quotaExhaustedEnvelope explains an empty candidate set that was caused by
// exhausted allowances, including when the allowance comes back.
func quotaExhaustedEnvelope(current settings) []byte {
	var reset time.Time
	var plan string
	for _, cred := range current.Credentials {
		snapshot, ok := quotaSnapshotFor(cred.ID)
		if !ok || !snapshot.autoDisabled() {
			continue
		}
		if reset.IsZero() || snapshot.applicableReset().Before(reset) {
			reset = snapshot.applicableReset()
		}
		if plan == "" {
			plan = snapshot.PlanName
		}
	}
	message := "all Mistral credentials have spent their included monthly allowance and are paused"
	if plan != "" {
		message += " (" + plan + ")"
	}
	if !reset.IsZero() {
		message += "; they resume automatically after " + reset.Local().Format("2006-01-02 15:04")
	}
	return errorEnvelope("quota_exhausted", message, http.StatusTooManyRequests)
}
