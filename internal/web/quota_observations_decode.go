package web

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
	"time"
)

// This file ports the tolerant reading rules of ai-quota-viewer's own web
// client (apps/web/src/api/observations.ts) for the observations v1 block:
// an invalid access is dropped, an unreadable attempt makes its stream
// unknown, and an invalid retained reading is hidden while a valid
// collection failure stays visible. Nothing here produces card text.

// Upstream caps, applied the same way its own clients apply them.
const (
	maxObservationAccesses    = 16
	maxObservationStreams     = 4
	maxObservationBindings    = 64
	maxObservationLimits      = 32
	maxObservationBindingRefs = 64
)

var (
	observationID        = regexp.MustCompile(`^[A-Za-z0-9._:/-]{1,160}$`)
	observationDecimal   = regexp.MustCompile(`^-?(0|[1-9][0-9]{0,17})(\.[0-9]{0,9}[1-9])?$`)
	observationUUID      = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	observationTimestamp = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})$`)
)

func oneOf(values ...string) map[string]bool {
	set := map[string]bool{}
	for _, value := range values {
		set[value] = true
	}
	return set
}

var (
	verificationStates = oneOf("verified", "unknown")
	attemptStatuses    = oneOf("ok", "unknown", "error")
	coverageStatuses   = oneOf("unknown", "complete_through")
	reportingLags      = oneOf("unknown", "bounded")
	sourceKinds        = oneOf("official_cli", "public_api", "first_party_internal", "local_history", "configuration")
	metricKinds        = oneOf("allowance", "rate_limit", "balance", "usage_report")
	evidenceKinds      = oneOf("provider_report", "owner_attestation")
	windowKinds        = oneOf("session", "weekly", "monthly", "other", "none")
	limitStatuses      = oneOf("known", "unknown", "error")
	qualities          = oneOf("measured", "derived", "estimated", "unknown")
	freshnessStates    = oneOf("fresh", "expired", "unknown")
)

type observationAttempt struct {
	Status string
	// Reason is empty only for an ok attempt.
	Reason string
}

type bindingRef struct {
	Engine     string `json:"engine"`
	ProviderID string `json:"provider_id"`
	ModelID    string `json:"model_id"`
	Transport  string `json:"transport"`
}

type observationLimit struct {
	LimitID    string `json:"limit_id"`
	MetricKind string `json:"metric_kind"`
	AppliesTo  struct {
		Status   string       `json:"status"`
		Bindings []bindingRef `json:"bindings"`
	} `json:"applies_to"`
	PoolMembership struct {
		Status   string  `json:"status"`
		PoolID   *string `json:"pool_id"`
		Evidence *struct {
			Kind       string `json:"kind"`
			Reference  string `json:"reference"`
			VerifiedAt string `json:"verified_at"`
		} `json:"evidence"`
	} `json:"pool_membership"`
	Window struct {
		Kind     string  `json:"kind"`
		PeriodID *string `json:"period_id"`
		StartAt  *string `json:"start_at"`
		EndAt    *string `json:"end_at"`
	} `json:"window"`
	Unit       *string `json:"unit"`
	Used       *string `json:"used"`
	Limit      *string `json:"limit"`
	Remaining  *string `json:"remaining"`
	Exhausted  *bool   `json:"exhausted"`
	ResetAt    *string `json:"reset_at"`
	ValidUntil *string `json:"valid_until"`
	Status     string  `json:"status"`
	Reason     *string `json:"reason"`
	Quality    string  `json:"quality"`
}

type observationRecord struct {
	ObservationID         string  `json:"observation_id"`
	ConfigurationRevision uint64  `json:"configuration_revision"`
	CollectedAt           string  `json:"collected_at"`
	ProviderObservedAt    *string `json:"provider_observed_at"`
	CoverageThrough       *string `json:"coverage_through"`
	CoverageStatus        string  `json:"coverage_status"`
	ReportingLag          string  `json:"reporting_lag"`
	MaxReportingLagSecs   *uint64 `json:"max_reporting_lag_secs"`
	Source                struct {
		Kind      string `json:"kind"`
		Reference string `json:"reference"`
	} `json:"source"`
	Limits []observationLimit `json:"limits"`
}

// observationStream is one decoded stream. Observation is nil when there is
// no retained reading or when it could not be read (Unreadable).
type observationStream struct {
	Attempt     observationAttempt
	Observation *observationRecord
	Freshness   map[string]string
	Unreadable  bool
}

type observationAccess struct {
	AccessID  string
	ProductID string
	Streams   []observationStream
}

// decodeObservationAccesses returns nil for a missing, malformed or
// unknown-version block, and otherwise every readable access.
func decodeObservationAccesses(raw json.RawMessage) []observationAccess {
	var block struct {
		SchemaVersion *int              `json:"schema_version"`
		Accesses      []json.RawMessage `json:"accesses"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &block) != nil || block.SchemaVersion == nil || *block.SchemaVersion != 1 || block.Accesses == nil {
		return nil
	}
	if len(block.Accesses) > maxObservationAccesses {
		block.Accesses = block.Accesses[:maxObservationAccesses]
	}
	var accesses []observationAccess
	for _, item := range block.Accesses {
		if access, ok := decodeObservationAccess(item); ok {
			accesses = append(accesses, access)
		}
	}
	return accesses
}

