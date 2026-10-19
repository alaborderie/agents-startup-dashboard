package web

import (
	"encoding/json"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestQuotaObservationsFixture(t *testing.T) {
	data, err := os.ReadFile("../../testdata/ai-quota/quota-observations.json")
	if err != nil {
		t.Fatal(err)
	}
	view, err := decodeQuota(data)
	if err != nil {
		t.Fatal(err)
	}
	want := []QuotaProvider{
		{Provider: "cursor", DisplayName: "Cursor", Freshness: "missing", StatusState: "unknown",
			StatusMessage: "No supported personal quota collector", Windows: []QuotaWindow{}, Readings: []QuotaReading{}},
		{Provider: "xai_api", DisplayName: "xAI API", Freshness: "fresh", StatusState: "ok", Windows: []QuotaWindow{},
			Readings: []QuotaReading{{ID: "prepaid-ledger-total", Label: "Balance", Value: "−$10.00"}}},
		{Provider: "github_copilot", DisplayName: "GitHub Copilot", Freshness: "fresh", StatusState: "ok", Windows: []QuotaWindow{},
			Readings: []QuotaReading{{ID: "monthly-gross-ai-credit", Label: "Month", Value: "12.5 AI credits used"}}},
	}
	if !reflect.DeepEqual(view.Providers, want) {
		t.Fatalf("providers = %+v", view.Providers)
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

const okAttempt = `{"status":"ok","reason":null}`
const timeoutAttempt = `{"status":"error","reason":"timeout"}`

func percentLimit(id, used, exhausted string) string {
	return `{"limit_id":"` + id + `","metric_kind":"allowance","window":{"kind":"weekly"},"unit":"percent","used":"` + used +
		`","remaining":null,"exhausted":` + exhausted + `,"reset_at":"2026-10-22T00:00:00Z","status":"known","reason":null}`
}

func freshness(id, state string) string {
	return `{"limit_id":"` + id + `","state":"` + state + `"}`
}

func TestQuotaObservationsShowOnlyFreshReadings(t *testing.T) {
	for _, tc := range []struct {
		name   string
		access string
		want   QuotaProvider
	}{
		{"cursor percent becomes a meter", observationAccessJSON("cursor-x", "cursor", okAttempt,
			percentLimit("weekly", "72.5", "null"), freshness("weekly", "fresh")),
			QuotaProvider{Provider: "cursor", DisplayName: "Cursor", Freshness: "fresh", StatusState: "ok", Readings: []QuotaReading{},
				Windows: []QuotaWindow{{ID: "weekly", Label: "Week", UsedPercent: 72.5, Severity: "warn", ResetsAt: "2026-10-22T00:00:00Z"}}}},
		{"provider-reported exhaustion is limited", observationAccessJSON("cursor-x", "cursor", okAttempt,
			percentLimit("weekly", "40", "true"), freshness("weekly", "fresh")),
			QuotaProvider{Provider: "cursor", DisplayName: "Cursor", Freshness: "fresh", StatusState: "ok", Readings: []QuotaReading{},
				Windows: []QuotaWindow{{ID: "weekly", Label: "Week", UsedPercent: 40, Severity: "limited", ResetsAt: "2026-10-22T00:00:00Z", Limited: true}}}},
		{"expired reading is hidden", observationAccessJSON("cursor-x", "cursor", okAttempt,
			percentLimit("weekly", "12", "null"), freshness("weekly", "expired")),
			QuotaProvider{Provider: "cursor", DisplayName: "Cursor", Freshness: "expired", StatusState: "ok",
				StatusMessage: "Last reading expired", Windows: []QuotaWindow{}, Readings: []QuotaReading{}}},
		{"failed attempt keeps a still-fresh reading with a note", observationAccessJSON("cursor-x", "cursor", timeoutAttempt,
			percentLimit("weekly", "95", "null"), freshness("weekly", "fresh")),
			QuotaProvider{Provider: "cursor", DisplayName: "Cursor", Freshness: "fresh", StatusState: "ok", StatusMessage: "Timed out", Readings: []QuotaReading{},
				Windows: []QuotaWindow{{ID: "weekly", Label: "Week", UsedPercent: 95, Severity: "critical", ResetsAt: "2026-10-22T00:00:00Z"}}}},
		{"failed attempt with only an unknown limit is an error", observationAccessJSON("cursor-x", "cursor", timeoutAttempt,
			`{"limit_id":"weekly","metric_kind":"allowance","window":{"kind":"weekly"},"unit":null,"used":null,"remaining":null,
				"exhausted":null,"reset_at":null,"status":"unknown","reason":"incomplete_report"}`, freshness("weekly", "unknown")),
			QuotaProvider{Provider: "cursor", DisplayName: "Cursor", Freshness: "missing", StatusState: "error",
				StatusMessage: "Timed out · Incomplete report", Windows: []QuotaWindow{}, Readings: []QuotaReading{}}},
		{"non-canonical amount is not shown", observationAccessJSON("xai", "xai_api", okAttempt,
			`{"limit_id":"ledger","metric_kind":"balance","window":{"kind":"none"},"unit":"usd_cent","used":null,"remaining":"1e3",
				"exhausted":null,"reset_at":null,"status":"known","reason":null}`, freshness("ledger", "fresh")),
			QuotaProvider{Provider: "xai_api", DisplayName: "xAI API", Freshness: "missing", StatusState: "unknown",
				StatusMessage: "Unexpected response format", Windows: []QuotaWindow{}, Readings: []QuotaReading{}}},
		{"premium requests keep their unit", observationAccessJSON("copilot", "github_copilot", okAttempt,
			`{"limit_id":"monthly","metric_kind":"usage_report","window":{"kind":"monthly"},"unit":"premium_request","used":"1",
				"remaining":null,"exhausted":null,"reset_at":null,"status":"known","reason":null}`, freshness("monthly", "fresh")),
			QuotaProvider{Provider: "github_copilot", DisplayName: "GitHub Copilot", Freshness: "fresh", StatusState: "ok", Windows: []QuotaWindow{},
				Readings: []QuotaReading{{ID: "monthly", Label: "Month", Value: "1 premium request used"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			view, err := decodeQuota(observationQuota(1, tc.access))
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
	copilot := observationAccessJSON("copilot", "github_copilot", `{"status":"unknown","reason":"billing_regime_unverified"}`, "", "")
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
			view, err := decodeQuota(data)
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

func TestQuotaObservationsLabelRepeatedProductsByAccess(t *testing.T) {
	view, err := decodeQuota(observationQuota(1,
		observationAccessJSON("copilot-work", "github_copilot", okAttempt, "", ""),
		observationAccessJSON("cursor-x", "cursor", okAttempt, "", ""),
		observationAccessJSON("copilot-home", "github_copilot", okAttempt, "", "")))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, provider := range view.Providers {
		got = append(got, provider.Provider+"/"+provider.Plan+"/"+provider.StatusMessage)
	}
	want := []string{"cursor//No reading yet", "github_copilot/copilot-work/No reading yet", "github_copilot/copilot-home/No reading yet"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("providers = %v", got)
	}
}

func TestFormatNativeAmountIsExact(t *testing.T) {
	for _, tc := range []struct{ value, unit, want string }{
		{"-1000", "usd_cent", "−$10.00"},
		{"0", "usd_cent", "$0.00"},
		{"12.5", "usd_cent", "$0.125"},
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
