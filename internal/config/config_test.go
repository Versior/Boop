package config

import (
	"bytes"
	"encoding/base64"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"boop/internal/settings"
)

// envFrom turns a map into a LookupEnv-shaped function so tests never depend on
// ambient BOOP_* variables.
func envFrom(pairs map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		value, ok := pairs[key]
		return value, ok
	}
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := load(envFrom(nil))
	if err != nil {
		t.Fatalf("load() with empty environment: %v", err)
	}
	if cfg.Addr != "127.0.0.1:8080" {
		t.Errorf("Addr = %q, want 127.0.0.1:8080", cfg.Addr)
	}
	if cfg.DataDir != "./data" {
		t.Errorf("DataDir = %q, want ./data", cfg.DataDir)
	}
	if cfg.BaseURL != "http://localhost:8080" {
		t.Errorf("BaseURL = %q, want http://localhost:8080", cfg.BaseURL)
	}
	if cfg.SessionSecret != "" {
		t.Errorf("SessionSecret = %q, want empty", cfg.SessionSecret)
	}
	if cfg.MasterKey != "" {
		t.Errorf("MasterKey = %q, want empty", cfg.MasterKey)
	}
	if cfg.SecureCookies {
		t.Error("SecureCookies = true, want false")
	}
	if cfg.MaxUploadMB != 10 {
		t.Errorf("MaxUploadMB = %d, want 10", cfg.MaxUploadMB)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("LogLevel = %q, want info", cfg.LogLevel)
	}
	if cfg.Storage.Endpoint != "" || cfg.Storage.Bucket != "" || cfg.Storage.PublicURL != "" {
		t.Errorf("Storage = %+v, want it empty so uploads stay under DataDir", cfg.Storage)
	}
}

// objectEnvironment is a complete object-storage configuration, so each test
// below only has to state the one variable it is about.
func objectEnvironment() map[string]string {
	return map[string]string{
		"BOOP_R2_ENDPOINT":          "https://0123456789abcdef.r2.cloudflarestorage.com",
		"BOOP_R2_BUCKET":            "boop-uploads",
		"BOOP_R2_ACCESS_KEY_ID":     "an-access-key",
		"BOOP_R2_SECRET_ACCESS_KEY": "a-secret-key",
		"BOOP_R2_PUBLIC_URL":        "https://uploads.example.com",
	}
}

func TestLoadReadsObjectStorage(t *testing.T) {
	environment := objectEnvironment()
	environment["BOOP_R2_PREFIX"] = "/uploads/"
	environment["BOOP_R2_REGION"] = "auto"

	cfg, err := load(envFrom(environment))
	if err != nil {
		t.Fatalf("load(): %v", err)
	}
	object := cfg.ObjectStorage()
	if !object.Enabled() {
		t.Fatal("object storage is disabled after a complete configuration")
	}
	if object.Endpoint != environment["BOOP_R2_ENDPOINT"] {
		t.Errorf("endpoint = %q, want %q", object.Endpoint, environment["BOOP_R2_ENDPOINT"])
	}
	if object.Bucket != "boop-uploads" {
		t.Errorf("bucket = %q, want boop-uploads", object.Bucket)
	}
	if object.PublicURL != "https://uploads.example.com" {
		t.Errorf("public URL = %q, want https://uploads.example.com", object.PublicURL)
	}
	// A prefix is documented as a path, so it is accepted with the slashes a
	// path carries and stored without them.
	if object.Prefix != "uploads" {
		t.Errorf("prefix = %q, want uploads", object.Prefix)
	}
	if object.RegionOrDefault() != "auto" {
		t.Errorf("region = %q, want auto", object.RegionOrDefault())
	}
}

func TestLoadDefaultsTheObjectStorageRegion(t *testing.T) {
	// R2 signs with "auto"; an operator who never heard of a signing region
	// should not have to discover it.
	cfg, err := load(envFrom(objectEnvironment()))
	if err != nil {
		t.Fatalf("load(): %v", err)
	}
	if got := cfg.ObjectStorage().RegionOrDefault(); got != "auto" {
		t.Errorf("region = %q, want auto", got)
	}
}

