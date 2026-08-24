package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/alaborderie/agents-startup-dashboard/internal/config"
	"github.com/alaborderie/agents-startup-dashboard/internal/store"
	"github.com/alaborderie/agents-startup-dashboard/internal/web"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

func run() error {
	config, err := config.Load()
	if err != nil {
		return err
	}

	poolConfig, err := pgxpool.ParseConfig(config.DatabaseURL)
	if err != nil {
		return errors.New("invalid DATABASE_URL")
	}
	poolConfig.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(context.Background(), poolConfig)
	if err != nil {
		return errors.New("database pool creation failed")
	}
	defer pool.Close()

	startupContext, cancelStartup := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelStartup()
	if err := pool.Ping(startupContext); err != nil {
		return errors.New("database ping failed")
	}
	if err := store.Migrate(startupContext, pool); err != nil {
		return fmt.Errorf("migrations failed: %w", err)
	}

	dataStore := store.New(pool)
	app, err := web.New(dataStore, config.IngestToken)
	if err != nil {
		return fmt.Errorf("load templates: %w", err)
	}

	server := &http.Server{
		Addr:              ":" + config.Port,
		Handler:           app.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	serverErrors := make(chan error, 1)
	go func() {
		log.Printf("dashboard listening on :%s", config.Port)
		serverErrors <- server.ListenAndServe()
	}()

	signalContext, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	select {
	case err := <-serverErrors:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve HTTP: %w", err)
	case <-signalContext.Done():
	}

	shutdownContext, cancelShutdown := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelShutdown()
	if err := server.Shutdown(shutdownContext); err != nil {
		return fmt.Errorf("shutdown HTTP server: %w", err)
	}
	return nil
}
