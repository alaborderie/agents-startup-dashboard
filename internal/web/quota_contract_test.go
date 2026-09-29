package web

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestQuotaContractFixtures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		counts []int
	}{
		{"quota.json", []int{2, 1, 3, 0}},
		{"quota-busy.json", []int{2, 1, 3, 0}},
		{"quota-empty.json", []int{0, 0, 0, 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, err := os.ReadFile("../../testdata/ai-quota/" + tc.name)
			if err != nil {
				t.Fatal(err)
			}
			view, err := decodeQuota(data)
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			var counts []int
			for _, provider := range view.Providers {
				names = append(names, provider.Provider)
				counts = append(counts, len(provider.Windows))
			}
			if view.State != "ok" || view.GeneratedAt != "2026-09-25T23:10:00Z" ||
				!reflect.DeepEqual(names, []string{"claude", "codex", "opencode_go", "opencode_zen"}) || !reflect.DeepEqual(counts, tc.counts) {
				t.Fatalf("view = %+v, provider order = %v, window counts = %v", view, names, counts)
			}
			if tc.name == "quota.json" {
				if view.Providers[0].Windows[0].UsedPercent != 36 || view.Providers[0].Windows[0].ResetsAt != "2026-09-25T23:14:00Z" {
					t.Fatalf("Claude's live values dropped: %+v", view.Providers[0].Windows[0])
				}
				zen := view.Providers[3]
				if zen.Balance == nil || zen.Balance.AmountUSD != 19.31 || !zen.Balance.Estimated || zen.Plan != "" {
					t.Fatalf("Zen balance dropped: %+v", zen)
				}
			}
			if tc.name == "quota-busy.json" {
				if view.Providers[0].Windows[0].Severity != "critical" || view.Providers[1].StatusState != "error" || view.Providers[1].StatusMessage != "Codex is logged out" || view.Providers[2].Freshness != "stale" {
					t.Fatalf("warning states dropped: %+v", view.Providers)
				}
			}
			if tc.name == "quota-empty.json" && view.Providers[0].Freshness != "missing" {
				t.Fatalf("missing status dropped: %+v", view.Providers[0])
			}
		})
	}
}

func TestQuotaContractIgnoresUnknownProvidersAndNormalizesSeverity(t *testing.T) {
	data, err := os.ReadFile("../../testdata/ai-quota/quota.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	var providers []map[string]json.RawMessage
	if err := json.Unmarshal(doc["providers"], &providers); err != nil {
		t.Fatal(err)
	}
	providers[0]["provider"] = json.RawMessage(`"some_new_provider"`)
	providers[1]["windows"] = json.RawMessage(`[ {"id":"weekly","label":"Week","headline_eligible":true,"used_percent":72,"severity":"mystery","resets_at":"2026-10-01T12:00:00Z"} ]`)
	doc["providers"], _ = json.Marshal(providers)
	data, _ = json.Marshal(doc)
	view, err := decodeQuota(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Providers) != 3 || view.Providers[0].Provider != "codex" || view.Providers[0].Windows[0].Severity != "warn" {
		t.Fatalf("forward-compatible decode: %+v", view.Providers)
	}
	encoded, _ := json.Marshal(view)
	if strings.Contains(string(encoded), "some_new_provider") || strings.Contains(string(encoded), "mystery") {
		t.Fatalf("unknown enum escaped into browser: %s", encoded)
	}
}
