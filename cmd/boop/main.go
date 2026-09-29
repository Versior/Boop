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

// describeStorage says where uploads are kept, in the two terms an operator
// needs in order to reason about backups: which backend, and which bucket or
// directory. The credentials are never part of it.
func describeStorage(storage settings.Storage) string {
	if !storage.Object() {
		return "local"
	}
	return "bucket " + storage.Bucket + " read from " + storage.PublicURL
}

// importStorageFromEnvironment seeds the storage category from BOOP_R2_* on the
// first start, so a deployment that configures its bucket through the
// environment keeps working now that the category exists, and its settings page
// can show — and change — what it is using. Once the site has saved the
// category the environment is ignored; that is what makes the page meaningful.
//
// A missing BOOP_MASTER_KEY is not fatal here. The credentials cannot be
// encrypted without it, so nothing is written and the process stays on the
// environment's configuration, which is exactly what it did before this
// category existed.
func importStorageFromEnvironment(ctx context.Context, cfg config.Config, db *sql.DB, logger *slog.Logger) error {
	selection := cfg.StorageSelection()
	if !selection.Object() {
		return nil
	}
	box, err := cfg.SecretBox()
	if err != nil {
		return err
	}
	imported, err := settings.ImportStorage(ctx, db, box, selection)
	if errors.Is(err, settings.ErrMasterKeyRequired) {
		logger.Warn("BOOP_R2_* stays in charge: without BOOP_MASTER_KEY its credentials cannot be stored, so the storage settings page can only be read")
		return nil
	}
	if err != nil {
		return err
	}
	if imported {
		logger.Info("storage configuration imported from the environment",
			slog.String("bucket", selection.Bucket),
			slog.String("public_url", selection.PublicURL),
		)
	}
	return nil
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
	// The environment seeds the storage category before the defaults are
	// written: Seed fills in every missing key and never overwrites, so the
	// order matters only for the one category the environment still owns.
	if err := importStorageFromEnvironment(context.Background(), cfg, db, logger); err != nil {
		return err
	}
	if err := settings.Seed(context.Background(), db); err != nil {
		return err
	}
	// Where uploads go is read from the settings table rather than from the
	// environment, so the startup log reports what the process will actually
	// use — including a bucket configured from the page.
	box, err := cfg.SecretBox()
	if err != nil {
		return err
	}
	storage, err := settings.ReadStorage(context.Background(), db, box, cfg.StorageSelection())
	if err != nil {
		return err
	}
	uploads := describeStorage(storage)

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
			slog.String("uploads", uploads),
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
