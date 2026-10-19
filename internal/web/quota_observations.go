package web

import (
	"encoding/json"
	"math/big"
	"regexp"
	"strconv"
	"strings"
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

var windowLabels = map[string]string{"session": "Session", "weekly": "Week", "monthly": "Month", "other": "Period"}

// Canonical decimal string of the observations contract.
var observationDecimal = regexp.MustCompile(`^-?(0|[1-9][0-9]{0,17})(\.[0-9]{0,9}[1-9])?$`)

type observationLimit struct {
	LimitID    string `json:"limit_id"`
	MetricKind string `json:"metric_kind"`
	Window     struct {
		Kind string `json:"kind"`
	} `json:"window"`
	Unit      *string `json:"unit"`
	Used      *string `json:"used"`
	Remaining *string `json:"remaining"`
	Exhausted *bool   `json:"exhausted"`
	ResetAt   string  `json:"reset_at"`
	Status    string  `json:"status"`
	Reason    string  `json:"reason"`
}

type observationAccess struct {
	AccessID  string `json:"access_id"`
	ProductID string `json:"product_id"`
	Streams   []struct {
		LatestAttempt struct {
			Status string `json:"status"`
			Reason string `json:"reason"`
		} `json:"latest_attempt"`
		LastObservation *struct {
			Limits []observationLimit `json:"limits"`
		} `json:"last_observation"`
		Freshness []struct {
			LimitID string `json:"limit_id"`
			State   string `json:"state"`
		} `json:"freshness"`
	} `json:"streams"`
}

// decodeObservations reads the optional observations block one access at a
// time: a missing, malformed or unknown-version block, or a malformed access,
// leaves the legacy cards untouched and only drops what cannot be read.
func decodeObservations(raw json.RawMessage) []QuotaProvider {
	var block struct {
		SchemaVersion int               `json:"schema_version"`
		Accesses      []json.RawMessage `json:"accesses"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &block) != nil || block.SchemaVersion != 1 {
		return nil
	}
	byProduct := map[string][]observationAccess{}
	for _, item := range block.Accesses {
		var access observationAccess
		if json.Unmarshal(item, &access) != nil || access.AccessID == "" {
			continue
		}
		byProduct[access.ProductID] = append(byProduct[access.ProductID], access)
	}
	var providers []QuotaProvider
	for _, product := range observationProducts {
		accesses := byProduct[product.id]
		for _, access := range accesses {
			provider := observationProvider(product.name, access)
			if len(accesses) > 1 {
				provider.Plan = access.AccessID
			}
			providers = append(providers, provider)
		}
	}
	return providers
}

// observationProvider shows only fresh, known readings; anything else becomes
// a note, so the card never presents an expired or unknown value as current.
func observationProvider(name string, access observationAccess) QuotaProvider {
	provider := QuotaProvider{
		Provider: access.ProductID, DisplayName: name,
		Windows: []QuotaWindow{}, Readings: []QuotaReading{},
	}
	var notes []string
	note := func(text string) {
		for _, seen := range notes {
			if seen == text {
				return
			}
		}
		notes = append(notes, text)
	}
	failed, expired := false, false
	for _, stream := range access.Streams {
		if stream.LatestAttempt.Status != "ok" {
			failed = failed || stream.LatestAttempt.Status == "error"
			note(reasonText(stream.LatestAttempt.Reason))
		}
		if stream.LastObservation == nil {
			continue
		}
		fresh := map[string]bool{}
		for _, item := range stream.Freshness {
			fresh[item.LimitID] = item.State == "fresh"
		}
		for _, limit := range stream.LastObservation.Limits {
			switch {
			case limit.Status != "known":
				note(reasonText(limit.Reason))
			case !fresh[limit.LimitID]:
				expired = true
				note("Last reading expired")
			default:
				if !addObservationLimit(&provider, limit) {
					note(reasonText("schema_mismatch"))
				}
			}
		}
	}
	switch {
	case len(provider.Windows) > 0 || len(provider.Readings) > 0:
		provider.Freshness, provider.StatusState = "fresh", "ok"
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

// addObservationLimit adds a percentage allowance as a meter and any other
// amount as a native-unit reading. It reports false for an unreadable value.
func addObservationLimit(provider *QuotaProvider, limit observationLimit) bool {
	if limit.Unit == nil {
		return false
	}
	unit := *limit.Unit
	label := windowLabels[limit.Window.Kind]
	constraint := limit.MetricKind == "allowance" || limit.MetricKind == "rate_limit"
	if constraint && unit == "percent" && limit.Used != nil {
		if !observationDecimal.MatchString(*limit.Used) {
			return false
		}
		used, err := strconv.ParseFloat(*limit.Used, 64)
		if err != nil {
			return false
		}
		limited := limit.Exhausted != nil && *limit.Exhausted
		severity := "ok"
		switch {
		case limited:
			severity = "limited"
		case used >= 90:
			severity = "critical"
		case used >= 70:
			severity = "warn"
		}
		if label == "" {
			label = "Usage"
		}
		provider.Windows = append(provider.Windows, QuotaWindow{
			ID: limit.LimitID, Label: label, UsedPercent: used,
			Severity: severity, ResetsAt: limit.ResetAt, Limited: limited,
		})
		return true
	}
	var value, suffix string
	switch {
	case limit.MetricKind == "balance" && limit.Remaining != nil:
		value, label = *limit.Remaining, "Balance"
	case constraint && limit.Remaining != nil:
		value, suffix = *limit.Remaining, " left"
	case limit.Used != nil:
		value, suffix = *limit.Used, " used"
	default:
		return false
	}
	amount, ok := formatNativeAmount(value, unit)
	if !ok {
		return false
	}
	if label == "" {
		label = "Usage"
	}
	provider.Readings = append(provider.Readings, QuotaReading{ID: limit.LimitID, Label: label, Value: amount + suffix})
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
		text := strings.TrimRight(amount.FloatString(11), "0")
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