func TestLoadRefusesAPartialObjectStorage(t *testing.T) {
	// Each case drops exactly one variable and must name it in the error: a
	// missing bucket reported as "object storage is misconfigured" would leave
	// the operator to guess.
	for _, variable := range []string{
		"BOOP_R2_ENDPOINT", "BOOP_R2_BUCKET", "BOOP_R2_ACCESS_KEY_ID",
		"BOOP_R2_SECRET_ACCESS_KEY", "BOOP_R2_PUBLIC_URL",
	} {
		t.Run(variable, func(t *testing.T) {
			environment := objectEnvironment()
			delete(environment, variable)

			_, err := load(envFrom(environment))
			if err == nil {
				t.Fatalf("load() accepted a configuration without %s", variable)
			}
			if !strings.Contains(err.Error(), variable) {
				t.Errorf("error %q does not name %s", err, variable)
			}
		})
	}
}

func TestLoadRefusesAMalformedObjectStorage(t *testing.T) {
	cases := map[string]map[string]string{
		"endpoint without a scheme": {
			"BOOP_R2_ENDPOINT": "0123456789abcdef.r2.cloudflarestorage.com",
		},
		"endpoint with credentials": {
			"BOOP_R2_ENDPOINT": "https://key:secret@0123456789abcdef.r2.cloudflarestorage.com",
		},
		"endpoint that is not an S3 API address": {
			"BOOP_R2_ENDPOINT": "ftp://0123456789abcdef.r2.cloudflarestorage.com",
		},
		"public URL with a query": {
			"BOOP_R2_PUBLIC_URL": "https://uploads.example.com/?token=1",
		},
	}
	for name, override := range cases {
		t.Run(name, func(t *testing.T) {
			environment := objectEnvironment()
			for key, value := range override {
				environment[key] = value
			}

			if _, err := load(envFrom(environment)); err == nil {
				t.Fatalf("load() accepted %s", name)
			}
		})
	}
}

func TestLoadReadsProcessEnvironment(t *testing.T) {
	t.Setenv("BOOP_ADDR", "0.0.0.0:9000")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if cfg.Addr != "0.0.0.0:9000" {
		t.Errorf("Addr = %q, want 0.0.0.0:9000", cfg.Addr)
	}
}

func TestLoadOverrides(t *testing.T) {
	env := map[string]string{
		"BOOP_ADDR":           " 0.0.0.0:9000 ",
		"BOOP_DATA_DIR":       "G:/srv/boop-data",
		"BOOP_BASE_URL":       "https://blog.example.com/",
		"BOOP_SESSION_SECRET": strings.Repeat("s", 48),
		"BOOP_MASTER_KEY":     base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)),
		"BOOP_SECURE_COOKIES": "true",
		"BOOP_MAX_UPLOAD_MB":  "25",
		"BOOP_LOG_LEVEL":      "DEBUG",
	}

	cfg, err := load(envFrom(env))
	if err != nil {
		t.Fatalf("load(): %v", err)
	}
	if cfg.Addr != "0.0.0.0:9000" {
		t.Errorf("Addr = %q, want trimmed 0.0.0.0:9000", cfg.Addr)
	}
	if cfg.DataDir != "G:/srv/boop-data" {
		t.Errorf("DataDir = %q", cfg.DataDir)
	}
	if cfg.BaseURL != "https://blog.example.com" {
		t.Errorf("BaseURL = %q, want trailing slash trimmed", cfg.BaseURL)
	}
	if !cfg.SecureCookies {
		t.Error("SecureCookies = false, want true")
	}
	if cfg.MaxUploadMB != 25 {
		t.Errorf("MaxUploadMB = %d, want 25", cfg.MaxUploadMB)
	}
	if got := cfg.MaxUploadBytes(); got != 25<<20 {
		t.Errorf("MaxUploadBytes() = %d, want %d", got, 25<<20)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want lower-cased debug", cfg.LogLevel)
	}
	if got := cfg.SlogLevel(); got != slog.LevelDebug {
		t.Errorf("SlogLevel() = %v, want %v", got, slog.LevelDebug)
	}
	if cfg.SessionSecret != env["BOOP_SESSION_SECRET"] {
		t.Error("SessionSecret was not preserved")
	}
}

