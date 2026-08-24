package config

import (
	"strings"
	"testing"
)

func TestLoadRequiresSecretsByNameWithoutEchoingValues(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	t.Setenv("DASHBOARD_INGEST_TOKEN", "")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Fatalf("Load() error = %v, want DATABASE_URL named", err)
	}

	t.Setenv("DATABASE_URL", "postgres://user:do-not-echo@example.invalid/database")
	_, err = Load()
	if err == nil || !strings.Contains(err.Error(), "DASHBOARD_INGEST_TOKEN") {
		t.Fatalf("Load() error = %v, want DASHBOARD_INGEST_TOKEN named", err)
	}
	if strings.Contains(err.Error(), "do-not-echo") {
		t.Fatalf("Load() error exposed configuration value: %v", err)
	}
}

func TestLoadDefaults(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://example.invalid/database")
	t.Setenv("DASHBOARD_INGEST_TOKEN", "fake-test-token")
	t.Setenv("PORT", "")
	t.Setenv("PUBLIC_URL", "")

	config, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if config.Port != "8080" {
		t.Fatalf("Port = %q, want 8080", config.Port)
	}
	if config.PublicURL != "https://agents-startup.antoinelaborderie.com" {
		t.Fatalf("PublicURL = %q", config.PublicURL)
	}
}
