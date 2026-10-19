package web

import (
	"encoding/json"
	"math/big"
	"regexp"
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

// Canonical decimal string and identifier of the observations contract.
var (
	observationDecimal = regexp.MustCompile(`^-?(0|[1-9][0-9]{0,17})(\.[0-9]{0,9}[1-9])?$`)
	observationID      = regexp.MustCompile(`^[A-Za-z0-9._:/-]{1,160}$`)
)

var (
	metricKinds   = map[string]bool{"allowance": true, "rate_limit": true, "balance": true, "usage_report": true}
	limitStatuses = map[string]bool{"known": true, "unknown": true, "error": true}
	qualities     = map[string]bool{"measured": true, "derived": true, "estimated": true, "unknown": true}
)

type observationLimit struct {
	LimitID    string `json:"limit_id"`
	MetricKind string `json:"metric_kind"`
	Window     struct {
		Kind string `json:"kind"`
	} `json:"window"`
	Unit       *string `json:"unit"`
	Used       *string `json:"used"`
	Limit      *string `json:"limit"`
	Remaining  *string `json:"remaining"`
	Exhausted  *bool   `json:"exhausted"`
	ResetAt    string  `json:"reset_at"`
	ValidUntil string  `json:"valid_until"`
	Status     string  `json:"status"`
	Reason     string  `json:"reason"`
	Quality    string  `json:"quality"`
}

type observationAttempt struct {
	AttemptID     string  `json:"attempt_id"`
	CollectedAt   string  `json:"collected_at"`
	Status        string  `json:"status"`
	Reason        *string `json:"reason"`
	ObservationID *string `json:"observation_id"`
}

type observationStream struct {
	SourceID        string             `json:"source_id"`
	LatestAttempt   observationAttempt `json:"latest_attempt"`
	LastObservation *struct {
		ObservationID string             `json:"observation_id"`
		Limits        []observationLimit `json:"limits"`
	} `json:"last_observation"`
	Freshness []struct {
		LimitID string `json:"limit_id"`
		State   string `json:"state"`
	} `json:"freshness"`
}

type observationAccess struct {
	AccessID  string            `json:"access_id"`
	ProductID string            `json:"product_id"`
	Streams   []json.RawMessage `json:"streams"`
}

// Upstream caps, applied the same way its own clients apply them.
const (
	maxObservationAccesses = 16
	maxObservationStreams  = 4
	maxObservationLimits   = 32
)

var observationUUID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// decodeObservations reads the optional observations block one access at a
// time: a missing, malformed or unknown-version block, or a malformed access,
// leaves the legacy cards untouched and only drops what cannot be read.
func decodeObservations(raw json.RawMessage, now time.Time) []QuotaProvider {
	var block struct {
		SchemaVersion int               `json:"schema_version"`
		Accesses      []json.RawMessage `json:"accesses"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &block) != nil || block.SchemaVersion != 1 {
		return nil
	}
	if len(block.Accesses) > maxObservationAccesses {
		block.Accesses = block.Accesses[:maxObservationAccesses]
	}
	byProduct := map[string][]observationAccess{}
	for _, item := range block.Accesses {
		var access observationAccess
		if json.Unmarshal(item, &access) != nil || !observationID.MatchString(access.AccessID) {
			continue
		}
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

// observationProvider shows only fresh, valid, known readings; anything else
// becomes a note, so the card never presents an expired or unknown value as
// current. The server's freshness verdict is rechecked on the dashboard clock.
func observationProvider(name string, access observationAccess, now time.Time) QuotaProvider {
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
	// expiredNotes replace notes in the browser once every shown reading has
	// lapsed, so a collection failure stays visible without "last good" wording.
	expiredNotes := []string{"Last reading expired"}
	both := func(text string) {
		note(text)
		for _, seen := range expiredNotes {
			if seen == text {
				return
			}
		}
		expiredNotes = append(expiredNotes, text)
	}
	failed, expired := false, false
	streams := access.Streams
	if len(streams) > maxObservationStreams {
		streams = streams[:maxObservationStreams]
	}
	for _, raw := range streams {
		var stream observationStream
		if json.Unmarshal(raw, &stream) != nil || !observationID.MatchString(stream.SourceID) {
			continue
		}
		attempt, attemptOK := readAttempt(stream.LatestAttempt)
		if !attemptOK {
			// An unreadable attempt makes the whole stream unknown, never a reading.
			both(reasonText("schema_mismatch"))
			continue
		}
		failed = failed || attempt.Status == "error"
		if stream.LastObservation != nil && !validObservation(stream) {
			// An invalid historical reading must not erase a valid collection failure.
			both(reasonText("schema_mismatch"))
			stream.LastObservation = nil
		}
		shown := 0
		if stream.LastObservation != nil {
			fresh := map[string]bool{}
			for _, item := range stream.Freshness {
				fresh[item.LimitID] = item.State == "fresh"
			}
			for _, limit := range stream.LastObservation.Limits {
				switch {
				case limit.Status != "known":
					both(reasonText(limit.Reason))
				case !fresh[limit.LimitID] || !current(limit, now):
					expired = true
					note("Last reading expired")
				case addObservationLimit(&provider, access.ProductID, limit):
					shown++
				default:
					both(reasonText("schema_mismatch"))
				}
			}
		}
		switch {
		case attempt.Status == "ok":
		case shown > 0:
			note("Last good reading; latest attempt: " + reasonText(*attempt.Reason))
			expiredNotes = append(expiredNotes, "Latest attempt: "+reasonText(*attempt.Reason))
		default:
			both(reasonText(*attempt.Reason))
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
	if len(provider.Windows) > 0 || len(provider.Readings) > 0 {
		provider.ExpiredMessage = strings.Join(expiredNotes, " · ")
	}
	return provider
}

// readAttempt validates an attempt as upstream does. A reason this version
// does not know reads as a format error rather than failing the attempt.
func readAttempt(attempt observationAttempt) (observationAttempt, bool) {
	if !observationUUID.MatchString(attempt.AttemptID) || parseTime(attempt.CollectedAt).IsZero() ||
		attempt.ObservationID != nil && !observationUUID.MatchString(*attempt.ObservationID) {
		return attempt, false
	}
	if attempt.Reason != nil {
		if _, known := observationReasons[*attempt.Reason]; !known {
			mismatch := "schema_mismatch"
			attempt.Reason = &mismatch
		}
	}
	switch attempt.Status {
	case "ok":
		return attempt, attempt.Reason == nil && attempt.ObservationID != nil
	case "unknown", "error":
		return attempt, attempt.Reason != nil && attempt.ObservationID == nil
	}
	return attempt, false
}

// validObservation rejects a reading as a whole, as upstream does, when any
// of its limits or freshness entries is malformed.
func validObservation(stream observationStream) bool {
	observation := stream.LastObservation
	if !observationUUID.MatchString(observation.ObservationID) || len(observation.Limits) > maxObservationLimits {
		return false
	}
	seen := map[string]bool{}
	for _, limit := range observation.Limits {
		if !validLimit(limit) || seen[limit.LimitID] {
			return false
		}
		seen[limit.LimitID] = true
	}
	for _, item := range stream.Freshness {
		switch item.State {
		case "fresh", "expired", "unknown":
		default:
			return false
		}
	}
	return true
}

// validLimit applies the contract's per-limit rules, as ai-quota's own web
// client does, before a value can reach the card.
func validLimit(limit observationLimit) bool {
	if !observationID.MatchString(limit.LimitID) || !metricKinds[limit.MetricKind] || !limitStatuses[limit.Status] ||
		!qualities[limit.Quality] || !validTime(limit.ResetAt) || !validTime(limit.ValidUntil) {
		return false
	}
	if _, ok := windowLabels[limit.Window.Kind]; !ok {
		return false
	}
	if _, ok := observationReasons[limit.Reason]; limit.Reason != "" && !ok {
		return false
	}
	numbers := []*string{limit.Used, limit.Limit, limit.Remaining}
	for _, number := range numbers {
		if number != nil && (limit.Unit == nil || !observationDecimal.MatchString(*number) || *number == "-0") {
			return false
		}
	}
	if limit.Unit != nil && !observationID.MatchString(*limit.Unit) {
		return false
	}
	negative := func(number *string) bool { return number != nil && strings.HasPrefix(*number, "-") }
	if negative(limit.Used) || negative(limit.Limit) || negative(limit.Remaining) && limit.MetricKind != "balance" {
		return false
	}
	if limit.Status != "known" {
		return limit.Used == nil && limit.Limit == nil && limit.Remaining == nil && limit.Exhausted == nil && limit.Reason != ""
	}
	constraint := limit.MetricKind == "allowance" || limit.MetricKind == "rate_limit"
	if limit.Exhausted != nil && (!constraint || limit.Quality == "estimated") {
		return false
	}
	if limit.ValidUntil != "" && limit.ResetAt != "" && parseTime(limit.ValidUntil).After(parseTime(limit.ResetAt)) {
		return false
	}
	return true
}

// current is the contract's freshness rule: a reading needs a validity
// deadline and lapses at it or at a reset, whichever comes first.
func current(limit observationLimit, now time.Time) bool {
	expires := limitExpiry(limit)
	return !expires.IsZero() && now.Before(expires)
}

func limitExpiry(limit observationLimit) time.Time {
	expires := parseTime(limit.ValidUntil)
	if reset := parseTime(limit.ResetAt); !reset.IsZero() && reset.Before(expires) {
		expires = reset
	}
	return expires
}

func validTime(value string) bool {
	return value == "" || !parseTime(value).IsZero()
}

func parseTime(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}
	}
	return parsed
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
			Severity: severity, ResetsAt: limit.ResetAt, Limited: limited, ExpiresAt: expiresAt,
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
