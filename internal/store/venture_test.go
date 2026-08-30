package store

import (
	"encoding/json"
	"testing"
	"time"
)

func TestVentureUnmarshalCreatedAt(t *testing.T) {
	canonical := "2026-08-30T12:34:56Z"
	legacy := "2026-08-01T10:00:00Z"
	var venture Venture
	if err := json.Unmarshal([]byte(`{"created_at":"`+canonical+`","created":"`+legacy+`"}`), &venture); err != nil {
		t.Fatal(err)
	}

	want, err := time.Parse(time.RFC3339, canonical)
	if err != nil {
		t.Fatal(err)
	}
	if venture.CreatedAt == nil || !venture.CreatedAt.Equal(want) {
		t.Fatalf("CreatedAt = %v, want %s", venture.CreatedAt, want)
	}
}

func TestVentureUnmarshalLegacyCreated(t *testing.T) {
	legacy := "2026-08-01T10:00:00+02:00"
	var venture Venture
	if err := json.Unmarshal([]byte(`{"created":"`+legacy+`"}`), &venture); err != nil {
		t.Fatal(err)
	}

	want, err := time.Parse(time.RFC3339, legacy)
	if err != nil {
		t.Fatal(err)
	}
	if venture.CreatedAt == nil || !venture.CreatedAt.Equal(want) {
		t.Fatalf("CreatedAt = %v, want %s", venture.CreatedAt, want)
	}
}

func TestVentureUnmarshalWithoutCreatedAt(t *testing.T) {
	var venture Venture
	if err := json.Unmarshal([]byte(`{"slug":"no-created-at"}`), &venture); err != nil {
		t.Fatal(err)
	}
	if venture.CreatedAt != nil {
		t.Fatalf("CreatedAt = %v, want nil", venture.CreatedAt)
	}
}