func TestDatabasePath(t *testing.T) {
	cfg, err := load(envFrom(map[string]string{"BOOP_DATA_DIR": filepath.Join("G:", "srv", "boop")}))
	if err != nil {
		t.Fatalf("load(): %v", err)
	}
	if want := filepath.Join("G:", "srv", "boop", "boop.db"); cfg.DatabasePath() != want {
		t.Errorf("DatabasePath() = %q, want %q", cfg.DatabasePath(), want)
	}

	defaults, err := load(envFrom(nil))
	if err != nil {
		t.Fatalf("load(): %v", err)
	}
	if want := filepath.Join(".", "data", "boop.db"); defaults.DatabasePath() != want {
		t.Errorf("DatabasePath() = %q, want %q", defaults.DatabasePath(), want)
	}
}

func TestLoadTreatsBlankValuesAsUnset(t *testing.T) {
	cfg, err := load(envFrom(map[string]string{
		"BOOP_ADDR":      "   ",
		"BOOP_LOG_LEVEL": "",
	}))
	if err != nil {
		t.Fatalf("load(): %v", err)
	}
	if cfg.Addr != "127.0.0.1:8080" {
		t.Errorf("Addr = %q, want default", cfg.Addr)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("LogLevel = %q, want default", cfg.LogLevel)
	}
}

func TestLoadRejectsInvalidValues(t *testing.T) {
	validSecret := strings.Repeat("s", 32)
	validMasterKey := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32))
	shortMasterKey := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 16))

	tests := []struct {
		name    string
		env     map[string]string
		wantKey string
	}{
		{"addr without port", map[string]string{"BOOP_ADDR": "localhost"}, "BOOP_ADDR"},
		{"addr with non numeric port", map[string]string{"BOOP_ADDR": "127.0.0.1:http"}, "BOOP_ADDR"},
		{"addr with out of range port", map[string]string{"BOOP_ADDR": "127.0.0.1:99999"}, "BOOP_ADDR"},
		{"relative base url", map[string]string{"BOOP_BASE_URL": "/blog"}, "BOOP_BASE_URL"},
		{"base url without host", map[string]string{"BOOP_BASE_URL": "https://"}, "BOOP_BASE_URL"},
		{"base url with unsupported scheme", map[string]string{"BOOP_BASE_URL": "ftp://example.com"}, "BOOP_BASE_URL"},
		{"base url with query", map[string]string{"BOOP_BASE_URL": "https://example.com/?a=1"}, "BOOP_BASE_URL"},
		{"base url with credentials", map[string]string{"BOOP_BASE_URL": "https://user:pass@example.com"}, "BOOP_BASE_URL"},
		{"base url with path", map[string]string{"BOOP_BASE_URL": "https://example.com/boop"}, "BOOP_BASE_URL"},
		{"session secret too short", map[string]string{"BOOP_SESSION_SECRET": "short"}, "BOOP_SESSION_SECRET"},
		{"master key not base64", map[string]string{"BOOP_MASTER_KEY": "!!!not-base64!!!"}, "BOOP_MASTER_KEY"},
		{"master key wrong length", map[string]string{"BOOP_MASTER_KEY": shortMasterKey}, "BOOP_MASTER_KEY"},
		{"secure cookies not a bool", map[string]string{"BOOP_SECURE_COOKIES": "yes"}, "BOOP_SECURE_COOKIES"},
		{"upload size not a number", map[string]string{"BOOP_MAX_UPLOAD_MB": "ten"}, "BOOP_MAX_UPLOAD_MB"},
		{"upload size zero", map[string]string{"BOOP_MAX_UPLOAD_MB": "0"}, "BOOP_MAX_UPLOAD_MB"},
		{"upload size negative", map[string]string{"BOOP_MAX_UPLOAD_MB": "-5"}, "BOOP_MAX_UPLOAD_MB"},
		{"upload size absurd", map[string]string{"BOOP_MAX_UPLOAD_MB": "100000"}, "BOOP_MAX_UPLOAD_MB"},
		{"unknown log level", map[string]string{"BOOP_LOG_LEVEL": "verbose"}, "BOOP_LOG_LEVEL"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := load(envFrom(tt.env))
			if err == nil {
				t.Fatalf("load() succeeded, want error mentioning %s", tt.wantKey)
			}
			if !strings.Contains(err.Error(), tt.wantKey) {
				t.Errorf("error %q does not mention %s", err, tt.wantKey)
			}
		})
	}

	// Guard the happy path values so the table above cannot drift.
	if _, err := load(envFrom(map[string]string{
		"BOOP_SESSION_SECRET": validSecret,
		"BOOP_MASTER_KEY":     validMasterKey,
	})); err != nil {
		t.Fatalf("valid secret values rejected: %v", err)
	}
}

