// Command boop runs the single-process Boop blog: SSR pages, embedded assets
// and the JSON API on one HTTP listener. It also owns the one-time owner
// bootstrap command.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"boop/internal/config"
	"boop/internal/server"
	"boop/internal/settings"
	"boop/internal/store"
)

func main() {
	var err error
	switch commandName(os.Args) {
	case "serve":
		err = runServer()
	case "init-owner":
		err = runInitOwner(os.Args[2:], os.Stdout, promptHiddenPassword)
	default:
		err = fmt.Errorf("未知命令 %q，可用命令：serve（默认）、init-owner", os.Args[1])
	}
	if err != nil {
		logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
		logger.Error("boop exited with an error", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

// commandName reads the subcommand, defaulting to serve so an argument-free
// start (and flag-only invocations) keep working.
func commandName(args []string) string {
	if len(args) < 2 || strings.HasPrefix(args[1], "-") {
		return "serve"
	}
	return args[1]
}

// openMigratedStore opens the configured database and applies every pending
// migration, so both commands always work against the current schema.
func openMigratedStore(cfg config.Config, logger *slog.Logger) (*sql.DB, error) {
	db, err := store.Open(cfg.DatabasePath())
	if err != nil {
		return nil, err
	}
	if err := store.Migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	logger.Info("database ready",
		slog.String("path", cfg.DatabasePath()),
		slog.Int("schema_version", store.LatestVersion()),
	)
	return db, nil
}

// openStore is the command-side helper: load the configuration, then open the
// migrated database.
func openStore(logger *slog.Logger) (config.Config, *sql.DB, error) {
	cfg, err := config.Load()
	if err != nil {
		return config.Config{}, nil, err
	}
	db, err := openMigratedStore(cfg, logger)
	if err != nil {
		return config.Config{}, nil, err
	}
	return cfg, db, nil
}

func runServer() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.SlogLevel()}))
	slog.SetDefault(logger)

	db, err := openMigratedStore(cfg, logger)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := settings.Seed(context.Background(), db); err != nil {
		return err
	}

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           server.New(cfg, db),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       90 * time.Second,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("boop listening",
			slog.String("addr", cfg.Addr),
			slog.String("base_url", cfg.BaseURL),
			slog.String("data_dir", cfg.DataDir),
		)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
		logger.Info("boop shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}
