package config

import (
	"bytes"
	"encoding/base64"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
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
