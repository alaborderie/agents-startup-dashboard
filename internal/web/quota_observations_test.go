package web

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// observationNow is before every synthetic validity deadline below.
var observationNow = time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

func TestQuotaObservationsFixture(t *testing.T) {
	data, err := os.ReadFile("../../testdata/ai-quota/quota-observations.json")
	if err != nil {
		t.Fatal(err)
	}
	view, err := decodeQuota(data, time.Date(2026, 10, 18, 12, 5, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	want := []QuotaProvider{
		{Provider: "cursor", DisplayName: "Cursor", Freshness: "missing", StatusState: "unknown",
			StatusMessage: "No supported personal quota collector", Windows: []QuotaWindow{}, Readings: []QuotaReading{}},
		{Provider: "xai_api", DisplayName: "xAI API", Freshness: "fresh", StatusState: "ok", Windows: []QuotaWindow{},
			Readings: []QuotaReading{{ID: "prepaid-ledger-total", Label: "Ledger balance", Value: "−$10.00", ExpiresAt: "2026-10-18T12:30:00Z"}}},
		{Provider: "github_copilot", DisplayName: "GitHub Copilot", Freshness: "fresh", StatusState: "ok", Windows: []QuotaWindow{},
			Readings: []QuotaReading{{ID: "monthly-gross-ai-credit", Label: "Gross reported · Month", Value: "12.5 AI credits", ExpiresAt: "2026-10-18T12:30:00Z"}}},
	}
	if !reflect.DeepEqual(view.Providers, want) {
		t.Fatalf("providers = %+v", view.Providers)
	}
	// The server's "fresh" verdict lapses on the dashboard's clock too.
	view, err = decodeQuota(data, time.Date(2026, 10, 18, 12, 30, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	for _, provider := range view.Providers[1:] {
		if len(provider.Readings) != 0 || provider.Freshness != "expired" || provider.StatusMessage != "Last reading expired" {
			t.Fatalf("reading past valid_until shown: %+v", provider)
		}
	}
}

// observationQuota wraps accesses in an otherwise empty upstream response.
func observationQuota(schemaVersion int, accesses ...string) []byte {
	return []byte(`{"generated_at":"2026-10-19T12:00:00Z","providers":[],"observations":{"schema_version":` +
		strconv.Itoa(schemaVersion) + `,"accesses":[` + strings.Join(accesses, ",") + `]}}`)
}

// observationAccessJSON builds one access with one stream.
func observationAccessJSON(accessID, product, attempt, limits, freshness string) string {
	observation := "null"
	if limits != "" {
		observation = `{"limits":[` + limits + `]}`
	}
	return `{"access_id":"` + accessID + `","configuration_revision":1,"product_id":"` + product + `","bindings":[],
		"streams":[{"source_id":"s","latest_attempt":` + attempt + `,"last_observation":` + observation + `,
		"freshness":[` + freshness + `]}]}`
}

const okAttempt = `{"status":"ok","reason":null,"observation_id":"20000000-0000-4000-8000-000000000001"}`
const timeoutAttempt = `{"status":"error","reason":"timeout","observation_id":null}`

// limitJSON is a known, measured limit valid until 2026-10-21, with fields overridden.
func limitJSON(fields map[string]any) string {
	limit := map[string]any{
		"limit_id": "weekly", "metric_kind": "allowance", "window": map[string]any{"kind": "weekly"},
		"unit": "percent", "used": nil, "limit": nil, "remaining": nil, "exhausted": nil,
		"reset_at": "2026-10-22T00:00:00Z", "valid_until": "2026-10-21T00:00:00Z",
		"status": "known", "reason": nil, "quality": "measured",
	}
	for key, value := range fields {
		limit[key] = value
	}
	encoded, _ := json.Marshal(limit)
	return string(encoded)
}

func freshness(id, state string) string {
	return `{"limit_id":"` + id + `","state":"` + state + `"}`
}

func TestQuotaObservationsShowOnlyFreshValidReadings(t *testing.T) {
	const expires = "2026-10-21T00:00:00Z"
	meter := func(used float64, severity string, limited bool) []QuotaWindow {
		return []QuotaWindow{{ID: "weekly", Label: "Week", UsedPercent: used, Severity: severity,
			ResetsAt: "2026-10-22T00:00:00Z", Limited: limited, ExpiresAt: expires}}
	}
	hidden := func(product, name, freshness, state, message string) QuotaProvider {
		return QuotaProvider{Provider: product, DisplayName: name, Freshness: freshness, StatusState: state,
			StatusMessage: message, Windows: []QuotaWindow{}, Readings: []QuotaReading{}}
	}
	cursor := func(attempt string, fields map[string]any, state string) string {
		return observationAccessJSON("cursor-x", "cursor", attempt, limitJSON(fields), freshness("weekly", state))
	}
	for _, tc := range []struct {
		name   string
		access string
		want   QuotaProvider
	}{
		{"cursor percent becomes a meter", cursor(okAttempt, map[string]any{"used": "72.5"}, "fresh"),
			QuotaProvider{Provider: "cursor", DisplayName: "Cursor", Freshness: "fresh", StatusState: "ok", Readings: []QuotaReading{},
				Windows: meter(72.5, "warn", false)}},
		{"provider-reported exhaustion is limited", cursor(okAttempt, map[string]any{"used": "40", "exhausted": true}, "fresh"),
			QuotaProvider{Provider: "cursor", DisplayName: "Cursor", Freshness: "fresh", StatusState: "ok", Readings: []QuotaReading{},
				Windows: meter(40, "limited", true)}},
		{"server-expired reading is hidden", cursor(okAttempt, map[string]any{"used": "12"}, "expired"),
			hidden("cursor", "Cursor", "expired", "ok", "Last reading expired")},
		{"reading without a validity deadline is hidden", cursor(okAttempt, map[string]any{"used": "12", "valid_until": nil}, "fresh"),
			hidden("cursor", "Cursor", "expired", "ok", "Last reading expired")},
		{"reading past its reset is hidden", cursor(okAttempt, map[string]any{"used": "12",
			"reset_at": "2026-10-19T11:00:00Z", "valid_until": "2026-10-19T10:00:00Z"}, "fresh"),
			hidden("cursor", "Cursor", "expired", "ok", "Last reading expired")},
		{"failed attempt keeps a still-fresh reading as the last good one", cursor(timeoutAttempt, map[string]any{"used": "95"}, "fresh"),
			QuotaProvider{Provider: "cursor", DisplayName: "Cursor", Freshness: "fresh", StatusState: "ok", Readings: []QuotaReading{},
				StatusMessage: "Last good reading; latest attempt: Timed out", Windows: meter(95, "critical", false)}},
		{"failed attempt with only an unknown limit is an error", cursor(timeoutAttempt, map[string]any{"unit": nil,
			"status": "unknown", "reason": "incomplete_report", "quality": "unknown", "valid_until": nil}, "unknown"),
			hidden("cursor", "Cursor", "missing", "error", "Incomplete report · Timed out")},
		{"unknown quality is not shown", cursor(okAttempt, map[string]any{"used": "12", "quality": "guessed"}, "fresh"),
			hidden("cursor", "Cursor", "missing", "unknown", "Unexpected response format")},
		{"negative usage is not shown", cursor(okAttempt, map[string]any{"used": "-1"}, "fresh"),
			hidden("cursor", "Cursor", "missing", "unknown", "Unexpected response format")},
		{"validity past the reset is not shown", cursor(okAttempt, map[string]any{"used": "1", "valid_until": "2026-10-23T00:00:00Z"}, "fresh"),
			hidden("cursor", "Cursor", "missing", "unknown", "Unexpected response format")},
		{"malformed attempt reads as a format error", cursor(`{"status":"later","reason":null,"observation_id":null}`,
			map[string]any{"used": "12"}, "fresh"),
			QuotaProvider{Provider: "cursor", DisplayName: "Cursor", Freshness: "fresh", StatusState: "ok", Readings: []QuotaReading{},
				StatusMessage: "Last good reading; latest attempt: Unexpected response format", Windows: meter(12, "ok", false)}},
		{"ok attempt without an observation is malformed", observationAccessJSON("cursor-x", "cursor",
			`{"status":"ok","reason":null,"observation_id":null}`, "", ""),
			hidden("cursor", "Cursor", "missing", "unknown", "Unexpected response format")},
		{"non-canonical amount is not shown", observationAccessJSON("xai", "xai_api", okAttempt,
			limitJSON(map[string]any{"limit_id": "ledger", "metric_kind": "balance", "window": map[string]any{"kind": "none"},
				"unit": "usd_cent", "remaining": "1e3", "reset_at": nil}), freshness("ledger", "fresh")),
			hidden("xai_api", "xAI API", "missing", "unknown", "Unexpected response format")},
		{"estimated balance says so", observationAccessJSON("xai", "xai_api", okAttempt,
			limitJSON(map[string]any{"limit_id": "ledger", "metric_kind": "balance", "window": map[string]any{"kind": "none"},
				"unit": "usd_cent", "remaining": "250", "reset_at": nil, "quality": "estimated"}), freshness("ledger", "fresh")),
			QuotaProvider{Provider: "xai_api", DisplayName: "xAI API", Freshness: "fresh", StatusState: "ok", Windows: []QuotaWindow{},
				Readings: []QuotaReading{{ID: "ledger", Label: "Ledger balance", Value: "$2.50 (est.)", ExpiresAt: expires}}}},
		{"premium requests keep their unit", observationAccessJSON("copilot", "github_copilot", okAttempt,
			limitJSON(map[string]any{"limit_id": "monthly", "metric_kind": "usage_report", "window": map[string]any{"kind": "monthly"},
				"unit": "premium_request", "used": "1", "reset_at": nil}), freshness("monthly", "fresh")),
			QuotaProvider{Provider: "github_copilot", DisplayName: "GitHub Copilot", Freshness: "fresh", StatusState: "ok", Windows: []QuotaWindow{},
				Readings: []QuotaReading{{ID: "monthly", Label: "Gross reported · Month", Value: "1 premium request", ExpiresAt: expires}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			view, err := decodeQuota(observationQuota(1, tc.access), observationNow)
			if err != nil {
				t.Fatal(err)
			}
			if len(view.Providers) != 1 || !reflect.DeepEqual(view.Providers[0], tc.want) {
				t.Fatalf("providers = %+v", view.Providers)
			}
		})
	}
}

func TestQuotaObservationsAreIsolatedFromLegacyCards(t *testing.T) {
	legacy, err := os.ReadFile("../../testdata/ai-quota/quota.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(legacy, &doc); err != nil {
		t.Fatal(err)
	}
	copilot := observationAccessJSON("copilot", "github_copilot", `{"status":"unknown","reason":"billing_regime_unverified","observation_id":null}`, "", "")
	for _, tc := range []struct {
		name         string
		observations string
		want         []string
	}{
		{"missing block", "", nil},
		{"not an object", `"broken"`, nil},
		{"future schema", `{"schema_version":2,"accesses":[` + copilot + `]}`, nil},
		{"malformed access is dropped alone", `{"schema_version":1,"accesses":[{"access_id":"bad","product_id":7},` + copilot + `]}`,
			[]string{"github_copilot"}},
		{"invalid access id is dropped", `{"schema_version":1,"accesses":[` +
			observationAccessJSON("bad id", "cursor", okAttempt, "", "") + `]}`, nil},
		{"legacy products and unknown products are not repeated", `{"schema_version":1,"accesses":[` +
			observationAccessJSON("claude-max", "claude", okAttempt, "", "") + `,` +
			observationAccessJSON("future", "future_product", okAttempt, "", "") + `]}`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			delete(doc, "observations")
			if tc.observations != "" {
				doc["observations"] = json.RawMessage(tc.observations)
			}
			data, _ := json.Marshal(doc)
			view, err := decodeQuota(data, observationNow)
			if err != nil {
				t.Fatal(err)
			}
			var extra []string
			for _, provider := range view.Providers[4:] {
				extra = append(extra, provider.Provider)
			}
			if len(view.Providers) < 4 || view.Providers[0].Provider != "claude" || !reflect.DeepEqual(extra, tc.want) {
				t.Fatalf("providers = %+v", view.Providers)
			}
		})
	}
}

func TestQuotaObservationsNameRepeatedProductsByAccess(t *testing.T) {
	view, err := decodeQuota(observationQuota(1,
		observationAccessJSON("copilot-work", "github_copilot", okAttempt, "", ""),
		observationAccessJSON("cursor-x", "cursor", okAttempt, "", ""),
		observationAccessJSON("copilot-home", "github_copilot", okAttempt, "", "")), observationNow)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, provider := range view.Providers {
		got = append(got, provider.DisplayName+"/"+provider.Plan)
	}
	want := []string{"Cursor/", "GitHub Copilot · copilot-work/", "GitHub Copilot · copilot-home/"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("providers = %v", got)
	}
}

func TestQuotaFallsBackToLegacyViewWhenObservationsFail(t *testing.T) {
	var paths []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.RequestURI())
		if r.URL.Query().Get("include") == "observations" {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, quotaReply)
	}))
	defer upstream.Close()
	app, _ := newHQApp(t)
	app.quotaURL = upstream.URL
	cookie := login(t, app)
	got := serve(app, authed(http.MethodPut, "/hq/api/quota/token", `{"token":"`+quotaReadToken+`"}`, cookie))
	if got.Code != 200 || !strings.Contains(got.Body.String(), `"state":"ok"`) {
		t.Fatalf("legacy fallback: %d %s", got.Code, got.Body.String())
	}
	if want := []string{"/api/v1/quota?include=observations", "/api/v1/quota"}; !reflect.DeepEqual(paths, want) {
		t.Fatalf("requests = %v", paths)
	}
}