func TestLoadAcceptsRootBaseURLs(t *testing.T) {
	tests := map[string]string{
		"no path":       "https://blog.example.com",
		"root path":     "https://blog.example.com/",
		"host and port": "http://127.0.0.1:8080",
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			cfg, err := load(envFrom(map[string]string{"BOOP_BASE_URL": raw}))
			if err != nil {
				t.Fatalf("load(%q): %v", raw, err)
			}
			want := strings.TrimSuffix(raw, "/")
			if cfg.BaseURL != want {
				t.Errorf("BaseURL = %q, want %q", cfg.BaseURL, want)
			}
		})
	}
}

// StorageSelection is the environment's answer in the shape the settings
// package uses, so the same value seeds the settings table, is reported at
// startup, and stays in charge until the site saves the category.
func TestStorageSelectionMapsTheEnvironment(t *testing.T) {
	cfg, err := load(envFrom(objectEnvironment()))
	if err != nil {
		t.Fatalf("load(): %v", err)
	}
	selection := cfg.StorageSelection()
	if !selection.Object() {
		t.Fatal("a configured bucket produced a local selection")
	}
	for name, pair := range map[string][2]string{
		"endpoint":          {selection.Endpoint, cfg.Storage.Endpoint},
		"bucket":            {selection.Bucket, cfg.Storage.Bucket},
		"public URL":        {selection.PublicURL, cfg.Storage.PublicURL},
		"access key id":     {selection.AccessKeyID, cfg.Storage.AccessKey},
		"secret access key": {selection.SecretAccessKey, cfg.Storage.SecretKey},
	} {
		if pair[0] != pair[1] {
			t.Errorf("%s = %q, want %q", name, pair[0], pair[1])
		}
	}

	// A deployment that configures nothing keeps the documented default.
	empty, err := load(envFrom(map[string]string{}))
	if err != nil {
		t.Fatalf("load(): %v", err)
	}
	selection = empty.StorageSelection()
	if selection.Object() {
		t.Errorf("StorageSelection = %+v, want the local directory", selection)
	}
	if selection.Mode != settings.StorageModeLocal {
		t.Errorf("mode = %q, want %q", selection.Mode, settings.StorageModeLocal)
	}
}

func TestSecretBoxIsNilWithoutTheMasterKey(t *testing.T) {
	// The box is optional: a nil one makes every secret feature fail closed
	// rather than storing plaintext, which is the documented behaviour of a
	// site that never set BOOP_MASTER_KEY.
	cfg, err := load(envFrom(map[string]string{}))
	if err != nil {
		t.Fatalf("load(): %v", err)
	}
	box, err := cfg.SecretBox()
	if err != nil {
		t.Fatalf("SecretBox(): %v", err)
	}
	if box != nil {
		t.Error("SecretBox() returned a box without a master key")
	}

	cfg, err = load(envFrom(map[string]string{"BOOP_MASTER_KEY": base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))}))
	if err != nil {
		t.Fatalf("load(): %v", err)
	}
	box, err = cfg.SecretBox()
	if err != nil {
		t.Fatalf("SecretBox(): %v", err)
	}
	if box == nil {
		t.Error("SecretBox() returned nil with a master key configured")
	}
}