func decodeObservationAccess(raw json.RawMessage) (observationAccess, bool) {
	var access struct {
		AccessID              string            `json:"access_id"`
		ConfigurationRevision uint64            `json:"configuration_revision"`
		ProductID             string            `json:"product_id"`
		Bindings              []bindingRef      `json:"bindings"`
		Streams               []json.RawMessage `json:"streams"`
	}
	if json.Unmarshal(raw, &access) != nil || !observationID.MatchString(access.AccessID) || access.ConfigurationRevision < 1 ||
		!observationID.MatchString(access.ProductID) || access.Bindings == nil || access.Streams == nil ||
		len(access.Bindings) > maxObservationBindings || !validBindingRefs(access.Bindings) {
		return observationAccess{}, false
	}
	decoded := observationAccess{AccessID: access.AccessID, ProductID: access.ProductID}
	if len(access.Streams) > maxObservationStreams {
		access.Streams = access.Streams[:maxObservationStreams]
	}
	for _, item := range access.Streams {
		var head struct {
			SourceID string `json:"source_id"`
		}
		if json.Unmarshal(item, &head) != nil || !observationID.MatchString(head.SourceID) {
			continue
		}
		decoded.Streams = append(decoded.Streams, decodeObservationStream(item))
	}
	return decoded, true
}