func TestFormatNativeAmountIsExact(t *testing.T) {
	for _, tc := range []struct{ value, unit, want string }{
		{"-1000", "usd_cent", "−$10.00"},
		{"0", "usd_cent", "$0.00"},
		{"12.5", "usd_cent", "$0.125"},
		{"0.0000000001", "usd_cent", "$0.000000000001"},
		{"123456789.123456789", "usd_cent", "$1,234,567.89123456789"},
		{"1234567.5", "usd", "$1,234,567.50"},
		{"1507.683275", "github_ai_credit", "1,507.683275 AI credits"},
		{"1", "github_ai_credit", "1 AI credit"},
		{"83.25", "percent", "83.25%"},
		{"5", "widgets", "5 widgets"},
	} {
		if got, ok := formatNativeAmount(tc.value, tc.unit); !ok || got != tc.want {
			t.Errorf("formatNativeAmount(%q, %q) = %q, %v; want %q", tc.value, tc.unit, got, ok, tc.want)
		}
	}
	for _, value := range []string{"01", "-0", "1.50", "1e3", "", ".5", "1234567890123456789"} {
		if got, ok := formatNativeAmount(value, "usd"); ok {
			t.Errorf("formatNativeAmount(%q) accepted as %q", value, got)
		}
	}
}
