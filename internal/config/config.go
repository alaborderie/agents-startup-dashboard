package config

import (
	"fmt"
	"os"
)

type Config struct {
	DatabaseURL string
	IngestToken string
	Port        string
	PublicURL   string
}

func Load() (Config, error) {
	config := Config{
		DatabaseURL: os.Getenv("DATABASE_URL"),
		IngestToken: os.Getenv("DASHBOARD_INGEST_TOKEN"),
		Port:        os.Getenv("PORT"),
		PublicURL:   os.Getenv("PUBLIC_URL"),
	}
	if config.DatabaseURL == "" {
		return Config{}, fmt.Errorf("DATABASE_URL is required")
	}
	if config.IngestToken == "" {
		return Config{}, fmt.Errorf("DASHBOARD_INGEST_TOKEN is required")
	}
	if config.Port == "" {
		config.Port = "8080"
	}
	if config.PublicURL == "" {
		config.PublicURL = "https://agents-startup.antoinelaborderie.com"
	}
	return config, nil
}