func decodeObservationStream(raw json.RawMessage) observationStream {
	unreadable := observationStream{Attempt: observationAttempt{Status: "unknown", Reason: "schema_mismatch"}, Unreadable: true}
	var parts struct {
		LatestAttempt   json.RawMessage `json:"latest_attempt"`
		LastObservation json.RawMessage `json:"last_observation"`
		Freshness       json.RawMessage `json:"freshness"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return unreadable
	}
	attempt, ok := decodeAttempt(parts.LatestAttempt)
	if !ok {
		return unreadable
	}
	stream := observationStream{Attempt: attempt}
	observation, observationOK := decodeObservation(parts.LastObservation)
	freshness, freshnessOK := decodeFreshness(parts.Freshness)
	if !observationOK || !freshnessOK {
		if attempt.Status == "ok" {
			return unreadable
		}
		// An invalid historical reading must not erase a valid collection failure.
		stream.Unreadable = true
		return stream
	}
	stream.Observation, stream.Freshness = observation, freshness
	return stream
}

func decodeAttempt(raw json.RawMessage) (observationAttempt, bool) {
	var attempt struct {
		AttemptID     string          `json:"attempt_id"`
		CollectedAt   string          `json:"collected_at"`
		Status        string          `json:"status"`
		Reason        json.RawMessage `json:"reason"`
		ObservationID *string         `json:"observation_id"`
	}
	if json.Unmarshal(raw, &attempt) != nil || !observationUUID.MatchString(attempt.AttemptID) ||
		!validTimestamp(attempt.CollectedAt) || !attemptStatuses[attempt.Status] ||
		attempt.ObservationID != nil && !observationUUID.MatchString(*attempt.ObservationID) {
		return observationAttempt{}, false
	}
	reason := ""
	if !isJSONNull(attempt.Reason) {
		// A reason this version does not know reads as a format error.
		reason = "schema_mismatch"
		var text string
		if json.Unmarshal(attempt.Reason, &text) == nil && observationReasons[text] != "" {
			reason = text
		}
	}
	if attempt.Status == "ok" {
		return observationAttempt{Status: "ok"}, reason == "" && attempt.ObservationID != nil
	}
	return observationAttempt{Status: attempt.Status, Reason: reason}, reason != "" && attempt.ObservationID == nil
}

func decodeObservation(raw json.RawMessage) (*observationRecord, bool) {
	if isJSONNull(raw) {
		return nil, true
	}
	var observation observationRecord
	if json.Unmarshal(raw, &observation) != nil || !validObservation(observation) {
		return nil, false
	}
	return &observation, true
}

func decodeFreshness(raw json.RawMessage) (map[string]string, bool) {
	var entries []struct {
		LimitID *string `json:"limit_id"`
		State   string  `json:"state"`
	}
	if json.Unmarshal(raw, &entries) != nil || entries == nil {
		return nil, false
	}
	freshness := map[string]string{}
	for _, entry := range entries {
		if entry.LimitID == nil || !freshnessStates[entry.State] {
			return nil, false
		}
		freshness[*entry.LimitID] = entry.State
	}
	return freshness, true
}

func validObservation(observation observationRecord) bool {
	if !observationUUID.MatchString(observation.ObservationID) || observation.ConfigurationRevision < 1 ||
		!validTimestamp(observation.CollectedAt) || !validOptionalTimestamp(observation.ProviderObservedAt) ||
		!validOptionalTimestamp(observation.CoverageThrough) || !coverageStatuses[observation.CoverageStatus] ||
		!reportingLags[observation.ReportingLag] || !sourceKinds[observation.Source.Kind] ||
		!observationID.MatchString(observation.Source.Reference) ||
		observation.Limits == nil || len(observation.Limits) > maxObservationLimits {
		return false
	}
	collected := parseTime(observation.CollectedAt)
	notAfterCollection := func(value *string) bool { return value == nil || !parseTime(*value).After(collected) }
	if !notAfterCollection(observation.ProviderObservedAt) || !notAfterCollection(observation.CoverageThrough) ||
		(observation.CoverageStatus == "complete_through") != (observation.CoverageThrough != nil) ||
		(observation.ReportingLag == "bounded") != (observation.MaxReportingLagSecs != nil) {
		return false
	}
	seen := map[string]bool{}
	for _, limit := range observation.Limits {
		if !validLimit(limit) || seen[limit.LimitID] {
			return false
		}
		seen[limit.LimitID] = true
	}
	return true
}

// validLimit applies the contract's per-limit rules before a value can reach
// the card.
func validLimit(limit observationLimit) bool {
	if !observationID.MatchString(limit.LimitID) || !metricKinds[limit.MetricKind] || !limitStatuses[limit.Status] ||
		!qualities[limit.Quality] || !validOptionalTimestamp(limit.ResetAt) || !validOptionalTimestamp(limit.ValidUntil) ||
		!validOptionalID(limit.Unit) || limit.Reason != nil && observationReasons[*limit.Reason] == "" {
		return false
	}
	applies := limit.AppliesTo
	if !verificationStates[applies.Status] || applies.Bindings == nil || len(applies.Bindings) > maxObservationBindingRefs ||
		!validBindingRefs(applies.Bindings) || (applies.Status == "verified") != (len(applies.Bindings) > 0) {
		return false
	}
	pool := limit.PoolMembership
	if !verificationStates[pool.Status] || !validOptionalID(pool.PoolID) {
		return false
	}
	if evidence := pool.Evidence; evidence != nil && (!evidenceKinds[evidence.Kind] ||
		!observationID.MatchString(evidence.Reference) || !validTimestamp(evidence.VerifiedAt)) {
		return false
	}
	if verified := pool.Status == "verified"; verified != (pool.PoolID != nil) || verified != (pool.Evidence != nil) {
		return false
	}
	window := limit.Window
	if !windowKinds[window.Kind] || !validOptionalID(window.PeriodID) ||
		!validOptionalTimestamp(window.StartAt) || !validOptionalTimestamp(window.EndAt) {
		return false
	}
	if window.Kind == "none" && (window.PeriodID != nil || window.StartAt != nil || window.EndAt != nil) {
		return false
	}
	if window.StartAt != nil && window.EndAt != nil && !parseTime(*window.StartAt).Before(parseTime(*window.EndAt)) {
		return false
	}
	numbers := []*string{limit.Used, limit.Limit, limit.Remaining}
	for _, number := range numbers {
		if number != nil && (limit.Unit == nil || !observationDecimal.MatchString(*number) || *number == "-0") {
			return false
		}
	}
	negative := func(number *string) bool { return number != nil && strings.HasPrefix(*number, "-") }
	// Only a signed ledger documents a negative remaining.
	if negative(limit.Used) || negative(limit.Limit) || negative(limit.Remaining) && limit.MetricKind != "balance" {
		return false
	}
	if limit.Status != "known" &&
		(limit.Used != nil || limit.Limit != nil || limit.Remaining != nil || limit.Exhausted != nil || limit.Reason == nil) {
		return false
	}
	// Reports, ledgers and estimates never become an exhaustion verdict.
	constraint := limit.MetricKind == "allowance" || limit.MetricKind == "rate_limit"
	if limit.Exhausted != nil && (!constraint || limit.Quality == "estimated") {
		return false
	}
	if limit.ValidUntil != nil && limit.ResetAt != nil && parseTime(*limit.ValidUntil).After(parseTime(*limit.ResetAt)) {
		return false
	}
	return true
}

func validBindingRefs(refs []bindingRef) bool {
	seen := map[bindingRef]bool{}
	for _, ref := range refs {
		if !observationID.MatchString(ref.Engine) || !observationID.MatchString(ref.ProviderID) ||
			!observationID.MatchString(ref.ModelID) || !observationID.MatchString(ref.Transport) || seen[ref] {
			return false
		}
		seen[ref] = true
	}
	return true
}

func validOptionalID(value *string) bool {
	return value == nil || observationID.MatchString(*value)
}

func validTimestamp(value string) bool {
	return observationTimestamp.MatchString(value) && !parseTime(value).IsZero()
}

func validOptionalTimestamp(value *string) bool {
	return value == nil || validTimestamp(*value)
}

func parseTime(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}
	}
	return parsed
}

func isJSONNull(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null"))
}
