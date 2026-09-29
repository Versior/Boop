package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"boop/internal/config"
	"boop/internal/settings"
	"boop/internal/store"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

func migratedStore(t *testing.T) *sql.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "boop.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(db); err != nil {
		t.Fatalf("store.Migrate: %v", err)
	}
	return db
}

func storageConfig(t *testing.T, masterKey bool) config.Config {
	t.Helper()
	cfg := config.Config{
		DataDir:     t.TempDir(),
		BaseURL:     "http://localhost:8080",
		MaxUploadMB: 10,
		LogLevel:    "info",
		Storage: config.Storage{
			Endpoint:  "https://account.r2.cloudflarestorage.com",
			Region:    "auto",
			Bucket:    "boop-uploads",
			Prefix:    "uploads",
			PublicURL: "https://uploads.example.com",
			AccessKey: "an-access-key-id",
			SecretKey: "a-secret-access-key",
		},
	}
	if masterKey {
		cfg.MasterKey = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	}
	return cfg
}

func TestImportStorageFromEnvironmentSeedsTheSettingsTableOnce(t *testing.T) {
	db := migratedStore(t)
	ctx := context.Background()
	cfg := storageConfig(t, true)

	if err := importStorageFromEnvironment(ctx, cfg, db, discardLogger()); err != nil {
		t.Fatalf("importStorageFromEnvironment: %v", err)
	}
	// Seed runs after the import, and must not disturb what it wrote.
	if err := settings.Seed(ctx, db); err != nil {
		t.Fatalf("settings.Seed: %v", err)
	}

	box, err := cfg.SecretBox()
	if err != nil {
		t.Fatalf("SecretBox: %v", err)
	}
	storage, err := settings.ReadStorage(ctx, db, box, config.Config{}.StorageSelection())
	if err != nil {
		t.Fatalf("ReadStorage: %v", err)
	}
	if !storage.Object() || storage.Bucket != "boop-uploads" {
		t.Fatalf("ReadStorage = %+v, want the environment's bucket", storage)
	}
	if storage.AccessKeyID != "an-access-key-id" || storage.SecretAccessKey != "a-secret-access-key" {
		t.Errorf("the credentials were not stored: %+v", storage)
	}

	// The environment is a seed, not a permanent override: once the page has
	// saved the category the environment is ignored, which is what makes the
	// page meaningful.
	if err := settings.Apply(ctx, db, box, settings.Update{
		Values: map[string]any{settings.KeyStorageMode: settings.StorageModeLocal},
	}); err != nil {
		t.Fatalf("settings.Apply: %v", err)
	}
	if err := importStorageFromEnvironment(ctx, cfg, db, discardLogger()); err != nil {
		t.Fatalf("second importStorageFromEnvironment: %v", err)
	}
	storage, err = settings.ReadStorage(ctx, db, box, cfg.StorageSelection())
	if err != nil {
		t.Fatalf("ReadStorage: %v", err)
	}
	if storage.Object() {
		t.Errorf("ReadStorage = %+v, want the saved local directory to win", storage)
	}
}

// A deployment configured with nothing but BOOP_R2_* has no way to have its
// credentials encrypted, so nothing is imported and the environment stays in
// charge. Writing the local default instead would send new uploads to the disk
// while every published image lives in the bucket.
func TestImportStorageLeavesTheEnvironmentInChargeWithoutTheMasterKey(t *testing.T) {
	db := migratedStore(t)
	ctx := context.Background()
	cfg := storageConfig(t, false)

	if err := importStorageFromEnvironment(ctx, cfg, db, discardLogger()); err != nil {
		t.Fatalf("importStorageFromEnvironment: %v", err)
	}
	if err := settings.Seed(ctx, db); err != nil {
		t.Fatalf("settings.Seed: %v", err)
	}
	configured, err := settings.StorageConfigured(ctx, db)
	if err != nil {
		t.Fatalf("StorageConfigured: %v", err)
	}
	if configured {
		t.Fatal("the failed import marked the storage category as saved")
	}

	storage, err := settings.ReadStorage(ctx, db, nil, cfg.StorageSelection())
	if err != nil {
		t.Fatalf("ReadStorage: %v", err)
	}
	if !storage.Object() || storage.Bucket != "boop-uploads" {
		t.Errorf("ReadStorage = %+v, want the environment's bucket", storage)
	}
	if storage.AccessKeyID == "" || storage.SecretAccessKey == "" {
		t.Errorf("the environment's credentials were not carried over: %+v", storage)
	}
}
