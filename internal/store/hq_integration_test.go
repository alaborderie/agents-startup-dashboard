package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestHQDocumentRoundTripsAndStaleDocumentsAreSkipped(t *testing.T) {
	resetDatabase(t)
	ctx := context.Background()
	if state, err := New(testPool).HQ(ctx); err != nil || state != nil {
		t.Fatalf("empty hq state = %+v, %v", state, err)
	}
	s := New(testPool)
	at := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	applied, err := s.ApplyHQ(ctx, at, json.RawMessage(`{"cycle":2,"ventures":[]}`))
	if err != nil || !applied {
		t.Fatalf("apply = %v, %v", applied, err)
	}
	applied, err = s.ApplyHQ(ctx, at.Add(-time.Minute), json.RawMessage(`{"cycle":1}`))
	if err != nil || applied {
		t.Fatalf("stale apply = %v, %v", applied, err)
	}
	state, err := s.HQ(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct{ Cycle int }
	if err := json.Unmarshal(state.Doc, &doc); err != nil || doc.Cycle != 2 || !state.GeneratedAt.Equal(at) {
		t.Fatalf("stored hq = %s at %v", state.Doc, state.GeneratedAt)
	}
}

func TestLoginLinksAreSingleUseAndExpire(t *testing.T) {
	resetDatabase(t)
	ctx := context.Background()
	s := New(testPool)
	if err := s.CreateLoginLink(ctx, "live", time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateLoginLink(ctx, "expired", time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		hash string
		want bool
	}{{"live", true}, {"live", false}, {"expired", false}, {"unknown", false}} {
		got, err := s.ConsumeLoginLink(ctx, c.hash)
		if err != nil || got != c.want {
			t.Fatalf("consume %s = %v, %v; want %v", c.hash, got, err, c.want)
		}
	}
}

func TestSessionsExpireAndCanBeDeleted(t *testing.T) {
	resetDatabase(t)
	ctx := context.Background()
	s := New(testPool)
	if err := s.CreateSession(ctx, "current", time.Now().Add(time.Hour), "test agent"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession(ctx, "old", time.Now().Add(-time.Second), ""); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.ValidSession(ctx, "current"); err != nil || !ok {
		t.Fatalf("current session = %v, %v", ok, err)
	}
	if ok, _ := s.ValidSession(ctx, "old"); ok {
		t.Fatal("expired session accepted")
	}
	if err := s.DeleteSession(ctx, "current"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.ValidSession(ctx, "current"); ok {
		t.Fatal("deleted session accepted")
	}
}

func TestCommandLifecycle(t *testing.T) {
	resetDatabase(t)
	ctx := context.Background()
	s := New(testPool)

	first, created, err := s.CreateCommand(ctx, NewCommand{ID: "c_1", Kind: "approve", Slug: "demo", Gate: "design", ClientID: "click"})
	if err != nil || !created || first.Status != "queued" {
		t.Fatalf("create = %+v %v %v", first, created, err)
	}
	again, created, err := s.CreateCommand(ctx, NewCommand{ID: "c_2", Kind: "approve", Slug: "demo", Gate: "design", ClientID: "click"})
	if err != nil || created || again.ID != "c_1" {
		t.Fatalf("repeated client id = %+v %v %v", again, created, err)
	}
	// Commands without a client id never collide with each other.
	for _, id := range []string{"c_3", "c_4"} {
		if _, created, err := s.CreateCommand(ctx, NewCommand{ID: id, Kind: "tick"}); err != nil || !created {
			t.Fatalf("create %s: %v %v", id, created, err)
		}
	}

	claimed, err := s.ClaimCommands(ctx, time.Hour, 10)
	if err != nil || len(claimed) != 3 || claimed[0].ID != "c_1" || claimed[0].Status != "delivered" || claimed[0].DeliveredAt == nil {
		t.Fatalf("claim = %+v %v", claimed, err)
	}
	if claimed, _ := s.ClaimCommands(ctx, time.Hour, 10); len(claimed) != 0 {
		t.Fatalf("delivered commands claimed again before redelivery: %+v", claimed)
	}
	if claimed, _ := s.ClaimCommands(ctx, 0, 1); len(claimed) != 1 || claimed[0].Attempts != 2 {
		t.Fatalf("redelivery = %+v", claimed)
	}

	done, found, changed, err := s.FinishCommand(ctx, "c_1", "done", "Approved demo (design).")
	if err != nil || !found || !changed || done.Status != "done" || done.FinishedAt == nil {
		t.Fatalf("finish = %+v %v %v %v", done, found, changed, err)
	}
	late, found, changed, err := s.FinishCommand(ctx, "c_1", "failed", "late")
	if err != nil || !found || changed || late.Status != "done" || late.Message != "Approved demo (design)." {
		t.Fatalf("second finish = %+v %v %v %v", late, found, changed, err)
	}
	if _, found, _, err := s.FinishCommand(ctx, "c_missing", "done", ""); err != nil || found {
		t.Fatalf("unknown finish found=%v err=%v", found, err)
	}
	if claimed, _ := s.ClaimCommands(ctx, 0, 10); len(claimed) != 2 {
		t.Fatalf("finished command redelivered: %+v", claimed)
	}

	recent, err := s.RecentCommands(ctx, 2)
	if err != nil || len(recent) != 2 || recent[0].ID != "c_4" {
		t.Fatalf("recent = %+v %v", recent, err)
	}
}

// The laptop's document carries artifact text and log-derived events, so it can
// hold any JSON string, including an escaped NUL or a lone surrogate. jsonb
// rejects both, which would freeze the HQ on the last good document.
func TestHQDocumentIsStoredVerbatimWhateverItsStrings(t *testing.T) {
	resetDatabase(t)
	ctx := context.Background()
	s := New(testPool)
	doc := json.RawMessage(`{"generated_at":"2026-09-27T12:00:00Z","focus":"a\u0000b \udcff"}`)
	applied, err := s.ApplyHQ(ctx, time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC), doc)
	if err != nil || !applied {
		t.Fatalf("apply = %v, %v", applied, err)
	}
	state, err := s.HQ(ctx)
	if err != nil || string(state.Doc) != string(doc) {
		t.Fatalf("stored doc = %s, %v", state.Doc, err)
	}
}
