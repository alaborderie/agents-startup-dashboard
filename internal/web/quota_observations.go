package web

import (
	"encoding/json"
	"math/big"
	"strconv"
	"strings"
	"time"
)

// observationProducts are the ai-quota products that exist only in the
// observations extension, in card order. The legacy products already have a
// card from the legacy view, so their per-access copies are not repeated.
var observationProducts = []struct{ id, name string }{
	{"cursor", "Cursor"},
	{"xai_api", "xAI API"},
	{"github_copilot", "GitHub Copilot"},
}

// observationReasons is ai-quota's closed v1 reason list, as short card text.
var observationReasons = map[string]string{
	"not_configured":                    "Not configured",
	"disabled":                          "Collector disabled",
	"unsupported_personal_usage_source": "No supported personal quota collector",
	"binding_unverified":                "Account binding unverified",
	"billing_regime_unverified":         "Billing regime unverified",
	"entitlement_unavailable":           "Entitlement unavailable",
	"permission_denied":                 "Permission denied",
	"auth_failed":                       "Authentication failed",
	"rate_limited":                      "Rate limited",
	"timeout":                           "Timed out",
	"source_unavailable":                "Source unavailable",
	"schema_mismatch":                   "Unexpected response format",
	"incomplete_report":                 "Incomplete report",
	"metric_unavailable":                "Metric unavailable",
	"coverage_unknown":                  "Coverage unknown",
	"clock_skew":                        "Clock skew",
	"retired":                           "Retired",
	"configuration_changed":             "Configuration changed; awaiting a new reading",
}

var windowLabels = map[string]string{"session": "Session", "weekly": "Week", "monthly": "Month", "other": "Period", "none": ""}

// decodeObservations turns the optional observations block into cards for
// the products the legacy view lacks. Without a readable block the legacy
// cards stand alone.
func decodeObservations(raw json.RawMessage, now time.Time) []QuotaProvider {
	byProduct := map[string][]observationAccess{}
	for _, access := range decodeObservationAccesses(raw) {
		byProduct[access.ProductID] = append(byProduct[access.ProductID], access)
	}
	var providers []QuotaProvider
	for _, product := range observationProducts {
		accesses := byProduct[product.id]
		for _, access := range accesses {
			name := product.name
			if len(accesses) > 1 {
				// The title, not the plan badge, carries identity: the badge hides on phones.
				name += " · " + access.AccessID
			}
			providers = append(providers, observationProvider(name, access, now))
		}
	}
	return providers
}

// observationProvider shows only fresh, known readings; anything else becomes
// a note, so the card never presents an expired or unknown value as current.
// The server's freshness verdict is rechecked on the dashboard clock.
func observationProvider(name string, access observationAccess, now time.Time) QuotaProvider {
	provider := QuotaProvider{
		Provider: access.ProductID, DisplayName: name,
		Windows: []QuotaWindow{}, Readings: []QuotaReading{},
	}
	var notes []string
	// expiredNotes replace notes in the browser once every shown reading has
	// lapsed, so a collection failure stays visible without "last good" wording.
	expiredNotes := []string{"Last reading expired"}
	add := func(list *[]string, text string) {
		for _, seen := range *list {
			if seen == text {
				return
			}
		}
		*list = append(*list, text)
	}
	note := func(text string) { add(&notes, text) }
	both := func(text string) { note(text); add(&expiredNotes, text) }
	failed, expired := false, false
	for _, stream := range access.Streams {
		failed = failed || stream.Attempt.Status == "error"
		if stream.Unreadable {
			both(reasonText("schema_mismatch"))
		}
		shown := 0
		if stream.Observation != nil {
			for _, limit := range stream.Observation.Limits {
				switch {
				case limit.Status != "known":
					both(reasonText(*limit.Reason))
				case stream.Freshness[limit.LimitID] != "fresh" || !current(limit, now):
					expired = true
					note("Last reading expired")
				case addObservationLimit(&provider, access.ProductID, limit):
					shown++
				default:
					both(reasonText("schema_mismatch"))
				}
			}
		}
		switch reason := reasonText(stream.Attempt.Reason); {
		case stream.Attempt.Status == "ok":
		case shown > 0:
			note("Last good reading; latest attempt: " + reason)
			add(&expiredNotes, "Latest attempt: "+reason)
		default:
			both(reason)
		}
	}
	switch {
	case len(provider.Windows) > 0 || len(provider.Readings) > 0:
		provider.Freshness, provider.StatusState = "fresh", "ok"
		provider.ExpiredMessage = strings.Join(expiredNotes, " · ")
	case failed:
		provider.Freshness, provider.StatusState = "missing", "error"
	case expired:
		provider.Freshness, provider.StatusState = "expired", "ok"
	default:
		provider.Freshness, provider.StatusState = "missing", "unknown"
	}
	if len(notes) == 0 && provider.StatusState != "ok" || len(access.Streams) == 0 {
		notes = []string{"No reading yet"}
	}
	provider.StatusMessage = strings.Join(notes, " · ")
	return provider
}

// current is the contract's freshness rule: a reading needs a validity
// deadline and lapses at it or at a reset, whichever comes first.
func current(limit observationLimit, now time.Time) bool {
	expires := limitExpiry(limit)
	return !expires.IsZero() && now.Before(expires)
}

func limitExpiry(limit observationLimit) time.Time {
	if limit.ValidUntil == nil {
		return time.Time{}
	}
	expires := parseTime(*limit.ValidUntil)
	if limit.ResetAt != nil {
		if reset := parseTime(*limit.ResetAt); reset.Before(expires) {
			expires = reset
		}
	}
	return expires
}

// addObservationLimit adds a percentage allowance as a meter and any other
// amount as a native-unit reading. It reports false for an unreadable value.
func addObservationLimit(provider *QuotaProvider, product string, limit observationLimit) bool {
	if limit.Unit == nil {
		return false
	}
	unit := *limit.Unit
	period := windowLabels[limit.Window.Kind]
	expiresAt := limitExpiry(limit).UTC().Format(time.RFC3339)
	estimated := ""
	if limit.Quality == "estimated" {
		estimated = " (est.)"
	}
	constraint := limit.MetricKind == "allowance" || limit.MetricKind == "rate_limit"
	if constraint && unit == "percent" && limit.Used != nil {
		used, err := strconv.ParseFloat(*limit.Used, 64)
		if err != nil {
			return false
		}
		limited := limit.Exhausted != nil && *limit.Exhausted
		resetsAt := ""
		if limit.ResetAt != nil {
			resetsAt = *limit.ResetAt
		}
		severity := "ok"
		switch {
		case limited:
			severity = "limited"
		case used >= 90:
			severity = "critical"
		case used >= 70:
			severity = "warn"
		}
		if period == "" {
			period = "Usage"
		}
		provider.Windows = append(provider.Windows, QuotaWindow{
			ID: limit.LimitID, Label: period + estimated, UsedPercent: used,
			Severity: severity, ResetsAt: resetsAt, Limited: limited, ExpiresAt: expiresAt,
		})
		return true
	}
	var label, value, suffix string
	switch {
	case limit.MetricKind == "balance" && limit.Remaining != nil:
		// A ledger is evidence, not a spendable quota.
		label, value = "Ledger balance", *limit.Remaining
	case limit.MetricKind == "usage_report" && limit.Used != nil:
		// A billing report says nothing about what remains.
		label, value = "Reported", *limit.Used
		if product == "github_copilot" {
			label = "Gross reported"
		}
	case constraint && limit.Remaining != nil:
		label, value, suffix = "Remaining", *limit.Remaining, " left"
	case constraint && limit.Used != nil:
		label, value, suffix = "Used", *limit.Used, " used"
	default:
		return false
	}
	amount, ok := formatNativeAmount(value, unit)
	if !ok {
		return false
	}
	if period != "" {
		label += " · " + period
	}
	provider.Readings = append(provider.Readings, QuotaReading{
		ID: limit.LimitID, Label: label, Value: amount + suffix + estimated, ExpiresAt: expiresAt,
	})
	return true
}

func reasonText(reason string) string {
	if text, ok := observationReasons[reason]; ok {
		return text
	}
	return "Unavailable"
}

// formatNativeAmount renders a canonical decimal exactly in its own unit:
// cents only move their decimal point to dollars, and nothing is rounded.
func formatNativeAmount(value, unit string) (string, bool) {
	if !observationDecimal.MatchString(value) || value == "-0" {
		return "", false
	}
	sign := ""
	if strings.HasPrefix(value, "-") {
		sign, value = "−", value[1:]
	}
	switch unit {
	case "usd", "usd_cent":
		amount, _ := new(big.Rat).SetString(value)
		if unit == "usd_cent" {
			amount.Quo(amount, big.NewRat(100, 1))
		}
		text := strings.TrimRight(amount.FloatString(12), "0")
		if dot := strings.IndexByte(text, '.'); len(text)-dot < 3 {
			text += strings.Repeat("0", 3-(len(text)-dot))
		}
		return sign + "$" + groupDigits(text), true
	case "percent":
		return sign + groupDigits(value) + "%", true
	}
	names := map[string][2]string{
		"github_ai_credit": {"AI credit", "AI credits"},
		"premium_request":  {"premium request", "premium requests"},
		"request":          {"request", "requests"},
		"token":            {"token", "tokens"},
	}
	name := unit
	if pair, ok := names[unit]; ok {
		name = pair[1]
		if value == "1" {
			name = pair[0]
		}
	}
	return sign + groupDigits(value) + " " + name, true
}

// groupDigits adds thousands separators to the integer part only.
func groupDigits(value string) string {
	whole, fraction, hasFraction := strings.Cut(value, ".")
	var grouped strings.Builder
	for i, digit := range whole {
		if i > 0 && (len(whole)-i)%3 == 0 {
			grouped.WriteByte(',')
		}
		grouped.WriteRune(digit)
	}
	if hasFraction {
		return grouped.String() + "." + fraction
	}
	return grouped.String()
}
